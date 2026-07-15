package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	sdktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
	larkimv1 "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func (b *Bot) handleP2MessageReceiveV1(ctx context.Context, event *larkimv1.P2MessageReceiveV1) error {
	incoming, err := extractIncomingMessage(event, b.logger)
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

	err = (*h)(ctx, incoming)
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

// extractIncomingMessage converts the SDK event into our internal message.
// It is intentionally strict about nil pointer checks because the SDK fields
// are pointers and panics here were observed in production; extracting the
// data keeps the handler focused on routing/logging.
func extractIncomingMessage(event *larkimv1.P2MessageReceiveV1, logger *log.Logger) (*IncomingMessage, error) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil, errors.New("nil_event")
	}
	msg := event.Event.Message

	chatID := derefString(msg.ChatId)
	messageID := derefString(msg.MessageId)
	content := derefString(msg.Content)
	chatType := derefString(msg.ChatType)
	msgType := derefString(msg.MessageType)

	var senderOpenID string
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil {
		senderOpenID = derefString(event.Event.Sender.SenderId.OpenId)
	}

	var createTimeMs int64
	if msg.CreateTime != nil {
		if ms, err := strconv.ParseInt(*msg.CreateTime, 10, 64); err == nil {
			createTimeMs = ms
		}
	}

	if msgType == "text" {
		parsed, parseErr := parseTextContent(content)
		if parseErr != nil {
			logger.Debug("parse text content failed", log.FieldError, parseErr)
		} else {
			content = parsed
		}
	}

	return &IncomingMessage{
		EventID:      event.EventV2Base.Header.EventID,
		MessageID:    messageID,
		ChatID:       chatID,
		ChatType:     chatType,
		SenderOpenID: senderOpenID,
		Content:      content,
		MsgType:      msgType,
		Mentions:     extractMentions(msg.Mentions),
		CreateTimeMs: createTimeMs,
	}, nil
}

// derefString returns the string value of a SDK string pointer, or "" for nil.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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

// extractMentions copies SDK mention pointers into a slice of concrete values,
// skipping nil entries and converting the bot flag to a boolean.
func extractMentions(raw []*larkimv1.MentionEvent) []sdktypes.Mention {
	mentions := make([]sdktypes.Mention, 0, len(raw))
	for _, mention := range raw {
		if mention == nil {
			continue
		}
		parsed := sdktypes.Mention{
			Key:  derefString(mention.Key),
			Name: derefString(mention.Name),
		}
		if mention.Id != nil {
			parsed.OpenID = derefString(mention.Id.OpenId)
		}
		if mention.MentionedType != nil && *mention.MentionedType == "app" {
			parsed.IsBot = true
		}
		mentions = append(mentions, parsed)
	}
	return mentions
}
