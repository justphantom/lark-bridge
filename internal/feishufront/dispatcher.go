package feishufront

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/feishufront/renderer"
	"github.com/justphantom/lark-bridge/internal/fileconvert"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
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
// card (returns its CardRef identity), update an existing card, or send plain
// text. SendText is the fallback path for when SendCard rejects a result card's
// content (e.g. a reply with too many markdown tables hits Feishu's element
// limit): the reply text is delivered as plain text instead of being lost.
type CardSink interface {
	// SendCard ships a card and returns its CardRef. In legacy mode CardID is
	// empty and later updates address the card by MessageID; in cardkit mode
	// CardID carries the entity id the caller must hand back to UpdateCard.
	SendCard(ctx context.Context, chatID string, card []byte, replyToID string) (feishu.CardRef, error)
	// UpdateCard patches the card. cardID != "" targets the CardKit entity
	// (PUT with per-card sequence); cardID == "" keeps the legacy im PATCH by
	// messageID. Callers in cardkit mode always carry CardID from the CardRef;
	// legacy-mode callers pass "".
	UpdateCard(ctx context.Context, messageID, cardID string, card []byte) error
	// UpdateCardVerified PATCHes the card then read-back confirms the header
	// template persisted, re-PATCHing if Feishu silently reverted it. Used only
	// by the three terminal/submitted delayed-PATCH sites where the click-
	// handling window can roll a PATCH back; streaming progress keeps plain
	// UpdateCard (no click window there).
	UpdateCardVerified(ctx context.Context, messageID, cardID string, card []byte) error
	SendText(ctx context.Context, chatID, text, replyToID string) (string, error)
}

// FileSender is the subset of the Feishu bot needed to materialise a TypeFile
// control: upload bytes as a Feishu file resource and send it to the chat
// (send-file-design.md §3.3/§3.4). Declared as an interface so the dispatcher
// stays testable; production wires *feishu.Bot, whose SendFile uploads then
// sends in two REST round-trips.
type FileSender interface {
	SendFile(ctx context.Context, chatID, fileName string, r io.Reader) error
}

// ChatRouter maps a Feishu chatID to its bound backendID.
type ChatRouter interface {
	Resolve(chatID string) (string, error)
	Set(chatID, backendID string) error
	ChatsOf(backendID string) []string
	Touch(chatID string)
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
	// pendingSubmits caches the submitted ("✓ 已回答") card bytes of a
	// multi-round picker click whose immediate flip was skipped (skipSubmitFlip)
	// because a delayed in-place refresh PATCH is still pending. sendInteractive
	// flushes the entry when the next round's question control arrives for the
	// same card, so the user still sees a submitted echo before the refresh
	// lands; skipSubmitFlip's background timer garbage-collects any entry the
	// next round never claims. Keyed by card messageID; guarded by cardMu.
	pendingSubmits map[string][]byte
	// pickerCards / pickerTimers mirror the interactive-card TTL machinery but
	// for frontend-owned /backend picker cards (which carry no requestID). A
	// picker left unclicked for cardkit.InteractiveTimeout is flipped to a grey
	// "已失效" state; cancelled on the first button click. Guarded by cardMu.
	pickerCards  map[string][]byte
	pickerTimers map[string]*time.Timer
	// pickerCardIDs tracks the CardKit entity id (cardID) behind each /backend
	// picker messageID, so the delayed outcome PATCH can target the entity in
	// cardkit mode. Same lifecycle/lock as pickerCards. Legacy mode leaves it
	// empty (the update falls back to the im PATCH by messageID).
	pickerCardIDs map[string]string

	// flapMu guards flap, the per-backend debounce state for online/offline
	// notices. A flapping backend (rapid disconnect/reconnect) would otherwise
	// spam every bound chat with offline→online card pairs: an offline event
	// arms a timer, and a reconnect before it fires cancels the pending notice
	// silently. Only a backend down for the whole window triggers a notice.
	flapMu sync.Mutex
	flap   map[string]*flapState

	// offlineNoticeDebounce is how long an offline event must persist before a
	// notice is sent. Defaults to the package const; overridable for tests.
	offlineNoticeDebounce time.Duration

	// cardPatchDelay is how long handleBackendChoice waits after a click
	// before PATCHing the picker card (Feishu's click-handling window
	// reverts an immediate PATCH). Defaults to cardPatchDelayDefault;
	// overridable via SetCardPatchDelay from config.
	cardPatchDelay time.Duration

	// maxThinkingRunes caps the progress card's "思考中" zone. 0 lets the
	// renderer use its built-in default. Wired from
	// config.Renderer.MaxThinkingRunes via SetMaxThinkingRunes.
	maxThinkingRunes int

	// statusMu guards statusCards: key = chatID + "\x00" + reportKey → the
	// messageID of the standing overview card for that (chat, status-monitor
	// backend) pair. The status-monitor backend pushes a TypeStatusReport each
	// tick; the dispatcher PATCHes the existing card, or SendCards a new one
	// when none is cached or the prior one was withdrawn (feishu.IsCardGone).
	// statusCardIDs mirrors statusCards with the CardKit entity id (cardID) the
	// same card was sent under; empty in legacy mode. UpdateCard consumes both.
	statusMu      sync.Mutex
	statusCards   map[string]string
	statusCardIDs map[string]string

	// logger is stored atomically: SetLogger runs on the main goroutine while
	// notifyBackendChat reads it from the IPCServer.fireCallback goroutine.
	logger atomic.Pointer[log.Logger]

	// —— 文件上传管线（可选）：仅当 SetFilePipeline 装配后启用 ——
	// fileDownloader 抓取飞书消息资源；fileConverter 把 docx/md/txt 转成 .md。
	// promptTemplate 把单文件上传事件渲染成发给 agent 的 prompt 文本（变量：
	// FileName/Path/UserText）。postPromptTemplate 是富文本消息对应的模板
	// （变量 Path/UserText）。三者任一为零值时，对应 MsgType 走降级路径。
	fileDownloader     FileDownloader
	fileConverter      *fileconvert.Converter
	promptTemplate     *template.Template
	postPromptTemplate *template.Template
	xlsxPromptTemplate *template.Template
	inboxDir           string
	inboxMaxSize       int64

	// fileSender uploads+sends a TypeFile control's bytes to Feishu. nil until
	// SetFileSender wires *feishu.Bot (production); tests that exercise the
	// file-control path inject a stub. A TypeFile arriving with no fileSender
	// is surfaced as an error notice rather than dropped silently.
	fileSender FileSender
}

// FileDownloader is the subset of the Feishu bot needed to pull a binary
// resource attached to a message. Declared as an interface so the dispatcher
// stays testable with a stub; production wires *feishu.Bot, which already
// implements DownloadFile.
type FileDownloader interface {
	DownloadFile(ctx context.Context, messageID, fileKey, fileType string) (io.ReadCloser, error)
}

// SetFilePipeline enables the inbound file-message pipeline. Pass a non-nil
// downloader + converter + a writable inboxDir + a parsed prompt template to
// accept file-type Feishu messages; before this is called (or with nil
// downloader) the dispatcher keeps the legacy "reject non-text" behaviour so
// existing tests/configs are unaffected. maxSize<=0 keeps the fileconvert
// default. The caller owns template parsing (config.Validate already syntax-
// checked it at Load time).
//
// postPromptTemplate may be nil — post messages then fall back to the plain
// "render to Markdown text" path (no body.md, no image download) so an
// operator who only wants single-file uploads can still receive post
// messages without configuring a second template.
func (d *Dispatcher) SetFilePipeline(downloader FileDownloader, converter *fileconvert.Converter, inboxDir string, maxSize int64, promptTemplate *template.Template, postPromptTemplate *template.Template) {
	d.fileDownloader = downloader
	d.fileConverter = converter
	d.inboxDir = inboxDir
	d.inboxMaxSize = maxSize
	d.promptTemplate = promptTemplate
	d.postPromptTemplate = postPromptTemplate
}

// SetXlsxPromptTemplate wires the optional C-paradigm prompt template for xlsx
// uploads (office-extract-design.md §3.2). Kept as a separate setter rather
// than a seventh SetFilePipeline parameter so existing callers (and every
// dispatcher test) are untouched; an unset template means xlsx uploads fall
// back to the generic promptTemplate (path only, no per-sheet schema).
func (d *Dispatcher) SetXlsxPromptTemplate(tmpl *template.Template) {
	d.xlsxPromptTemplate = tmpl
}

// SetFileSender wires the bot's file-send capability used by handleFileControl
// to materialise a TypeFile control (send-file-design.md). Separate setter so
// the dispatcher can be constructed without it for tests that do not exercise
// the send path; a nil fileSender makes every TypeFile an error notice.
func (d *Dispatcher) SetFileSender(fs FileSender) {
	d.fileSender = fs
}

// filePipelineEnabled reports whether the file pipeline is wired for
// single-file uploads (the original path). Kept as a method so the gating
// logic stays next to the fields it reads.
func (d *Dispatcher) filePipelineEnabled() bool {
	return d.fileDownloader != nil && d.fileConverter != nil && d.inboxDir != "" && d.promptTemplate != nil
}

// postPipelineEnabled reports whether the post pipeline is fully wired:
// file pipeline plus a postPromptTemplate. When false but filePipelineEnabled
// is true, post messages degrade to text-only Markdown (方案 B semantics).
func (d *Dispatcher) postPipelineEnabled() bool {
	return d.filePipelineEnabled() && d.postPromptTemplate != nil
}

func NewDispatcher(bot CardSink, registry *BackendRegistry, turns *TurnManager, router ChatRouter) *Dispatcher {
	d := &Dispatcher{
		bot:                   bot,
		registry:              registry,
		turns:                 turns,
		router:                router,
		progress:              make(map[string]*renderer.ProgressState),
		finalized:             make(map[string]struct{}),
		eventIDs:              newDedupSet(eventDedupTTL, eventDedupMaxEntries),
		actionIDs:             newDedupSet(actionDedupTTL, 0),
		terminals:             newDedupSet(terminalDedupTTL, 0),
		cards:                 make(map[string][]byte),
		interactiveTimers:     make(map[string]*time.Timer),
		pendingSubmits:        make(map[string][]byte),
		pickerCards:           make(map[string][]byte),
		pickerTimers:          make(map[string]*time.Timer),
		pickerCardIDs:         make(map[string]string),
		flap:                  make(map[string]*flapState),
		statusCards:           make(map[string]string),
		statusCardIDs:         make(map[string]string),
		offlineNoticeDebounce: offlineNoticeDebounce,
		cardPatchDelay:        cardPatchDelayDefault,
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

// SetCardPatchDelay overrides the post-click PATCH delay used by
// handleBackendChoice. Called by main.go from config
// (timeouts.card_patch_delay); non-positive values keep the default.
func (d *Dispatcher) SetCardPatchDelay(delay time.Duration) {
	if delay > 0 {
		d.cardPatchDelay = delay
	}
}

// SetMaxThinkingRunes overrides the progress card's reasoning-zone cap.
// Called by main.go from config (renderer.max_thinking_runes); non-positive
// values keep the renderer's built-in default.
func (d *Dispatcher) SetMaxThinkingRunes(n int) {
	if n > 0 {
		d.maxThinkingRunes = n
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
		curTTL, curMax := d.eventIDs.Config()
		ttl := eventTTL
		if ttl <= 0 {
			ttl = curTTL
		}
		maxEntries := eventMaxEntries
		if maxEntries <= 0 {
			maxEntries = curMax
		}
		d.eventIDs.Configure(ttl, maxEntries)
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

// StartDedupPrune launches the periodic TTL sweep for all three dedup sets.
// Add no longer scans for expired entries on the hot path (it was O(n) per
// call); this ticker is what bounds the TTL-only sets (actionIDs/terminals)
// in the steady state. eventIDs also benefits but already has maxEntries as
// a backstop. C6 note: configuring maxEntries=0 via SetDedupConfig degrades
// eventIDs to TTL-only — in that mode StartDedupPrune is the ONLY bound on
// its growth, so this call becomes load-bearing, not optional.
// Call once at startup after NewDispatcher; idempotent in effect
// but a second call would spawn a second ticker per set harmlessly.
func (d *Dispatcher) StartDedupPrune(ctx context.Context) {
	d.eventIDs.StartPrune(ctx)
	d.actionIDs.StartPrune(ctx)
	d.terminals.StartPrune(ctx)
}

// updateCard sends (or enqueues) an UpdateCard. When a debouncer is wired,
// progress updates go through it; terminal updates (result/notice) go direct.
// A messageID marked finalized (its terminal card already sent) rejects the
// update so a straggler progress frame can never overwrite the final card.
func (d *Dispatcher) updateCard(ctx context.Context, messageID, cardID string, card []byte) error {
	d.progressMu.Lock()
	if _, done := d.finalized[messageID]; done {
		d.progressMu.Unlock()
		return nil
	}
	d.progressMu.Unlock()
	if d.debouncer != nil {
		d.debouncer.enqueue(messageID, cardID, card)
		return nil
	}
	return d.bot.UpdateCard(ctx, messageID, cardID, card)
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
	switch msg.MsgType {
	case "", "text":
		return d.handleTextMessage(ctx, msg)
	case "file":
		if !d.filePipelineEnabled() {
			return d.notice(ctx, msg.ChatID, "info", "不支持的消息类型",
				"暂仅支持文本消息。如需处理上传的文件，请在配置中启用 file_convert。")
		}
		return d.handleFileMessage(ctx, msg)
	case "post":
		return d.handlePostIncoming(ctx, msg)
	default:
		// image / share / ... content arrives as raw JSON; forwarding it as
		// a prompt would leak metadata and confuse the model. Keep rejected.
		return d.notice(ctx, msg.ChatID, "info", "不支持的消息类型",
			"暂仅支持文本消息、文件（docx/md/txt）与富文本（post）")
	}
}

// handlePostIncoming routes a post-type message: if the file pipeline is
// fully wired (file_convert.enabled + post_prompt_template), run the full
// materialisation path (image downloads + body.md). Otherwise degrade to
// text-only Markdown so the message is never silently lost.
//
// Post==nil means feishu.buildIncomingMessage could not parse the content;
// surface as a notice so the user knows their message was rejected.
func (d *Dispatcher) handlePostIncoming(ctx context.Context, msg *feishu.IncomingMessage) error {
	if msg.Post == nil {
		return d.notice(ctx, msg.ChatID, "warning", "解析失败",
			"无法解析富文本消息，请改用文本或重新发送")
	}
	if d.postPipelineEnabled() {
		return d.handlePostMessage(ctx, msg)
	}
	// Degraded path: no inbox, no image download. Render to plain Markdown
	// and forward as a text prompt. This keeps post useful for deployments
	// that have not enabled file_convert.
	body := d.renderPostBodyAsTextOnly(msg)
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if len(body) > maxPromptBytes {
		return d.notice(ctx, msg.ChatID, "warning", "消息过长",
			"消息超过 "+strconv.Itoa(maxPromptBytes>>10)+"KiB 上限，请缩短后重试")
	}
	return d.dispatchPrompt(ctx, msg, body, false)
}

// handleTextMessage handles the legacy text-prompt path: @-strip → /backend or
// /skill resolution → dispatchPrompt. Extracted from DispatchIncoming when
// file-type support was added; behaviour is unchanged.
func (d *Dispatcher) handleTextMessage(ctx context.Context, msg *feishu.IncomingMessage) error {
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
	return d.dispatchPrompt(ctx, msg, prompt, skill)
}

// dispatchPrompt resolves the bound backend, sends the placeholder progress
// card, and forwards the prompt as a Prompt Event. Shared by the text path
// and the file pipeline so they emit identical turn state + IPC framing for
// the backend to consume.
//
// prompt must already be @-stripped and length-checked by the caller.
func (d *Dispatcher) dispatchPrompt(ctx context.Context, msg *feishu.IncomingMessage, prompt string, skill bool) error {
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
			"backend "+backendID+" 未连接。请用 /backend 重新选择在线后端。")
	}
	// 5. progress card with "starting" placeholder. Body starts empty: the
	// title "处理中" + footer status convey the state, and the first event
	// (SessionInit/Progress) arrives shortly to populate the tool zones.
	// Elapsed is empty here: the turn is started only after SendCard returns
	// the messageID, so the first frame (updateProgress) is where elapsed
	// begins to show.
	header := cardkit.HeaderInfo{BackendType: backendType, Title: "处理中", Template: "blue"}
	footer := cardkit.FooterInfo{BackendID: backendID, BackendType: backendType, Status: "处理中"}
	placeholder := renderer.NewProgressState()
	placeholder.SetMaxThinkingRunes(d.maxThinkingRunes)
	card, err := placeholder.Render(header, footer)
	if err != nil {
		// Nothing durable was established for this message yet: clear the
		// dedup marker so Feishu's redelivery (triggered by our error return)
		// is reprocessed instead of silently dropped.
		d.eventIDs.Delete(msg.EventID)
		return err
	}
	ref, err := d.bot.SendCard(ctx, msg.ChatID, card, msg.MessageID)
	if err != nil {
		d.eventIDs.Delete(msg.EventID)
		return err
	}
	messageID := ref.MessageID
	promptID := msg.MessageID
	d.turns.Start(promptID, msg.ChatID, messageID, ref.CardID, backendID)
	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: promptID,
		Prompt: &protocol.PromptPayload{
			ChatID: msg.ChatID,
			Text:   prompt,
			Skill:  skill,
			// Lets a long-running backend (deploy-monitor) patch THIS card
			// for its terminal notice even if the frontend restarts
			// mid-job and the turn map is gone.
			CardMessageID: messageID,
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
