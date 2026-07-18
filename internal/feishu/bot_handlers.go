package feishu

import (
	"context"

	sdktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	"github.com/justphantom/lark-bridge/internal/log"
)

func (b *Bot) registerHandlers() {
	b.ch.OnCardAction(func(ctx context.Context, ev *sdktypes.CardActionEvent) error {
		return b.handleCardAction(ctx, ev)
	})

	b.ch.OnReady(func() {
		b.logger.Info("websocket connection established")
		b.markHealthy()
	})

	b.ch.OnError(func(err error) {
		b.logger.Error("websocket connection error",
			log.FieldError, err.Error())
	})
	b.ch.OnReconnecting(func() {
		b.logger.Info("websocket reconnection started")
	})
	b.ch.OnReconnected(func() {
		b.logger.Info("websocket reconnected successfully")
		b.markHealthy()
	})
	b.ch.OnDisconnected(func() {
		b.logger.Warn("websocket connection closed",
			log.FieldReason, "server_initiated_or_network_error")
	})
}

// handleCardAction is the body of the OnCardAction callback, extracted
// so the guard logic is unit-testable without spinning up a real WS
// channel. Returns nil for any event the bridge cannot process so the
// channel never sees a panic; recover() at the channel layer would
// otherwise swallow the panic and the click would silently no-op.
//
// Note: in this SDK version Operator and Action are value types
// (CardActionOperator / CardActionPayload), so they cannot be nil. The
// realistic malformed-payload failure mode is an empty Operator.OpenID
// (e.g. permission revoked mid-session, anonymous test click): we drop
// such events with a single log line because downstream permission
// checks and reply addressing need a real user id.
func (b *Bot) handleCardAction(ctx context.Context, ev *sdktypes.CardActionEvent) error {
	if ev == nil {
		return nil
	}
	b.markHealthy() // any inbound event proves the WS is alive
	h := b.onCardAction.Load()
	if h == nil {
		return nil
	}
	if ev.Operator.OpenID == "" {
		b.logger.Debug("drop card action: empty operator openid", "event_id", ev.EventID, log.FieldChatID, ev.ChatID)
		return nil
	}
	return (*h)(ctx, buildCardAction(ev))
}

func toLarkLogLevel(level string) larkcore.LogLevel {
	switch level {
	case "debug":
		return larkcore.LogLevelDebug
	case "warn":
		return larkcore.LogLevelWarn
	case "error":
		return larkcore.LogLevelError
	default:
		return larkcore.LogLevelInfo
	}
}

// buildCardAction converts an SDK CardActionEvent into the bridge's
// CardAction struct. Caller MUST have already verified that ev.Action
// and ev.Operator are non-nil (see registerHandlers).
func buildCardAction(ev *sdktypes.CardActionEvent) *CardAction {
	return &CardAction{
		EventID:    ev.EventID,
		ChatID:     ev.ChatID,
		MessageID:  ev.MessageID,
		Value:      ev.Action.Value,
		FormValue:  ev.Action.FormValue,
		UserOpenID: ev.Operator.OpenID,
	}
}
