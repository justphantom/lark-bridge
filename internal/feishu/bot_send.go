package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

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
// message (callbacks, replies); CardID is the CardKit entity id the caller
// must hand back to UpdateCard for updates.
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

// cardStateFor returns the per-entity sequence handle for cardID, creating
// it on first use. Keyed by cardID: the entity id is the update handle
// (cardkit-migration §3.3, plan B).
func (b *Bot) cardStateFor(cardID string) *cardState {
	b.cardsMu.Lock()
	defer b.cardsMu.Unlock()
	if b.cards == nil {
		b.cards = make(map[string]*cardState)
	}
	st := b.cards[cardID]
	if st == nil {
		st = &cardState{}
		b.cards[cardID] = st
	}
	return st
}

// SendCard sends a card and returns both its messageID and its CardKit entity
// id as a CardRef. The send is two-phase: create the entity (POST
// cardkit/v1/cards) then ship it by reference
// (content={"type":"card","data":{"card_id":...}}). A failure after a
// successful create leaks the entity id; it expires after 14 days and is
// logged, per the migration assessment §3.2.
func (b *Bot) SendCard(ctx context.Context, chatID string, card []byte, replyToID string) (CardRef, error) {
	if len(card) == 0 {
		return CardRef{}, errors.New("feishu: empty card body")
	}
	cardID, err := b.client.CreateCardEntity(ctx, string(card))
	if err != nil {
		if isCardContentRejected(err) {
			b.logger.Info("card entity content rejected by server",
				log.FieldChatID, chatID,
				"card_size_bytes", len(card))
			return CardRef{}, fmt.Errorf("%w: %w", ErrCardContentRejected, err)
		}
		return CardRef{}, fmt.Errorf("feishu: create card entity: %w", err)
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

// SendCardInline sends a card as inline JSON (not a CardKit entity reference)
// so the card's button callbacks (card.action.trigger) fire on click. The
// trade-off: inline cards can only be updated via im PATCH (not CardKit PUT),
// which is the legacy path — but interactive cards (permission/question/
// backend-picker) are short-lived and updated at most once after a click, so
// the PATCH path is sufficient.
//
// Returns a CardRef with CardID="" so callers know to update via PATCH (im
// PATCH keyed by MessageID), not CardKit PUT.
func (b *Bot) SendCardInline(ctx context.Context, chatID string, card []byte, replyToID string) (CardRef, error) {
	if len(card) == 0 {
		return CardRef{}, errors.New("feishu: empty card body")
	}
	return b.sendCardPayload(ctx, chatID, card, "", replyToID)
}

// sendCardPayload ships the card through im/v1/messages: the entity reference
// envelope (content={"type":"card","data":{"card_id":...}}). Shared by
// SendCard so the rejection/watchdog handling lives in exactly one place.
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

// UpdateCard replaces a card body via the CardKit entity PUT. cardID (from the
// CardRef SendCard returned) is the update handle; the PUT carries the next
// strictly-increasing per-card sequence. When the server rejects the content
// (oversized body, too many elements) the entity is overwritten with a
// minimal fallback card so the frame is not lost. An entity that vanished
// server-side (deleted or past the 14-day TTL) surfaces as IsCardGone so
// callers fall back to a fresh card.
func (b *Bot) UpdateCard(ctx context.Context, messageID, cardID string, card []byte) error {
	if b.client == nil {
		return errors.New("feishu: client not initialized")
	}
	if len(card) == 0 {
		return errors.New("feishu: empty card body")
	}
	if cardID == "" {
		// No entity id → this is an inline card (SendCardInline). Update via
		// im PATCH (the only path for non-entity cards). messageID is required.
		if messageID == "" {
			return errors.New("feishu: UpdateCard needs card_id or message_id")
		}
		err := b.client.PatchMessage(ctx, messageID, string(card))
		if err != nil {
			b.logger.Warn("UpdateCard PatchMessage failed",
				"message_id", messageID,
				"card_size", len(card),
				"error", err)
		}
		return err
	}
	return b.updateCardEntity(ctx, cardID, card)
}

// updateCardEntity is the CardKit update path: full-replacement PUT on the
// card entity with the next strictly-increasing sequence for this card and a
// fresh idempotency uuid. The sequence is allocated under the per-card
// cardState mutex so concurrent updaters cannot interleave equal values (the
// platform rejects non-increasing sequences with 300317, which is exactly
// what orders racing progress frames). An entity that vanished server-side
// (200740/200750 — deleted or past the 14-day TTL) surfaces as the
// IsCardGone sentinel so callers fall back to a fresh card.
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
		// sentinel so the dispatcher falls back to a fresh card.
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
		return fmt.Errorf("feishu: update card fallback after rejection: %w (original: %s)", ferr, err.Error())
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
// updated. CardKit codes (feishu-cardkit-migration-assessment.md §3.3):
// 200740 (实体不存在) and 200750 (卡片实体超过 14 天有效期) — both mean the
// entity is permanently gone, mapped to the "drop the cached id and SendCard
// a fresh one" downgrade path. Exported because the dispatcher (package
// feishufront) inspects the error returned by CardSink.UpdateCard across the
// package boundary.
func IsCardGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, code := range []string{"code:200740", "code:200750"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return false
}

// fallbackText is the plain-text body sent when a card is rejected for being
// too large. It is deliberately short so it can never trip the size limit.
const fallbackText = "⚠️ 消息内容过长，卡片已折叠。请缩短内容后重试。"

// fallbackCardJSON returns a minimal schema-2.0 card whose single markdown
// element carries fallbackText. Used when updating an existing card entity
// whose original content was too large.
func fallbackCardJSON() []byte {
	// Built via json.Marshal rather than string concatenation so fallbackText
	// is properly escaped if it ever grows to contain a quote or backslash
	// (string concat would produce invalid JSON). Schema 2.0 + body.elements
	// matches cardkit.Card's layout.
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
		Body struct {
			Elements []struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}{
		Schema: "2.0",
	}
	card.Config.UpdateMulti = true
	card.Header.Title.Tag = "plain_text"
	card.Header.Title.Content = "消息过长"
	card.Header.Template = "grey"
	card.Body.Elements = []struct {
		Tag     string `json:"tag"`
		Content string `json:"content"`
	}{{Tag: "markdown", Content: fallbackText}}
	b, _ := json.Marshal(card)
	return b
}
