package feishu

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
)

// feishuClient is the subset of *lark.Client the Bot depends on. Declared as
// an interface so tests inject a fake without a real WS/REST round-trip;
// production wires *lark.Client, which satisfies it.
type feishuClient interface {
	Send(ctx context.Context, in *lark.SendInput) (*lark.SendResult, error)
	PatchMessage(ctx context.Context, messageID, content string) error
	DownloadResource(ctx context.Context, messageID, fileKey, fileType string) (io.ReadCloser, error)
	SetHandler(h lark.Handler)
	SetLifecycle(lc lark.Lifecycle)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

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
	// The dispatcher rejects unsupported types explicitly so a non-text payload
	// never reaches the backend as a prompt.
	MsgType string
	// Mentions carries the parsed user mentions in the message. text-type
	// messages embed "@_user_N" placeholders in Content; callers must run
	// StripMentionPlaceholders on Content before forwarding the prompt
	// downstream.
	Mentions []Mention
	// CreateTimeMs is the message send time in Unix milliseconds. 0 means
	// the field was absent or unparseable; the dispatcher's stale check
	// lets such messages through (de-dup alone guards them).
	CreateTimeMs int64
	// FileKey is set for file/image-type messages: the resource id used with
	// Bot.DownloadFile to fetch the binary. Empty for text/post/etc.
	FileKey string
	// FileName is the original upload name on a file-type message. Empty for
	// other types or when Feishu omits it.
	FileName string
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

// Bot is the Feishu client wrapper. It dispatches inbound events to
// registered handlers, surfaces send/update helpers via its methods, and
// exposes a health signal for the frontend watchdog. The underlying lark
// client manages its own WebSocket reconnection; this wrapper no longer
// needs the SDK-era soft-restart machinery.
type Bot struct {
	appID     string
	appSecret string
	botOpts   []BotOption

	client feishuClient

	// onIncoming/onCardAction are stored atomically: OnIncoming/OnCardAction
	// run on the main goroutine while the inbound handlers fire on the
	// lark client's WS goroutine. atomic.Pointer removes any ordering
	// assumption between registration and Start.
	onIncoming   atomic.Pointer[IncomingHandler]
	onCardAction atomic.Pointer[CardActionHandler]
	logger       *log.Logger

	logDebugRedact atomic.Bool // redact sensitive text from debug logs (opt-in); atomic, read concurrently with SetDebugRedact

	// lastHealthy is the unix-nano time of the most recent signal that the WS
	// connection is live (OnReady / OnReconnected / outbound success). The
	// frontend watchdog reads it to decide whether to exit for supervisor
	// recovery. 0 means "never healthy".
	lastHealthy atomic.Int64
}

// BotOption configures a Bot at construction time.
type BotOption func(*botConfig)

type botConfig struct {
	Domain   string
	LogLevel string
	// clientFactory overrides *lark.Client for tests; nil in production.
	// Wired in via withClientFactory (defined in bot_test.go).
	clientFactory feishuClient
}

// WithDomain overrides the default Feishu API domain (e.g. for testing).
func WithDomain(d string) BotOption {
	return func(c *botConfig) { c.Domain = d }
}

// WithLogLevel overrides the client log level (defaults to "info").
func WithLogLevel(l string) BotOption {
	return func(c *botConfig) { c.LogLevel = l }
}

// NewBotWithLogger creates a Bot with a slog.Logger.
func NewBotWithLogger(appID, appSecret string, logger *log.Logger, opts ...BotOption) (*Bot, error) {
	if appID == "" || appSecret == "" {
		return nil, errors.New("feishu: appID/appSecret required")
	}
	if logger == nil {
		logger = log.Nop()
	}
	cfg := applyBotOpts(opts)
	b := &Bot{
		appID:     appID,
		appSecret: appSecret,
		botOpts:   opts,
		logger:    logger,
	}
	if cfg.clientFactory != nil {
		b.client = cfg.clientFactory
	} else {
		c, err := lark.NewClient(appID, appSecret, larkOpts(cfg)...)
		if err != nil {
			return nil, err
		}
		b.client = c
	}
	b.client.SetHandler(larkHandlerAdapter{b: b})
	b.client.SetLifecycle(b.lifecycle())
	return b, nil
}

// larkHandlerAdapter adapts *Bot to the lark.Handler interface. It exists
// because the Bot's public API already exposes OnCardAction(CardActionHandler)
// for handler registration, which collides with lark.Handler.OnCardAction's
// signature; routing the inbound calls through private methods sidesteps the
// clash without renaming the public registration surface.
type larkHandlerAdapter struct{ b *Bot }

func (a larkHandlerAdapter) OnMessageReceive(ctx context.Context, ev *lark.MessageReceiveEvent) error {
	return a.b.handleMessageReceive(ctx, ev)
}

func (a larkHandlerAdapter) OnCardAction(ctx context.Context, ev *lark.CardActionEvent) error {
	return a.b.handleCardAction(ctx, ev)
}

// larkOpts translates the feishu wrapper config into lark.Client options.
func larkOpts(cfg botConfig) []lark.Option {
	opts := []lark.Option{}
	if cfg.Domain != "" {
		opts = append(opts, lark.WithDomain(cfg.Domain))
	}
	if cfg.LogLevel != "" {
		opts = append(opts, lark.WithLogLevel(cfg.LogLevel))
	}
	return opts
}

// applyBotOpts folds the option chain onto a default botConfig.
func applyBotOpts(opts []BotOption) botConfig {
	cfg := botConfig{Domain: "feishu", LogLevel: "info"}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// lifecycle builds the lark.Lifecycle that refreshes the health signal and
// logs connection state. The new client reconnects itself, so these
// callbacks are purely informational + health-refresh.
func (b *Bot) lifecycle() lark.Lifecycle {
	return lark.Lifecycle{
		OnReady: func() {
			b.logger.Info("websocket connection established")
			b.markHealthy()
		},
		OnError: func(err error) {
			b.logger.Error("websocket connection error", log.FieldError, err.Error())
		},
		OnReconnecting: func() {
			b.logger.Info("websocket reconnection started")
		},
		OnReconnected: func() {
			b.logger.Info("websocket reconnected successfully")
			b.markHealthy()
		},
		OnDisconnected: func() {
			b.logger.Warn("websocket connection closed", log.FieldReason, "server_initiated_or_network_error")
		},
	}
}

// Start connects the underlying client and blocks until ctx is done or a
// fatal (auth) bootstrap error occurs. The lark client owns reconnection;
// this wrapper no longer performs soft-restart.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.client.Start(ctx); err != nil {
		return err
	}
	return nil
}

// Stop gracefully shuts down the underlying client.
func (b *Bot) Stop(ctx context.Context) error {
	return b.client.Stop(ctx)
}

// DownloadFile fetches a binary resource attached to a Feishu message. The
// returned reader MUST be closed by the caller. fileType selects the IM
// resources endpoint's `type` query parameter ("file" or "image"); empty
// defaults to "file". Used by the frontend dispatcher to materialise an
// uploaded file for pandoc conversion.

// DownloadFile fetches a binary resource attached to a Feishu message. The
// returned reader MUST be closed by the caller. fileType selects the IM
// resources endpoint's `type` query parameter ("file" or "image"); empty
// defaults to "file". Used by the frontend dispatcher to materialise an
// uploaded file for pandoc conversion.
func (b *Bot) DownloadFile(ctx context.Context, messageID, fileKey, fileType string) (io.ReadCloser, error) {
	if fileType == "" {
		fileType = "file"
	}
	return b.client.DownloadResource(ctx, messageID, fileKey, fileType)
}

// LastHealthy returns the time of the most recent OnReady/OnReconnected/
// outbound success, or the zero time if the bot has never been healthy.
// Read by the frontend watchdog to decide whether to exit for supervisor
// recovery.
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
//
// The lark client reconnects itself on transient failure; this watchdog is
// the fallback for catastrophic cases (the reconnect loop wedged, or the
// process lost the ability to send/receive entirely). Exiting lets systemd
// pull up a fresh copy.
func ShouldExitUnhealthy(now, lastHealthy, startedAt time.Time, fatalAfter time.Duration) bool {
	if lastHealthy.IsZero() {
		return false // never connected yet; initial-connect failure surfaces from Start
	}
	if now.Sub(startedAt) < fatalAfter {
		return false // grace period: let the client settle past a transient blip at startup
	}
	return now.Sub(lastHealthy) > fatalAfter
}

// OnIncoming registers the handler invoked for each inbound message. Passing
// nil unregisters (a nil IncomingHandler stored as a non-nil pointer would
// otherwise defeat the Load()==nil guard and panic on every message).
func (b *Bot) OnIncoming(handler IncomingHandler) {
	if handler == nil {
		b.onIncoming.Store(nil)
		return
	}
	b.onIncoming.Store(&handler)
}

// OnCardAction registers the handler invoked for each card callback. nil
// unregisters (see OnIncoming).
func (b *Bot) OnCardAction(handler CardActionHandler) {
	if handler == nil {
		b.onCardAction.Store(nil)
		return
	}
	b.onCardAction.Store(&handler)
}

// SetDebugRedact enables or disables redaction of sensitive text from debug logs.
func (b *Bot) SetDebugRedact(redact bool) {
	b.logDebugRedact.Store(redact)
}
