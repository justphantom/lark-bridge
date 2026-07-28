package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
)

// handleMessageReceive is the lark.Handler entry point for inbound messages.
// The lark client calls it (via a small adapter) on its WS goroutine after
// parsing the im.message.receive_v1 payload. We normalize the event into
// IncomingMessage and fan out to the registered IncomingHandler.
func (b *Bot) handleMessageReceive(ctx context.Context, ev *lark.MessageReceiveEvent) error {
	incoming, err := buildIncomingMessage(ev, b.logger)
	if err != nil {
		b.logger.Warn("message handling failed", log.FieldReason, err.Error())
		return nil
	}

	b.markHealthy() // any inbound event proves the WS is alive
	start := time.Now()
	b.logger.Info("message received from Feishu",
		log.FieldChatID, incoming.ChatID,
		log.FieldMessageID, incoming.MessageID)

	b.logger.Info("message forwarding to handler",
		log.FieldChatID, incoming.ChatID,
		log.FieldMessageID, incoming.MessageID,
		"sender_open_id", incoming.SenderOpenID,
		"chat_type", incoming.ChatType,
		"content_type", incoming.MsgType)

	h := b.onIncoming.Load()
	if h == nil {
		duration := time.Since(start)
		b.logger.Warn("message handling failed",
			log.FieldChatID, incoming.ChatID,
			log.FieldMessageID, incoming.MessageID,
			log.FieldDuration, duration.Milliseconds(),
			log.FieldReason, "no_handler")
		return nil
	}

	// Recover per inbound message: OnMessageReceive runs on the lark
	// client's WS goroutine, and (*h) fans out into routing, prompt
	// emission, command execution — any of which can panic on a malformed
	// payload. Letting the panic escape would crash the WS reader and
	// silently drop every later message. Catch it here, log, and promote
	// to a returned error so the lark client still sees (and ACKs) the
	// failure.
	err = func() (outErr error) {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error("panic in incoming handler",
					log.FieldChatID, incoming.ChatID,
					log.FieldMessageID, incoming.MessageID,
					log.FieldPanic, r,
					log.FieldStack, string(debug.Stack()))
				outErr = fmt.Errorf("incoming handler panic: %v", r)
			}
		}()
		return (*h)(ctx, incoming)
	}()
	duration := time.Since(start)
	if err != nil {
		b.logger.Error("message handling failed",
			log.FieldChatID, incoming.ChatID,
			log.FieldMessageID, incoming.MessageID,
			log.FieldDuration, duration.Milliseconds(),
			log.FieldError, err.Error())
		return err
	}
	b.logger.Info("message handled successfully",
		log.FieldChatID, incoming.ChatID,
		log.FieldMessageID, incoming.MessageID,
		log.FieldDuration, duration.Milliseconds())
	return nil
}

// handleCardAction is the lark.Handler entry point for card callbacks.
// Guard logic mirrors handleMessageReceive: drop events with no operator,
// then fan out under a recover so a downstream panic cannot escape into the
// WS goroutine.
func (b *Bot) handleCardAction(ctx context.Context, ev *lark.CardActionEvent) error {
	if ev == nil {
		b.logger.Warn("card action: nil event dropped")
		return nil
	}
	b.markHealthy() // any inbound event proves the WS is alive
	// 入口 Info：诊断 ws→lark 链路是否把 card 事件投递到 Bot。
	// value 整条记录，便于发现飞书 schema 2.0 下 button 回调 value 字段缺失。
	b.logger.Info("card action received",
		"event_id", ev.EventID,
		log.FieldChatID, ev.ChatID,
		log.FieldMessageID, ev.MessageID,
		"operator_openid", ev.Operator.OpenID,
		"value", ev.Action.Value)
	h := b.onCardAction.Load()
	if h == nil {
		b.logger.Warn("card action: no handler registered",
			"event_id", ev.EventID, log.FieldChatID, ev.ChatID)
		return nil
	}
	if ev.Operator.OpenID == "" {
		b.logger.Debug("drop card action: empty operator openid", "event_id", ev.EventID, log.FieldChatID, ev.ChatID)
		return nil
	}
	return func() (outErr error) {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error("panic in card action handler",
					"event_id", ev.EventID,
					log.FieldChatID, ev.ChatID,
					log.FieldPanic, r,
					log.FieldStack, string(debug.Stack()))
				outErr = fmt.Errorf("card action handler panic: %v", r)
			}
		}()
		return (*h)(ctx, buildCardAction(ev))
	}()
}

// buildIncomingMessage converts the parsed event into the bridge's
// IncomingMessage. Errors here are non-fatal: the caller logs and ACKs so
// the server does not redeliver a structurally-bad event forever.
func buildIncomingMessage(ev *lark.MessageReceiveEvent, logger *log.Logger) (*IncomingMessage, error) {
	if ev == nil {
		return nil, errors.New("nil_event")
	}
	content := ev.Content
	if ev.MsgType == "text" {
		if parsed, parseErr := parseTextContent(content); parseErr != nil {
			logger.Debug("parse text content failed", log.FieldError, parseErr)
		} else {
			content = parsed
		}
	}
	return &IncomingMessage{
		EventID:      ev.EventID,
		MessageID:    ev.MessageID,
		ChatID:       ev.ChatID,
		ChatType:     ev.ChatType,
		SenderOpenID: ev.SenderOpenID,
		Content:      content,
		MsgType:      ev.MsgType,
		Mentions:     convertLarkMentions(ev.Mentions),
		CreateTimeMs: ev.CreateTimeMs,
		FileKey:      ev.FileKey,
		FileName:     ev.FileName,
	}, nil
}

// convertLarkMentions copies the lark-level mention slice into the feishu
// package's value type. The field sets match by design; nil → nil.
func convertLarkMentions(in []lark.Mention) []Mention {
	if len(in) == 0 {
		return nil
	}
	out := make([]Mention, len(in))
	for i, m := range in {
		out[i] = Mention{Key: m.Key, OpenID: m.OpenID, Name: m.Name, IsBot: m.IsBot}
	}
	return out
}

// parseTextContent extracts the inner text from Feishu's text-message JSON
// wrapper. Returns an error when content is non-empty but not valid JSON so
// the caller can log it; a non-empty error leaves the caller's content
// unchanged.
func parseTextContent(content string) (string, error) {
	if content == "" {
		return "", nil
	}
	var wrapper struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &wrapper); err != nil {
		return content, err
	}
	return wrapper.Text, nil
}

// buildCardAction converts a lark CardActionEvent into the bridge's
// CardAction struct.
func buildCardAction(ev *lark.CardActionEvent) *CardAction {
	return &CardAction{
		EventID:    ev.EventID,
		ChatID:     ev.ChatID,
		MessageID:  ev.MessageID,
		Value:      ev.Action.Value,
		FormValue:  ev.Action.FormValue,
		UserOpenID: ev.Operator.OpenID,
	}
}
