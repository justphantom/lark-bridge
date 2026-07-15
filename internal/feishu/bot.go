package feishu

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/channel"
	sdktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/hu/lark-bridge/internal/log"
)

// IncomingMessage is the normalized payload for one inbound Feishu
// message delivered to the bot.
type IncomingMessage struct {
	EventID      string
	MessageID    string
	ChatID       string
	ChatType     string
	SenderOpenID string
	Content      string
	// MsgType is the Feishu message type ("text", "image", "file", "post", …).
	// The dispatcher rejects anything but "text" so a non-text payload never
	// reaches the backend as a prompt.
	MsgType string
	// Mentions carries the SDK-parsed user mentions in the message.
	// text-type messages embed "@_user_N" placeholders in Content;
	// callers must run StripMentionPlaceholders on Content before
	// forwarding the prompt downstream.
	Mentions []sdktypes.Mention
	// CreateTimeMs is the message send time in Unix milliseconds, parsed
	// from EventMessage.CreateTime. 0 means the field was absent or
	// unparseable; the dispatcher's stale check lets such messages
	// through (de-dup alone guards them).
	CreateTimeMs int64
}

// CardAction is the normalized payload for one interactive card
// callback (button click, form submit) from a Feishu message.
type CardAction struct {
	EventID   string
	ChatID    string
	MessageID string
	Value     map[string]any // Button value data (for non-form button clicks)
	// FormValue carries the form container's submitted values (Feishu
	// action.form_value), keyed by each interactive component's name.
	// Populated only when the click is a form_submit; nil for plain buttons.
	FormValue  map[string]any
	UserOpenID string
}

// IncomingHandler handles a normalized inbound message.
type IncomingHandler func(context.Context, *IncomingMessage) error

// CardActionHandler handles a normalized card-action callback.
type CardActionHandler func(context.Context, *CardAction) error

// Bot is the Feishu WebSocket client wrapper. It dispatches inbound
// events to registered handlers, manages reconnection, and exposes
// send helpers via its methods.
type Bot struct {
	ch        sdktypes.Channel
	imService *larkim.Service

	// onIncoming/onCardAction are stored atomically: OnIncoming/OnCardAction
	// run on the main goroutine while handleP2MessageReceiveV1/handleCardAction
	// fire on the SDK's WS goroutine. atomic.Pointer removes any ordering
	// assumption between registration and Start, matching how logger/debug
	// flags are already protected.
	onIncoming   atomic.Pointer[IncomingHandler]
	onCardAction atomic.Pointer[CardActionHandler]
	logger       *log.Logger

	logDebugRedact atomic.Bool // redact sensitive text from debug logs (opt-in); atomic, read concurrently with SetDebugRedact

	// lastHealthy is the unix-nano time of the most recent signal that the WS
	// connection is live (OnReady / OnReconnected). The frontend watchdog reads
	// it to detect the SDK's "Start succeeds then silently dies forever" mode:
	// since Start blocks on select{} and never returns, a permanently-dead link
	// leaves the process up but dropping every message. 0 means "never healthy".
	lastHealthy atomic.Int64
}

// BotOption configures a Bot at construction time.
type BotOption func(*botConfig)

type botConfig struct {
	Domain   string
	LogLevel string
}

// WithDomain overrides the default Feishu API domain (e.g. for testing).
func WithDomain(d string) BotOption {
	return func(c *botConfig) { c.Domain = d }
}

// WithLogLevel overrides the SDK log level (defaults to "info").
func WithLogLevel(l string) BotOption {
	return func(c *botConfig) { c.LogLevel = l }
}

// NewBotWithLogger creates a Bot with a slog.Logger.
func NewBotWithLogger(appID, appSecret string, logger *log.Logger, opts ...BotOption) (*Bot, error) {
	if appID == "" || appSecret == "" {
		return nil, errors.New("feishu: appID/appSecret required")
	}
	cfg := botConfig{Domain: "feishu", LogLevel: "info"}
	for _, o := range opts {
		o(&cfg)
	}
	if logger == nil {
		logger = log.Nop()
	}
	b := &Bot{logger: logger}

	lvl := toLarkLogLevel(cfg.LogLevel)
	// WithReqTimeout: SDK leaves ReqTimeout==0 → http.DefaultClient (no
	// Timeout), so a stuck Feishu API would block dispatcher goroutines
	// forever. Bound it explicitly.
	larkClient := lark.NewClient(appID, appSecret,
		lark.WithLogLevel(lvl),
		lark.WithReqTimeout(30*time.Second),
	)
	eventDispatcher := dispatcher.NewEventDispatcher("", "")
	eventDispatcher.OnP2MessageReceiveV1(b.handleP2MessageReceiveV1)
	wsOpts := []larkws.ClientOption{
		larkws.WithLogLevel(lvl),
		larkws.WithEventHandler(eventDispatcher),
	}
	if cfg.Domain != "" && cfg.Domain != "feishu" {
		wsOpts = append(wsOpts, larkws.WithDomain(cfg.Domain))
	}
	wsClient := larkws.NewClient(appID, appSecret, wsOpts...)

	outboundCfg := sdktypes.DefaultChannelConfig().Outbound
	outboundCfg.TextChunkLimit = maxContentSize
	b.ch = channel.NewChannel(larkClient, wsClient,
		sdktypes.WithOutboundConfig(outboundCfg),
	)
	b.imService = larkClient.Im
	b.registerHandlers()
	return b, nil
}

// Start connects the WebSocket channel and blocks until ctx is done.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.ch.Start(ctx); err != nil {
		return fmt.Errorf("feishu: channel start: %w", err)
	}
	return nil
}

// LastHealthy returns the time of the most recent OnReady/OnReconnected, or
// the zero time if the bot has never been healthy. Read by the frontend
// watchdog (cmd/feishu-front) to detect the SDK's silent-death mode.
func (b *Bot) LastHealthy() time.Time {
	ns := b.lastHealthy.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (b *Bot) markHealthy() {
	b.lastHealthy.Store(time.Now().UnixNano())
}

// ShouldExitUnhealthy reports whether the watchdog should fatal-exit the
// process: only after the bot has been healthy at least once (so a slow
// initial connect is not mistaken for death), and only when no healthy
// signal has arrived within fatalAfter. Pure function for unit testing.
func ShouldExitUnhealthy(now, lastHealthy, startedAt time.Time, fatalAfter time.Duration) bool {
	if lastHealthy.IsZero() {
		return false // never connected yet; initial-connect failure is surfaced by Start itself
	}
	if now.Sub(startedAt) < fatalAfter {
		return false // grace period: let the SDK settle past a transient blip at startup
	}
	return now.Sub(lastHealthy) > fatalAfter
}

// Stop gracefully shuts down the WebSocket channel.
func (b *Bot) Stop(ctx context.Context) error {
	if b.ch == nil {
		return nil
	}
	return b.ch.Stop(ctx)
}

// OnIncoming registers the handler invoked for each inbound message.
func (b *Bot) OnIncoming(handler IncomingHandler) {
	b.onIncoming.Store(&handler)
}

// OnCardAction registers the handler invoked for each card callback.
func (b *Bot) OnCardAction(handler CardActionHandler) {
	b.onCardAction.Store(&handler)
}

// SetDebugRedact enables or disables redaction of sensitive text from debug logs.
func (b *Bot) SetDebugRedact(redact bool) {
	b.logDebugRedact.Store(redact)
}
