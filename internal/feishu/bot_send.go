package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sdktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// MaxTextBodyBytes is the maximum byte length of raw text sent in a single
// plain-text message. The Feishu API caps request bodies at 30 KB for
// post/card types; this value leaves headroom for JSON envelope overhead
// and escape expansion. Exported so the dispatcher can pre-truncate a text
// fallback (e.g. when a result card is rejected for too many tables).
const MaxTextBodyBytes = 25000

// maxContentSize aliases MaxTextBodyBytes for the in-package call sites
// (bot.go's text-chunk limit) that pre-date the export.
const maxContentSize = MaxTextBodyBytes

// cardRetry is the max number of extra UpdateCard attempts after a transient
// failure. UpdateCard is idempotent for a given messageID, so a retry never
// produces a duplicate card; SendCard is NOT retried (it would double-post).
// Only network/SDK-layer errors retry — business codes (content too large,
// permission) return immediately since retrying cannot help.
const cardRetry = 3

// cardRetryBase is the initial backoff between UpdateCard retries; each retry
// doubles it. Kept small so a stalled update does not wedge the dispatcher.
const cardRetryBase = 300 * time.Millisecond

// feishuCodeContentTooLarge is the Feishu API error code for "message
// content reaches its limit" (body byte size). Used for direct resp.Code
// comparison on Patch responses; channel errors embed it as the substring
// "code:"+strconv.Itoa(feishuCodeContentTooLarge) (the SDK classifies
// 230025 as an unknown code, so detection is by that substring).
const feishuCodeContentTooLarge = 230025

// feishuCodeCardElementOverLimit is the Feishu error for a card exceeding an
// element-count cap (tables/columns/images/…). Observed as
// "card table number over limit" wrapping code 11310 inside the generic
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
	res, err := (*b.ch.Load()).Send(ctx, &sdktypes.SendInput{
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
		// The SDK returned success but no result. Treat as an error so the
		// caller does not silently proceed with an empty messageID (which would
		// leave every later UpdateCard targeting "" and the turn stuck).
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
	res, err := (*b.ch.Load()).Send(ctx, &sdktypes.SendInput{
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

// UpdateCard updates an existing card message with new content.
// This is useful for dynamic status updates, progress displays, and feedback scenarios.
func (b *Bot) UpdateCard(ctx context.Context, messageID string, card []byte) error {
	if len(card) == 0 {
		return errors.New("feishu: empty card body")
	}
	if messageID == "" {
		return errors.New("feishu: message_id required")
	}
	if b.imService == nil {
		return errors.New("feishu: im service not initialized")
	}

	b.logger.Debug("update feishu card",
		log.FieldMessageID, messageID,
		"card_type", "interactive",
		"card_size_bytes", len(card),
		"card_preview", strutil.DebugRedact(strutil.Truncate(string(card), 300), b.logDebugRedact.Load()))

	// Build update request
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(string(card)).
			Build()).
		Build()

	// Send update request with bounded retry on transient (network/SDK) errors.
	// Content-too-large is detected on the error string and short-circuits to
	// the fallback; business codes after a successful HTTP round-trip are not
	// retried (retrying a content/permission rejection cannot help).
	var resp *larkim.PatchMessageResp
	var err error
	backoff := cardRetryBase
	for attempt := 0; ; attempt++ {
		resp, err = b.imService.Message.Patch(ctx, req)
		if err == nil {
			break
		}
		if isCardContentRejected(err) {
			// Same fallback as the resp.Code path below: a minimal card beats
			// an erroring one. Not retried (retrying cannot shrink the content).
			b.logger.Info("card content rejected, falling back to minimal card",
				log.FieldMessageID, messageID,
				"card_size_bytes", len(card))
			return b.updateFallbackCard(ctx, messageID)
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

	// Check response status
	if resp == nil || !resp.Success() {
		code := -1
		msg := "unknown error"
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		// Content-limit rejection (230025 body too large, or an "over limit"
		// element-count code surfaced only in msg for Patch responses). Re-patch
		// with a minimal card so the existing message shows a readable
		// placeholder instead of erroring.
		if code == feishuCodeContentTooLarge || strings.Contains(msg, "over limit") {
			b.logger.Info("card content rejected, falling back to minimal card",
				log.FieldMessageID, messageID,
				"card_size_bytes", len(card))
			return b.updateFallbackCard(ctx, messageID)
		}
		return fmt.Errorf("feishu: update card failed: code=%d, msg=%s", code, msg)
	}

	b.markHealthy() // outbound success refreshes the watchdog

	b.logger.Info("card update completed",
		log.FieldMessageID, messageID,
		"response_code", resp.Code)

	return nil
}

// updateFallbackCard re-patches messageID with a minimal card after the
// original content was rejected as too large (230025).
func (b *Bot) updateFallbackCard(ctx context.Context, messageID string) error {
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(string(fallbackCardJSON())).
			Build()).
		Build()
	resp, err := b.imService.Message.Patch(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu: update card request failed: %w", err)
	}
	if resp == nil || !resp.Success() {
		code, msg := -1, "unknown error"
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		return fmt.Errorf("feishu: update card failed: code=%d, msg=%s", code, msg)
	}
	b.markHealthy()
	return nil
}

// isCardContentRejected reports whether err represents a Feishu CONTENT
// rejection: body too large (230025), too many tables/elements (11310), or
// any sibling "over limit" rejection. The SDK classifies these as unknown
// codes, so identification is by substring (the inner code + the English
// "over limit" phrase Feishu uses for element-count caps) rather than a typed
// constant.
func isCardContentRejected(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "code:"+strconv.Itoa(feishuCodeContentTooLarge)) ||
		strings.Contains(s, "code:"+strconv.Itoa(feishuCodeCardElementOverLimit)) ||
		strings.Contains(s, "over limit")
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
	card := struct {
		Schema string `json:"schema"`
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
