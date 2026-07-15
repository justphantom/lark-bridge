package feishufront

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hu/lark-bridge/internal/feishu"
	"github.com/hu/lark-bridge/internal/feishufront/cardkit"
	"github.com/hu/lark-bridge/internal/feishufront/renderer"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
)

// Dedup TTLs: how long a seen id is remembered so a retried backend POST
// does not double-post. Events/actions share 5m (transport retries);
// terminal controls (result/error/notice) get 10m (replay suppression).
const (
	eventDedupTTL        = 5 * time.Minute
	actionDedupTTL       = 5 * time.Minute
	terminalDedupTTL     = 10 * time.Minute
	eventDedupMaxEntries = 1000
)

// defaultStaleWindow is the dispatcher's built-in stale-message threshold
// used when SetDedupConfig has not overridden it (or passed a non-positive
// value). A message whose create_time is older than this is silently
// dropped before entering the dedup table. Kept tighter than the SDK's
// 30min IsStale window so the application layer is the stricter first
// line; the SDK check remains as a backstop.
const defaultStaleWindow = 300 * time.Second

// maxPromptBytes bounds a forwarded prompt. It sits safely below Linux's
// per-argument MAX_ARG_STRLEN (128 KiB, hit by opencode's positional prompt
// argv) and below the backend SSE frame cap (1 MiB), so an oversized message
// is rejected up front with a notice instead of being silently dropped by the
// transport or rejected by execve.
const maxPromptBytes = 64 << 10 // 64 KiB

// CardSink is the subset of the Feishu bot the dispatcher needs: send a new
// card (returns message_id) or update an existing card.
type CardSink interface {
	SendCard(ctx context.Context, chatID string, card []byte, replyToID string) (string, error)
	UpdateCard(ctx context.Context, messageID string, card []byte) error
}

// ChatRouter maps a Feishu chatID to its bound backendID.
type ChatRouter interface {
	Resolve(chatID string) (string, error)
	Set(chatID, backendID string) error
	ChatsOf(backendID string) []string
}

// Dispatcher is the frontend orchestrator.
type Dispatcher struct {
	bot      CardSink
	registry *BackendRegistry
	turns    *TurnManager
	router   ChatRouter

	progressMu sync.Mutex
	progress   map[string]*renderer.ProgressState

	// finalized tracks messageIDs whose terminal card has been sent. A late
	// progress update arriving after the terminal card is dropped at updateCard
	// so the debouncer never flushes a stale progress frame over the final
	// result/notice. Guarded by progressMu.
	finalized map[string]struct{}

	// debouncer coalesces UpdateCard calls to avoid API rate limits.
	debouncer *cardDebouncer

	eventIDs  *dedupSet
	actionIDs *dedupSet
	// terminals dedupes terminal controls (Result/Error/Notice) by PromptID so
	// a retried POST from a backend does not double-post the final card. A
	// turn's terminal control is processed exactly once; subsequent duplicates
	// for the same PromptID within the TTL window are dropped.
	terminals *dedupSet

	// staleWindow bounds how old an inbound message may be (by create_time)
	// before it is dropped as a replay. <=0 falls back to defaultStaleWindow.
	staleWindow time.Duration

	cardMu sync.Mutex
	cards  map[string][]byte
	// interactiveTimers schedules each interactive card's expiry notice. A card
	// left pending longer than cardkit.InteractiveTimeout is flipped to an "已失效" state so
	// a returning user sees why the backend stopped waiting. Cancelled when the
	// user submits (DispatchCardAction). Guarded by cardMu alongside cards.
	interactiveTimers map[string]*time.Timer

	// logger is stored atomically: SetLogger runs on the main goroutine while
	// notifyBackendChat reads it from the IPCServer.fireCallback goroutine.
	logger atomic.Pointer[log.Logger]
}

func NewDispatcher(bot CardSink, registry *BackendRegistry, turns *TurnManager, router ChatRouter) *Dispatcher {
	d := &Dispatcher{
		bot:               bot,
		registry:          registry,
		turns:             turns,
		router:            router,
		progress:          make(map[string]*renderer.ProgressState),
		finalized:         make(map[string]struct{}),
		eventIDs:          newDedupSet(eventDedupTTL, eventDedupMaxEntries),
		actionIDs:         newDedupSet(actionDedupTTL, 0),
		terminals:         newDedupSet(terminalDedupTTL, 0),
		cards:             make(map[string][]byte),
		interactiveTimers: make(map[string]*time.Timer),
	}
	d.logger.Store(log.Nop())
	return d
}

// SetLogger wires the component logger. Called by main.go after NewDispatcher;
// nil is rejected to keep d.logger always usable.
func (d *Dispatcher) SetLogger(l *log.Logger) {
	if l != nil {
		d.logger.Store(l)
	}
}

// SetDedupConfig overrides the built-in replay-guard parameters. Called by
// main.go after NewDispatcher. Each non-positive argument keeps the
// dispatcher's built-in default (defaultStaleWindow / eventDedupTTL /
// eventDedupMaxEntries). Only eventIDs is affected; actionIDs and terminals
// stay TTL-only because their volume is far lower.
func (d *Dispatcher) SetDedupConfig(staleWindow, eventTTL time.Duration, eventMaxEntries int) {
	if staleWindow > 0 {
		d.staleWindow = staleWindow
	}
	// dedupSet.Configure handles the locking and field updates so the
	// dispatcher does not reach into the set's private fields.
	if eventTTL > 0 || eventMaxEntries > 0 {
		ttl := eventTTL
		if ttl <= 0 {
			ttl = d.eventIDs.ttl
		}
		max := eventMaxEntries
		if max <= 0 {
			max = d.eventIDs.maxEntries
		}
		d.eventIDs.Configure(ttl, max)
	}
}

// isStale reports whether a message should be dropped as too old by
// create_time. A non-positive CreateTimeMs (field absent / parse failure)
// returns false so such messages are still de-duplicated. The window is
// d.staleWindow, falling back to defaultStaleWindow when unset.
func (d *Dispatcher) isStale(createTimeMs int64) bool {
	if createTimeMs <= 0 {
		return false
	}
	w := d.staleWindow
	if w <= 0 {
		w = defaultStaleWindow
	}
	return time.Since(time.UnixMilli(createTimeMs)) > w
}

// InitDebouncer creates and wires a card debouncer using the app context (so
// it flushes on shutdown) and the given flush interval. The debouncer type
// is unexported, keeping the implementation inside the package.
func (d *Dispatcher) InitDebouncer(ctx context.Context, interval time.Duration) {
	d.debouncer = newCardDebouncer(ctx, d.bot, interval)
}

// updateCard sends (or enqueues) an UpdateCard. When a debouncer is wired,
// progress updates go through it; terminal updates (result/notice) go direct.
// A messageID marked finalized (its terminal card already sent) rejects the
// update so a straggler progress frame can never overwrite the final card.
func (d *Dispatcher) updateCard(ctx context.Context, messageID string, card []byte) error {
	d.progressMu.Lock()
	if _, done := d.finalized[messageID]; done {
		d.progressMu.Unlock()
		return nil
	}
	d.progressMu.Unlock()
	if d.debouncer != nil {
		d.debouncer.enqueue(messageID, card)
		return nil
	}
	return d.bot.UpdateCard(ctx, messageID, card)
}

func (d *Dispatcher) DispatchIncoming(ctx context.Context, msg *feishu.IncomingMessage) error {
	// Stale check precedes dedup so an expired message never enters the
	// dedup table (which would pollute it and suppress a legitimate later
	// redelivery once the SDK's own retry has moved on).
	if d.isStale(msg.CreateTimeMs) {
		return nil
	}
	if !d.eventIDs.Add(msg.EventID) {
		return nil
	}
	// Reject non-text messages before any prompt processing: image/file/post/
	// forwarded content arrives as raw JSON in Content, which would otherwise
	// be forwarded to the backend as a prompt (leaking metadata, confusing the
	// model). Only plain text is supported.
	if msg.MsgType != "" && msg.MsgType != "text" {
		return d.notice(ctx, msg.ChatID, "info", "不支持的消息类型",
			"暂仅支持文本消息，图片/文件/富文本/转发消息暂无法处理")
	}
	prompt := strings.TrimSpace(feishu.StripMentionPlaceholders(msg.Content, msg.Mentions))
	if prompt == "" {
		return nil
	}
	if len(prompt) > maxPromptBytes {
		return d.notice(ctx, msg.ChatID, "warning", "消息过长",
			"消息超过 "+strconv.Itoa(maxPromptBytes>>10)+"KiB 上限，请缩短后重试")
	}
	if cmd, args := parseBackendCommand(prompt); cmd == "/backend" {
		return d.handleBackendCommand(ctx, msg, args)
	}

	// /skill is a frontend wrapper: strip it and tell the bound backend to treat
	// the remaining text as a normal prompt, not as a local slash command.
	skill := false
	if prompt == "/skill" || strings.HasPrefix(prompt, "/skill ") {
		prompt = strings.TrimSpace(strings.TrimPrefix(prompt, "/skill"))
		skill = true
		if prompt == "" {
			return d.notice(ctx, msg.ChatID, "warning", "用法", "/skill <完整指令>")
		}
	}

	if d.router == nil {
		return d.notice(ctx, msg.ChatID, "error", "路由未就绪", "前端路由尚未初始化")
	}
	backendID, err := d.router.Resolve(msg.ChatID)
	if err != nil {
		return d.notice(ctx, msg.ChatID, "error", "路由失败", err.Error())
	}
	backendType := d.registry.BackendType(backendID)
	if backendType == "" {
		return d.notice(ctx, msg.ChatID, "warning", "后端离线",
			"backend "+backendID+" 未连接。请用 /backend list 查看在线后端，或 /backend use {id} 切换。")
	}
	// 5. progress card with "starting" placeholder. Elapsed is empty here:
	// the turn is started only after SendCard returns the messageID, so the
	// first frame (updateProgress) is where elapsed begins to show.
	header := cardkit.HeaderInfo{BackendType: backendType, Title: "处理中", Template: "blue"}
	footer := cardkit.FooterInfo{BackendID: backendID, BackendType: backendType, Status: "处理中"}
	placeholder := renderer.NewProgressState()
	placeholder.AddText("🔄 正在启动…")
	card, err := placeholder.Render(header, footer)
	if err != nil {
		// Nothing durable was established for this message yet: clear the
		// dedup marker so Feishu's redelivery (triggered by our error return)
		// is reprocessed instead of silently dropped.
		d.eventIDs.Delete(msg.EventID)
		return err
	}
	messageID, err := d.bot.SendCard(ctx, msg.ChatID, card, msg.MessageID)
	if err != nil {
		d.eventIDs.Delete(msg.EventID)
		return err
	}
	promptID := msg.MessageID
	d.turns.Start(promptID, msg.ChatID, messageID, backendID)
	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: promptID,
		Prompt: &protocol.PromptPayload{
			ChatID: msg.ChatID,
			Text:   prompt,
			Skill:  skill,
		},
	}
	if err := d.registry.SendEvent(backendID, ev); err != nil {
		d.turns.Finish(promptID)
		return d.notice(ctx, msg.ChatID, "warning", "发送失败", "无法转发到后端: "+err.Error())
	}
	return nil
}

// DispatchControl lives in dispatcher_control.go alongside the sendResult /
// updateProgress / sendInteractive paths it dispatches to.
