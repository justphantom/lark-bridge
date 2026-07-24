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

	"github.com/justphantom/lark-bridge/internal/log"
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
	appID      string
	appSecret  string
	botOpts    []BotOption // 留底供 Restart 重建 ws 配置
	larkClient *lark.Client

	// ch 用 atomic.Pointer 而非直接字段:支持 Restart 期间并发 Send 调用,Load
	// 总返回可用 channel(旧或新),不会读到中间态。详见
	// docs/feishu-ws-soft-restart.md §3。
	ch        atomic.Pointer[sdktypes.Channel]
	imService *larkim.Service

	// newChannelFn overrides newChannel when non-nil. Always nil in production;
	// tests inject a fakeChannel factory to exercise Restart/NewBotWithLogger
	// without a real WS handshake.
	newChannelFn func() sdktypes.Channel

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

	// restartCount 累积 soft-restart 次数,达 restartMax 后 Restart 返回
	// ErrTooManyRestarts,调用方应让进程退出交由 supervisor 重启。健康恢复时
	// (markHealthy)清零,仅持续故障才触顶。
	restartCount atomic.Int32
}

// BotOption configures a Bot at construction time.
type BotOption func(*botConfig)

type botConfig struct {
	Domain   string
	LogLevel string
	// channelFactory overrides newChannel for tests; nil in production.
	// Wired in via withChannelFactory (defined in bot_test.go).
	channelFactory func() sdktypes.Channel
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
	if logger == nil {
		logger = log.Nop()
	}
	cfg := applyBotOpts(opts)
	b := &Bot{
		appID:        appID,
		appSecret:    appSecret,
		botOpts:      opts,
		logger:       logger,
		newChannelFn: cfg.channelFactory,
	}

	lvl := toLarkLogLevel(cfg.LogLevel)
	// WithReqTimeout: SDK leaves ReqTimeout==0 → http.DefaultClient (no
	// Timeout), so a stuck Feishu API would block dispatcher goroutines
	// forever. Bound it explicitly.
	// larkClient 一次构造全期复用:SDK 评估项 2 已证 Im 服务无状态、并发安全。
	b.larkClient = lark.NewClient(appID, appSecret,
		lark.WithLogLevel(lvl),
		lark.WithReqTimeout(30*time.Second),
	)
	b.imService = b.larkClient.Im

	ch := b.freshChannel()
	b.ch.Store(&ch)
	b.registerHandlersOn(ch)
	return b, nil
}

// applyBotOpts folds the option chain onto a default botConfig. Shared by
// NewBotWithLogger and newChannel so the two paths cannot drift on defaults.
func applyBotOpts(opts []BotOption) botConfig {
	cfg := botConfig{Domain: "feishu", LogLevel: "info"}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// freshChannel returns a channel for NewBotWithLogger / Restart. It routes
// through newChannelFn when set (tests only); production always calls
// newChannel.
func (b *Bot) freshChannel() sdktypes.Channel {
	if b.newChannelFn != nil {
		return b.newChannelFn()
	}
	return b.newChannel()
}

// newChannel constructs a fresh underlying WS channel with its own wsClient
// (the SDK's wsClient is stateful; reusing one across Stop/Start would keep
// stale conn state). Handlers are NOT registered here — that is
// registerHandlersOn's job, kept separate so Restart can re-mount them on
// each fresh channel.
func (b *Bot) newChannel() sdktypes.Channel {
	cfg := applyBotOpts(b.botOpts)
	lvl := toLarkLogLevel(cfg.LogLevel)

	// Each channel needs its own event dispatcher: the SDK routes inbound
	// P2MessageReceiveV1 through the dispatcher bound at wsClient
	// construction, so a shared dispatcher would silently drop events on the
	// new channel after Restart.
	eventDispatcher := dispatcher.NewEventDispatcher("", "")
	eventDispatcher.OnP2MessageReceiveV1(b.handleP2MessageReceiveV1)
	wsOpts := []larkws.ClientOption{
		larkws.WithLogLevel(lvl),
		larkws.WithEventHandler(eventDispatcher),
	}
	if cfg.Domain != "" && cfg.Domain != "feishu" {
		wsOpts = append(wsOpts, larkws.WithDomain(cfg.Domain))
	}
	wsClient := larkws.NewClient(b.appID, b.appSecret, wsOpts...)

	outboundCfg := sdktypes.DefaultChannelConfig().Outbound
	outboundCfg.TextChunkLimit = maxContentSize
	return channel.NewChannel(b.larkClient, wsClient,
		sdktypes.WithOutboundConfig(outboundCfg),
	)
}

// Start connects the WebSocket channel and blocks until ctx is done.
func (b *Bot) Start(ctx context.Context) error {
	ch := b.ch.Load()
	if ch == nil {
		return errors.New("feishu: bot channel not initialized")
	}
	if err := (*ch).Start(ctx); err != nil {
		return fmt.Errorf("feishu: channel start: %w", err)
	}
	return nil
}

// restartMax 限制 soft-restart 累积次数。每次 Restart 必 leak 一个旧 goroutine
// (SDK 用 select{} 结束 Start,见 Restart 注释),达限后返回 ErrTooManyRestarts
// 退出进程,由 systemd 拉起干净副本。健康恢复(markHealthy)清零,仅持续故障
// 触顶。最快 wsFatalAfter/次,5 次即最坏 25min 故障容忍窗口。
const restartMax = 5

// ErrTooManyRestarts 由 Restart 返回:累计 soft-restart 次数已达 restartMax。
// 调用方应让进程退出交由 supervisor(systemd Restart=on-failure)拉起干净副本,
// 避免在 SDK 无法回收旧 Start goroutine 的情况下无限累积。
var ErrTooManyRestarts = errors.New("feishu: too many soft restarts; exit for supervisor recovery")

// Restart swaps the underlying WS channel for a fresh one in place. On success
// subsequent Load() returns the new channel; the old channel is stopped.
//
// 旧 goroutine leak 不可根除:larksuite SDK 在 ws/client.go:230 用裸 select{}
// 结束 Start,既不监听 ctx 也不响应 Close,所以旧 Start goroutine 永不返回。每次
// Restart 必 leak 一个旧 goroutine,由 restartCount+restartMax 限流:累计达
// restartMax 后返回 ErrTooManyRestarts,调用方 os.Exit 让 supervisor 拉起干净
// 副本。markHealthy 清零计数。详见 docs/feishu-ws-soft-restart.md §8「已知限制」。
//
// P0 未做串行化:并发 Restart 会丢失对中间 channel 的引用,watchdog tick 间隔
// 30s 远大于 Restart 耗时,P1 加 sync.Mutex 收口。
func (b *Bot) Restart(ctx context.Context) error {
	if b.restartCount.Add(1) > restartMax {
		return ErrTooManyRestarts
	}
	fresh := b.freshChannel()
	b.registerHandlersOn(fresh)
	old := b.ch.Swap(&fresh)
	go func() { _ = fresh.Start(ctx) }() //nolint:errcheck // 新 goroutine;旧 goroutine leak(见上)
	if old == nil {
		return nil
	}
	return (*old).Stop(ctx)
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
	b.restartCount.Store(0)
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

// Stop gracefully shuts down the current WebSocket channel.
func (b *Bot) Stop(ctx context.Context) error {
	ch := b.ch.Load()
	if ch == nil {
		return nil
	}
	return (*ch).Stop(ctx)
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
