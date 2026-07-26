package lark

import "context"

// Mention mirrors the SDK's Mention shape that feishu.StripMentionPlaceholders
// and feishu.IncomingMessage consume. Field set is the minimum the bridge uses.
type Mention struct {
	Key    string // "@_user_1" placeholder
	OpenID string // "ou_xxx"
	Name   string // display name
	IsBot  bool   // true if the mention targets the running app
}

// SendInput is the subset of fields feishu.Bot.Send* uses. Exactly one of
// Text or Card must be set; ReplyMessageID (when non-empty) routes the send
// through the reply endpoint instead of the create endpoint.
type SendInput struct {
	ChatID         string
	Text           string
	Card           string
	ReplyMessageID string
}

// SendResult carries the new message id returned by Send.
type SendResult struct {
	MessageID string
}

// CardActionEvent is the parsed payload of card.action.trigger.
type CardActionEvent struct {
	EventID   string
	ChatID    string
	MessageID string
	Operator  CardActionOperator
	Action    CardActionPayload
}

// CardActionOperator carries the user who clicked.
type CardActionOperator struct {
	OpenID string
}

// CardActionPayload carries the button/form data attached to the click.
type CardActionPayload struct {
	Value     map[string]any
	FormValue map[string]any
}

// MessageReceiveEvent is the parsed payload of im.message.receive_v1.
type MessageReceiveEvent struct {
	EventID      string
	MessageID    string
	ChatID       string
	ChatType     string
	MsgType      string
	Content      string
	CreateTimeMs int64
	SenderOpenID string
	Mentions     []Mention
}

// Handler is the consumer of inbound WS events. Both methods must be safe to
// call from the WS client's goroutines; the lark.Client serialises nothing.
type Handler interface {
	OnMessageReceive(ctx context.Context, ev *MessageReceiveEvent) error
	OnCardAction(ctx context.Context, ev *CardActionEvent) error
}

// HandlerFunc is a convenience adapter letting a caller satisfy Handler with
// closures. A nil field is a no-op (returns nil).
type HandlerFunc struct {
	OnMessageReceiveFn func(ctx context.Context, ev *MessageReceiveEvent) error
	OnCardActionFn     func(ctx context.Context, ev *CardActionEvent) error
}

// OnMessageReceive dispatches to OnMessageReceiveFn when set.
func (h HandlerFunc) OnMessageReceive(ctx context.Context, ev *MessageReceiveEvent) error {
	if h.OnMessageReceiveFn == nil {
		return nil
	}
	return h.OnMessageReceiveFn(ctx, ev)
}

// OnCardAction dispatches to OnCardActionFn when set.
func (h HandlerFunc) OnCardAction(ctx context.Context, ev *CardActionEvent) error {
	if h.OnCardActionFn == nil {
		return nil
	}
	return h.OnCardActionFn(ctx, ev)
}

// Lifecycle bundles the WS connection lifecycle callbacks. Any nil field is a
// no-op; this matches the SDK's per-channel OnXXX registration surface so the
// feishu wrapper can wire its existing watchdog/refresh logic unchanged.
type Lifecycle struct {
	OnReady        func()
	OnError        func(error)
	OnReconnecting func()
	OnReconnected  func()
	OnDisconnected func()
}
