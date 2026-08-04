package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
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

func (b *Bot) SendCard(ctx context.Context, chatID string, card []byte, replyToID string) (string, error) {
	if len(card) == 0 {
		return "", errors.New("feishu: empty card body")
	}
	b.logger.Debug("send card",
		log.FieldChatID, chatID,
		"reply_to", replyToID,
		"card", strutil.DebugRedact(string(card), b.logDebugRedact.Load()))
	res, err := b.client.Send(ctx, &lark.SendInput{
		ChatID:         chatID,
		Card:           string(card),
		ReplyMessageID: replyToID,
	})
	if err != nil {
		if isCardContentRejected(err) {
			// Surface a detectable error so a caller with the original text
			// (sendResult) can fall back to plain text. We no longer auto-send
			// a fixed stub here: that lost the reply for table/element
			// rejections where the text itself fits fine.
			b.logger.Info("card content rejected by server",
				log.FieldChatID, chatID,
				"card_size_bytes", len(card))
			return "", fmt.Errorf("%w: %w", ErrCardContentRejected, err)
		}
		return "", fmt.Errorf("feishu: send card: %w", err)
	}
	b.markHealthy() // outbound success refreshes the watchdog: without this, a long conversation with no inbound WS traffic trips fatal_after=5m
	if res == nil {
		return "", errors.New("feishu: send card returned no result")
	}
	return res.MessageID, nil
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

// UpdateCard updates an existing card message with new content.
// This is useful for dynamic status updates, progress displays, and feedback scenarios.
func (b *Bot) UpdateCard(ctx context.Context, messageID string, card []byte) error {
	if len(card) == 0 {
		return errors.New("feishu: empty card body")
	}
	if messageID == "" {
		return errors.New("feishu: message_id required")
	}
	if b.client == nil {
		return errors.New("feishu: client not initialized")
	}

	b.logger.Debug("update feishu card",
		log.FieldMessageID, messageID,
		"card_type", "interactive",
		"card_size_bytes", len(card),
		"card_preview", strutil.DebugRedact(strutil.Truncate(string(card), 300), b.logDebugRedact.Load()))

	// Send update request with bounded retry on transient (network) errors.
	// Content-too-large is detected on the error and short-circuits to the
	// fallback; business codes after a successful HTTP round-trip are not
	// retried (retrying a content/permission rejection cannot help).
	backoff := cardRetryBase
	for attempt := 0; ; attempt++ {
		err := b.client.PatchMessage(ctx, messageID, string(card))
		if err == nil {
			break
		}
		if isCardContentRejected(err) {
			b.logger.Info("card content rejected, falling back to minimal card",
				log.FieldMessageID, messageID,
				"card_size_bytes", len(card))
			return b.updateFallbackCard(ctx, messageID)
		}
		if IsCardGone(err) {
			// Card was withdrawn/deleted client-side: PATCH can never succeed,
			// so don't burn the retry budget. Surface the raw error (carrying
			// code:230011 via %w) so the caller (status-monitor broadcast) can
			// detect it with IsCardGone and re-send a fresh card.
			return fmt.Errorf("feishu: update card (message gone): %w", err)
		}
		if attempt >= cardRetry {
			return fmt.Errorf("feishu: update card request failed after %d retries: %w", attempt, err)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return fmt.Errorf("feishu: update card request failed: %w", err)
		}
		backoff *= 2
	}

	b.markHealthy() // outbound success refreshes the watchdog
	b.logger.Info("card update completed", log.FieldMessageID, messageID)
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

// UpdateCardVerified PATCHes the card, then read-back verifies the header
// template colour persisted, re-PATCHing up to cardVerifyMaxAttempts times if
// Feishu silently reverted it. Guards the three delayed-PATCH sites (picker
// outcome, /send refresh, submitted fallback) where the card.action.trigger
// click-handling window can roll a PATCH back even after cardPatchDelay.
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
func (b *Bot) UpdateCardVerified(ctx context.Context, messageID string, card []byte) error {
	if len(card) == 0 {
		return errors.New("feishu: empty card body")
	}
	want := extractHeaderTemplate(card)
	vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cardVerifyTimeout)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < cardVerifyMaxAttempts; attempt++ {
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
		if err := b.UpdateCard(vctx, messageID, card); err != nil {
			lastErr = err
			if IsCardGone(err) {
				return err // withdrawn: re-PATCH can never succeed
			}
			continue // transient/network — retry the PATCH
		}
		if want == "" {
			return nil // headerless card: nothing to verify, trust the PATCH
		}
		got, err := b.client.GetMessage(vctx, messageID)
		if err != nil {
			lastErr = err
			continue // cannot confirm — retry (loop cap bounds thrash)
		}
		if extractHeaderTemplate(got) == want {
			return nil // colour persisted
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
// PATCHed: code:230011 ("The message was withdrawn." — the client deleted/
// withdrew it) or code:99992354 (message_id invalid/non-existent, defensive).
// Verified against the live Feishu API (2026-07-27): PATCHing a withdrawn
// message returns exactly code:230011. The status-monitor broadcast path
// treats either as "drop the cached messageID and SendCard a new one".
// Exported because the dispatcher (package feishufront) inspects the error
// returned by CardSink.UpdateCard across the package boundary.
func IsCardGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "code:230011") || strings.Contains(s, "code:99992354")
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
