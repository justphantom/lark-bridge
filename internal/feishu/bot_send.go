package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// MaxTextBodyBytes is the maximum byte length of raw text sent in a single
// plain-text message. The Feishu API caps request bodies at 30 KB for
// post/card types; this value leaves headroom for JSON envelope overhead
// and escape expansion. Exported so the dispatcher can pre-truncate a text
// fallback (e.g. when a result card is rejected for too many tables).
const MaxTextBodyBytes = 25000

// cardRetry is the max number of extra UpdateCard attempts after a transient
// failure. UpdateCard is idempotent for a given messageID, so a retry never
// produces a duplicate card; SendCard is NOT retried (it would double-post).
// Only network/transport errors retry — business codes (content too large,
// permission) return immediately since retrying cannot help.
const cardRetry = 3

// cardRetryBase is the initial backoff between UpdateCard retries; each retry
// doubles it. Kept small so a stalled update does not wedge the dispatcher.
const cardRetryBase = 300 * time.Millisecond

// feishuCodeContentTooLarge is the Feishu API error code for "message
// content reaches its limit" (body byte size). The REST client surfaces this
// as *lark.APIError whose Error() contains "code:230025"; isCardContentRejected
// matches on that substring.
const feishuCodeContentTooLarge = 230025

// feishuCodeCardElementOverLimit is the Feishu error for a card exceeding an
// element-count cap (tables/columns/images/…). Surfaced inside the generic
// 230099 "Failed to create card content"; the body itself is under the byte
// limit, so this is distinct from feishuCodeContentTooLarge.
const feishuCodeCardElementOverLimit = 11310

// ErrCardContentRejected is returned by SendCard when Feishu rejects the card
// CONTENT — body too large (230025), too many tables/elements (11310), or any
// sibling "over limit" rejection. The card itself was syntactically valid; a
// caller that has the underlying text (e.g. the result-card reply) can
// errors.Is this and fall back to SendText so the reply is not lost.
var ErrCardContentRejected = errors.New("feishu: card content rejected by server")

// CardRef is the returned identity of a sent card. MessageID locates the
// message (callbacks, replies, the legacy PATCH update path); CardID is the
// CardKit entity id the caller must hand back to UpdateCard for updates —
// empty when the bot runs in legacy (schema 1.0 im PATCH) mode, in which
// case updates address the card by MessageID instead.
type CardRef struct {
	MessageID string
	CardID    string
}

// cardState tracks the CardKit update sequence of one sent card entity.
// The CardKit PUT endpoint rejects a non-increasing sequence (code 300317),
// so the counter must be strictly monotonic per card. Guards its own fields.
type cardState struct {
	mu  sync.Mutex
	seq int64
}

// SetCardEngine switches the card send/update pipeline. With cardkit=true
// every new SendCard creates a CardKit card entity (schema 2.0) and ships it
// by reference; the returned CardRef.CardID is then the update handle the
// caller passes to UpdateCard. Cards sent in legacy mode have no CardID and
// keep the im PATCH path, so a mid-migration mix of old and new cards works.
// Called once at startup from config; not goroutine-safe by design.
func (b *Bot) SetCardEngine(cardkit bool) {
	b.cardkit = cardkit
}

// cardStateFor returns the per-entity sequence handle for cardID, creating
// it on first use. Keyed by cardID (not messageID): the entity id is the
// update handle in cardkit mode (cardkit-migration §3.3, plan B).
func (b *Bot) cardStateFor(cardID string) *cardState {
	b.cardsMu.Lock()
	defer b.cardsMu.Unlock()
	st := b.cards[cardID]
	if st == nil {
		st = &cardState{}
		b.cards[cardID] = st
	}
	return st
}

// SendCard sends a card and returns both its messageID and (in cardkit mode)
// its CardKit entity id as a CardRef. In cardkit mode the send is two-phase:
// create the entity (POST cardkit/v1/cards) then ship it by reference
// (content={"type":"card","data":{"card_id":...}}). A failure after a
// successful create leaks the entity id; it expires after 14 days and is
// logged, per the migration assessment §3.2. Legacy mode returns only
// MessageID (CardID "") and the update path stays im PATCH.
func (b *Bot) SendCard(ctx context.Context, chatID string, card []byte, replyToID string) (CardRef, error) {
	if len(card) == 0 {
		return CardRef{}, errors.New("feishu: empty card body")
	}
	cardID := ""
	if b.cardkit {
		var err error
		cardID, err = b.client.CreateCardEntity(ctx, string(card))
		if err != nil {
			if isCardContentRejected(err) {
				b.logger.Info("card entity content rejected by server",
					log.FieldChatID, chatID,
					"card_size_bytes", len(card))
				return CardRef{}, fmt.Errorf("%w: %w", ErrCardContentRejected, err)
			}
			return CardRef{}, fmt.Errorf("feishu: create card entity: %w", err)
		}
	}
	ref, err := b.sendCardPayload(ctx, chatID, card, cardID, replyToID)
	if err != nil {
		if cardID != "" {
			// Entity created but the message send failed: the entity leaks
			// until its 14-day TTL. Acceptable per the migration assessment;
			// logged so leaks are observable.
			b.logger.Warn("card entity orphaned after send failure",
				log.FieldChatID, chatID, "card_id", cardID, log.FieldError, err.Error())
		}
		return CardRef{}, err
	}
	return ref, nil
}

// sendCardPayload ships the card through im/v1/messages: legacy mode passes
// the raw card JSON (msg_type=interactive); cardkit mode passes the entity
// reference envelope. Shared by both engines so the rejection/watchdog
// handling lives in exactly one place.
func (b *Bot) sendCardPayload(ctx context.Context, chatID string, card []byte, cardID, replyToID string) (CardRef, error) {
	b.logger.Debug("send card",
		log.FieldChatID, chatID,
		"reply_to", replyToID,
		"card_id", cardID,
		"card", strutil.DebugRedact(string(card), b.logDebugRedact.Load()))
	in := &lark.SendInput{ChatID: chatID, ReplyMessageID: replyToID}
	if cardID != "" {
		in.CardID = cardID
	} else {
		in.Card = string(card)
	}
	res, err := b.client.Send(ctx, in)
	if err != nil {
		if isCardContentRejected(err) {
			// Surface a detectable error so a caller with the original text
			// (sendResult) can fall back to plain text. We no longer auto-send
			// a fixed stub here: that lost the reply for table/element
			// rejections where the text itself fits fine.
			b.logger.Info("card content rejected by server",
				log.FieldChatID, chatID,
				"card_size_bytes", len(card))
			return CardRef{}, fmt.Errorf("%w: %w", ErrCardContentRejected, err)
		}
		return CardRef{}, fmt.Errorf("feishu: send card: %w", err)
	}
	b.markHealthy() // outbound success refreshes the watchdog: without this, a long conversation with no inbound WS traffic trips fatal_after=5m
	if res == nil {
		return CardRef{}, errors.New("feishu: send card returned no result")
	}
	return CardRef{MessageID: res.MessageID, CardID: cardID}, nil
}

// SendText sends a plain-text (msgType=text) message. Used as the fallback
// when SendCard rejects a result card's content (e.g. a reply with too many
// markdown tables) — the reply text is delivered as plain text instead of
// being lost entirely. text is expected to be pre-truncated by the caller to
// fit the text-message size limit.
func (b *Bot) SendText(ctx context.Context, chatID, text, replyToID string) (string, error) {
	if text == "" {
		return "", errors.New("feishu: empty text body")
	}
	res, err := b.client.Send(ctx, &lark.SendInput{
		ChatID:         chatID,
		Text:           text,
		ReplyMessageID: replyToID,
	})
	if err != nil {
		return "", fmt.Errorf("feishu: send text: %w", err)
	}
	b.markHealthy()
	if res == nil {
		return "", errors.New("feishu: send text returned no result")
	}
	return res.MessageID, nil
}

// SendFile uploads a binary to Feishu and sends it as a file message to
// chatID (send-file-design.md §3.4). Used by the frontend's handleFileControl
// to materialise a TypeFile control a backend emitted. The upload (file_key)
// and the send (msg_type=file) are two REST round-trips; both must succeed.
// fileName is the display name the recipient sees; r carries the raw bytes.
func (b *Bot) SendFile(ctx context.Context, chatID, fileName string, r io.Reader) error {
	if fileName == "" {
		return errors.New("feishu: empty file name")
	}
	if r == nil {
		return errors.New("feishu: nil file reader")
	}
	fileKey, err := b.client.UploadFile(ctx, fileName, "stream", r)
	if err != nil {
		return fmt.Errorf("feishu: upload file: %w", err)
	}
	if _, err := b.client.Send(ctx, &lark.SendInput{ChatID: chatID, FileKey: fileKey}); err != nil {
		return fmt.Errorf("feishu: send file message: %w", err)
	}
	b.markHealthy() // outbound success refreshes the watchdog
	b.logger.Debug("file sent", log.FieldChatID, chatID, "file_name", fileName)
	return nil
}

// UpdateCard replaces a card body. The update handle comes from the
// CardRef SendCard returned: cardID != "" targets the CardKit entity via
// PUT with a strictly-increasing per-card sequence (cardkit engine);
// cardID == "" is the legacy path and addresses the card by messageID via
// im PATCH. The content-rejected fallback behaves the same on both paths:
// swap the body to the minimal fallback card and retry once, so a malformed
// reply surfaces as a card rather than a dispatcher error.
func (b *Bot) UpdateCard(ctx context.Context, messageID, cardID string, card []byte) error {
	if b.client == nil {
		return errors.New("feishu: client not initialized")
	}
	if len(card) == 0 {
		return errors.New("feishu: empty card body")
	}
	if cardID != "" {
		return b.updateCardEntity(ctx, cardID, card)
	}
	if messageID == "" {
		return errors.New("feishu: empty messageID")
	}
	return b.updateCardLegacy(ctx, messageID, card)
}

// updateCardLegacy is the pre-CardKit update path: im PATCH on messageID,
// with the content-rejected fallback retry. Serves the legacy engine and
// cardkit-engine cards that predate the process (no registered entity).
func (b *Bot) updateCardLegacy(ctx context.Context, messageID string, card []byte) error {
	err := b.client.PatchMessage(ctx, messageID, string(card))
	if err == nil {
		b.markHealthy()
		return nil
	}
	if !isCardContentRejected(err) {
		return fmt.Errorf("feishu: update card: %w", err)
	}
	if uerr := b.updateFallbackCard(ctx, messageID); uerr != nil {
		return fmt.Errorf("feishu: update card fallback after rejection (%v): %w", err, uerr)
	}
	return nil
}

// updateCardEntity is the CardKit update path: full-replacement PUT on the
// card entity with the next strictly-increasing sequence for this card and a
// fresh idempotency uuid. The sequence is allocated under the per-card
// cardState mutex so concurrent updaters cannot interleave equal values (the
// platform rejects non-increasing sequences with 300317, which is exactly
// what orders racing progress frames). An entity that vanished server-side
// (200740/200750 — deleted or past the 14-day TTL) surfaces as the
// IsCardGone sentinel so callers fall back to a fresh card, matching the
// withdrawn-card path.
func (b *Bot) updateCardEntity(ctx context.Context, cardID string, card []byte) error {
	st := b.cardStateFor(cardID)
	st.mu.Lock()
	st.seq++
	seq := st.seq
	st.mu.Unlock()
	uuid := strings.Join([]string{"upd", cardID, strconv.FormatInt(seq, 10)}, "-")
	err := b.client.UpdateCardEntity(ctx, cardID, string(card), seq, uuid)
	if err == nil {
		b.markHealthy()
		return nil
	}
	if IsCardGone(err) {
		// The entity is gone for good: drop the sequence state so a later
		// update does not keep paying a doomed PUT, and hand the caller the
		// same sentinel the legacy path yields for a withdrawn card.
		b.cardsMu.Lock()
		delete(b.cards, cardID)
		b.cardsMu.Unlock()
		return fmt.Errorf("feishu: update card: %w", err)
	}
	if !isCardContentRejected(err) {
		return fmt.Errorf("feishu: update card: %w", err)
	}
	// Content rejected (oversized body, too many tables): overwrite the
	// entity with the minimal fallback card instead of losing the frame.
	st.mu.Lock()
	st.seq++
	fseq := st.seq
	st.mu.Unlock()
	fuuid := strings.Join([]string{"upd", cardID, strconv.FormatInt(fseq, 10)}, "-")
	if ferr := b.client.UpdateCardEntity(ctx, cardID, string(fallbackCardJSON()), fseq, fuuid); ferr != nil {
		return fmt.Errorf("feishu: update card fallback after rejection (%v): %w", err, ferr)
	}
	b.markHealthy()
	return nil
}

// cardVerify loop tunables. var (not const) so tests can shrink the backoff
// to keep the PATCH→read-back→retry loop instant; production keeps the
// defaults below.
var (
	// cardVerifyMaxAttempts: attempt 0 is the real PATCH; later ones re-PATCH
	// after Feishu silently reverted the colour. Three is enough — if the
	// platform's persistence layer has not settled after two re-PATCHes the
	// card is effectively ungovernable, so stop rather than hammer the API.
	cardVerifyMaxAttempts = 3
	// cardVerifyBackoff is the sleep between a failed verification (colour
	// reverted or GET failed) and the next re-PATCH. Kept short: the first
	// PATCH already slept past the ~3-5s click window, so a revert here is the
	// platform's persistence layer, which usually settles within a second or two.
	cardVerifyBackoff = 2 * time.Second
	// cardVerifyTimeout bounds the whole PATCH→GET→retry loop so a stalled
	// Feishu API cannot keep the background goroutine alive forever. Detached
	// from the caller's ctx via WithoutCancel because these run in fire-and-
	// forget goroutines whose request ctx dies when the click handler returns.
	cardVerifyTimeout = 30 * time.Second
)

// ErrCardVerifyMismatch signals that a PATCH shipped but did not persist after
// cardVerifyMaxAttempts read-back checks — Feishu kept reverting the header
// colour. Surfaced (not silent) so logs can tell "bounced" from "stuck".
var ErrCardVerifyMismatch = errors.New("feishu: card update did not persist after verification")

// UpdateCardVerified updates the card (im PATCH by messageID, or CardKit PUT
// by cardID when cardID != ""), then read-back verifies the header template
// colour persisted, re-issuing up to cardVerifyMaxAttempts times if Feishu
// silently reverted it. Guards the three delayed-update sites (picker
// outcome, /send refresh, submitted fallback) where the card.action.trigger
// click-handling window can roll an update back even after cardPatchDelay.
//
// Verification uses a header.template colour fingerprint, NOT a full content
// compare: Feishu normalizes the stored content JSON (reorders keys, injects
// defaults), so byte/key equality would always mismatch and thrash into a
// re-PATCH loop. The template colour is exactly the user-visible signal of a
// bounce-back ("turned green then reverted to the old card"), so it is both
// sufficient and robust.
//
// Degrades gracefully: a headerless card (want=="") trusts the PATCH without a
// read-back; a card withdrawn mid-loop (IsCardGone) returns immediately since
// no re-PATCH can succeed; a GET failure (missing im:message:read scope,
// timeout) retries once then surfaces the last error rather than looping
// blindly. The whole loop runs under a self-managed cardVerifyTimeout.
func (b *Bot) UpdateCardVerified(ctx context.Context, messageID, cardID string, card []byte) error {
	if len(card) == 0 {
		return errors.New("feishu: empty card body")
	}
	want := elementsFingerprint(card)
	vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cardVerifyTimeout)
	defer cancel()

	var lastErr error
	for attempt := range cardVerifyMaxAttempts {
		if attempt > 0 {
			select {
			case <-time.After(cardVerifyBackoff):
			case <-vctx.Done():
				if lastErr == nil {
					lastErr = vctx.Err()
				}
				return lastErr
			}
		}
		if err := b.UpdateCard(vctx, messageID, cardID, card); err != nil {
			lastErr = err
			if IsCardGone(err) {
				return err // withdrawn: re-PATCH can never succeed
			}
			continue // transient/network — retry the PATCH
		}
		if want == "" {
			return nil // no elements to verify, trust the PATCH
		}
		got, err := b.client.GetMessage(vctx, messageID)
		if err != nil {
			lastErr = err
			continue // cannot confirm — retry (loop cap bounds thrash)
		}
		if elementsFingerprint(got) == want {
			return nil // content persisted
		}
		lastErr = ErrCardVerifyMismatch // reverted — loop re-PATCHes
	}
	return lastErr
}

// extractHeaderTemplate pulls the header.template colour out of a card JSON
// blob. Handles both the schema 1.0 layout we send (top-level
// {header:{template}}) and the schema 2.0 wrapper Feishu may return on
// read-back ({data:{template:{header:{template}}}}), so a compare never
// reports a phantom mismatch just because the envelope differs. Returns "" for
// a headerless card or an unparseable blob — callers treat "" as "no
// fingerprint to check, trust the PATCH".
// jsonKeys returns the sorted top-level keys of a parsed JSON object — a
// structure probe that never exposes content values (low-18). Diagnostic only.
func jsonKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// elementsFingerprint canonicalises a card's elements array for comparison.
// Feishu's read-back strips header/config/schema but keeps elements verbatim,
// so the elements are the only reliable persistence fingerprint. Parse +
// re-marshal normalises key order (Go map marshal sorts keys), and numbers are
// compared via their json re-encoding so 1 vs 1.0 do not false-mismatch.
// Returns "" for an unparseable blob or a card without elements — callers
// treat "" as "no fingerprint to check, trust the PATCH".
func elementsFingerprint(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return ""
	}
	// v1 keeps elements at the top level; v2 nests them under body.elements.
	elems, ok := root["elements"]
	if !ok {
		if body, _ := root["body"].(map[string]any); body != nil {
			elems, ok = body["elements"]
		}
	}
	if !ok || elems == nil {
		return ""
	}
	list, _ := elems.([]any)
	// Feishu may reorder both object keys AND the elements array on read-back,
	// so the fingerprint must be order-insensitive at both levels: canonicalize
	// each element (map keys sort on marshal), then sort the per-element strings
	// before joining. Two cards with the same element SET fingerprint equal even
	// when Feishu shuffles their order.
	parts := make([]string, 0, len(list))
	for _, el := range list {
		c, err := json.Marshal(el)
		if err != nil {
			return ""
		}
		parts = append(parts, string(c))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

func extractHeaderTemplate(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var v1 struct {
		Header struct {
			Template string `json:"template"`
		} `json:"header"`
	}
	if err := json.Unmarshal(b, &v1); err == nil && v1.Header.Template != "" {
		return v1.Header.Template
	}
	var v2 struct {
		Data struct {
			Template struct {
				Header struct {
					Template string `json:"template"`
				} `json:"header"`
			} `json:"template"`
		} `json:"data"`
	}
	_ = json.Unmarshal(b, &v2)
	return v2.Data.Template.Header.Template
}

// updateFallbackCard re-patches messageID with a minimal card after the
// original content was rejected as too large (230025).
func (b *Bot) updateFallbackCard(ctx context.Context, messageID string) error {
	if err := b.client.PatchMessage(ctx, messageID, string(fallbackCardJSON())); err != nil {
		return fmt.Errorf("feishu: update card request failed: %w", err)
	}
	b.markHealthy()
	return nil
}

// isCardContentRejected reports whether err represents a Feishu CONTENT
// rejection: body too large (230025), too many tables/elements (11310), or
// any sibling "over limit" rejection. The REST client surfaces these as
// *lark.APIError whose Error() contains "code:<N>"; we identify by substring
// (the inner code + the English "over limit" phrase Feishu uses for element-
// count caps) so both direct APIError codes and any wrapped variants match.
func isCardContentRejected(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "code:"+strconv.Itoa(feishuCodeContentTooLarge)) ||
		strings.Contains(s, "code:"+strconv.Itoa(feishuCodeCardElementOverLimit)) ||
		strings.Contains(s, "over limit")
}

// IsCardGone reports whether err represents a card that can no longer be
// updated. Legacy im PATCH codes: 230011 ("The message was withdrawn.") and
// 99992354 (message_id invalid/non-existent, defensive). CardKit codes
// (feishu-cardkit-migration-assessment.md §3.3): 200740 (实体不存在) and
// 200750 (卡片实体超过 14 天有效期) — both mean the entity is permanently
// gone, mapped to the same "drop the cached id and SendCard a fresh one"
// downgrade path. Verified against the live Feishu API (2026-07-27): PATCHing
// a withdrawn message returns exactly code:230011. Exported because the
// dispatcher (package feishufront) inspects the error returned by
// CardSink.UpdateCard across the package boundary.
func IsCardGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, code := range []string{"code:230011", "code:99992354", "code:200740", "code:200750"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return false
}

// fallbackText is the plain-text body sent when a card is rejected for being
// too large. It is deliberately short so it can never trip the size limit.
const fallbackText = "⚠️ 消息内容过长，卡片已折叠。请缩短内容后重试。"

// fallbackCardJSON returns a minimal interactive card whose single markdown
// element carries fallbackText. Used when patching an existing card message
// whose original content was too large.
func fallbackCardJSON() []byte {
	// Built via json.Marshal rather than string concatenation so fallbackText
	// is properly escaped if it ever grows to contain a quote or backslash
	// (string concat would produce invalid JSON and break the Patch).
	// Schema 1.0 + top-level elements matches cardkit.Card's layout so a
	// fallback patch is the same shape as the original card.
	card := struct {
		Schema string `json:"schema"`
		Config struct {
			UpdateMulti bool `json:"update_multi"`
		} `json:"config"`
		Header struct {
			Title struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"title"`
			Template string `json:"template"`
		} `json:"header"`
		Elements []struct {
			Tag     string `json:"tag"`
			Content string `json:"content"`
		} `json:"elements"`
	}{
		Schema: "1.0",
	}
	card.Config.UpdateMulti = true
	card.Header.Title.Tag = "plain_text"
	card.Header.Title.Content = "消息过长"
	card.Header.Template = "grey"
	card.Elements = []struct {
		Tag     string `json:"tag"`
		Content string `json:"content"`
	}{{Tag: "markdown", Content: fallbackText}}
	b, _ := json.Marshal(card)
	return b
}
