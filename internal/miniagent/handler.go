package miniagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/miniclient"
	"github.com/hu/lark-bridge/internal/protocol"
)

// controlSender is the subset of *backendrpc.Client the handler needs.
// Exists so tests substitute a fake capturing Controls instead of POSTing.
type controlSender interface {
	SendControl(ctx context.Context, ctrl *protocol.Control) error
}

// promptCancel is the cancel entry of one in-flight turn, registered under
// its chatID so busy-then-drop and Close can target exactly one chat. Local
// type (mirroring bridgebase.PromptCancel) keeps miniagent independent of the
// bridgebase package, which miniagent otherwise does not use.
type promptCancel struct {
	cancel    context.CancelFunc
	startTime time.Time
}

// closeGrace bounds how long Close waits for in-flight turns to wind down
// after cancelling them. Long enough for a final emit to land, short enough
// that a stuck goroutine does not hang SIGTERM.
const closeGrace = 5 * time.Second

// Handler owns the per-process agent state: the LLM client, the emit
// channel, the loop config derived from config.MiniAgent, and the optional
// per-chat history. One Handler per process; each turn runs on its own
// goroutine.
//
// cancelBy enforces busy-then-drop per chat: a chat with an in-flight turn
// rejects new prompts with a Notice instead of starting a second concurrent
// turn (which would race on the LLM, the history jsonl, and the emit
// ordering). wg tracks runTurn goroutines so Close can wait for them.
type Handler struct {
	llm           Client
	modelLister   ModelLister // for /models + /model picker; nil in CLI mode when no LLM creds at bridge
	cfg           LoopConfig
	rpc           controlSender
	logger        *log.Logger
	history        *History // nil → stateless (MemoryEnabled=false)
	answers        *answerBroker
	workspaceRoot  string // global default for tools + /cd picker scope
	cfgPermission  string // global default permission mode (from config)
	client         *miniclient.Client // non-nil → CLI subprocess mode (P3)
	historyDir     string             // state dir for CLI's --state-dir flag

	cancelMu  sync.Mutex
	cancelBy  map[string]*promptCancel // chatID → in-flight turn
	closed    bool                     // set under cancelMu by Close; rejects new startTurn
	wg        sync.WaitGroup           // tracks runTurn goroutines
	closeOnce sync.Once
}

func New(llm Client, cfg LoopConfig, rpc controlSender, logger *log.Logger, history *History, workspaceRoot string, client *miniclient.Client, cfgPermission string, ml ModelLister) *Handler {
	if logger == nil {
		logger = log.Nop()
	}
	return &Handler{
		llm:           llm,
		modelLister:   ml,
		cfg:           cfg,
		rpc:           rpc,
		logger:        logger,
		history:       history,
		answers:       newAnswerBroker(),
		workspaceRoot: workspaceRoot,
		cfgPermission: cfgPermission,
		client:        client,
		cancelBy:      make(map[string]*promptCancel),
	}
}

// RunningSession describes one in-flight turn for the /running card.
type RunningSession struct {
	ChatID   string
	Duration time.Duration
}

// RunningSessions snapshots all in-flight turns.
func (h *Handler) RunningSessions() []RunningSession {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	now := time.Now()
	out := make([]RunningSession, 0, len(h.cancelBy))
	for chatID, pc := range h.cancelBy {
		out = append(out, RunningSession{ChatID: chatID, Duration: now.Sub(pc.startTime)})
	}
	return out
}

// SetHistoryDir sets the state directory passed to miniagent-cli as
// --state-dir so the CLI subprocess can load/save per-chat history.
func (h *Handler) SetHistoryDir(dir string) {
	h.historyDir = dir
}

// startTurn reserves the per-chat turn slot. Returns (turnCtx, mine, false)
// when the chat already has an in-flight turn (busy-then-drop); the caller
// must NOT touch turnCtx/mine in that case. On success turnCtx is derived
// from the process ctx so Close can cancel it, and the wg is incremented so
// Close waits for this turn.
func (h *Handler) startTurn(ctx context.Context, chatID string) (turnCtx context.Context, mine *promptCancel, ok bool) {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	// After Close, reject new turns so the wg.Wait in Close is not held open
	// by a late HandleEvent that slipped in between cancelAll releasing the
	// lock and the wait starting.
	if h.closed {
		return nil, nil, false
	}
	if _, busy := h.cancelBy[chatID]; busy {
		return nil, nil, false
	}
	turnCtx, cancel := context.WithCancel(ctx)
	mine = &promptCancel{cancel: cancel, startTime: time.Now()}
	h.cancelBy[chatID] = mine
	h.wg.Add(1)
	return turnCtx, mine, true
}

// endTurn releases the per-chat slot only if it still points at mine (a
// later Close or superceding turn may have already cleared it). Always
// decrements wg to match startTurn's Add.
func (h *Handler) endTurn(chatID string, mine *promptCancel) {
	h.cancelMu.Lock()
	if cur, ok := h.cancelBy[chatID]; ok && cur == mine {
		delete(h.cancelBy, chatID)
	}
	h.cancelMu.Unlock()
	h.wg.Done()
}

// Close cancels every in-flight turn and waits up to closeGrace for them to
// wind down so the process does not exit mid-emit / mid-Append. Idempotent.
func (h *Handler) Close() {
	h.closeOnce.Do(func() {
		h.cancelMu.Lock()
		h.closed = true
		for _, pc := range h.cancelBy {
			pc.cancel()
		}
		h.cancelMu.Unlock()
		h.answers.Drain()
		done := make(chan struct{})
		go func() {
			h.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeGrace):
		}
	})
}

// abortChat cancels the in-flight turn for chatID, if any. Returns whether a
// turn was running. It does NOT delete the cancelBy entry: the goroutine that
// owns the slot (startTurn's caller) will endTurn on its own as it unwinds,
// and deleting here would make endTurn's `cur == mine` check fail to clean
// up. Mirrors bridgebase.Core.AbortChat's contract.
func (h *Handler) abortChat(chatID string) bool {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	if pc, ok := h.cancelBy[chatID]; ok {
		pc.cancel()
		return true
	}
	return false
}

// HandleEvent dispatches Prompt events. Each prompt launches runTurn on its
// own goroutine (the SSE event loop must not block on a multi-second LLM
// call). ctx is the process-lifetime ctx from backendrpc.Run; it is NOT
// per-prompt (P0 has no /abort), so a turn only cancels on process shutdown.
// promptID MUST come from ev.PromptID (frontend assigns one per inbound
// message); reusing a chatID-derived id made the 2nd turn's Result collide
// with the 1st's closed card and silently drop.
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	// TypeAbort: the frontend relays a user's stop request. Cancel the
	// in-flight turn for that chat; the owning runTurn goroutine unwinds and
	// emits an "已中止" notice. Non-Prompt, non-Abort events are ignored.
	if ev.Type == protocol.TypeAbort {
		if ev.Abort == nil || ev.Abort.ChatID == "" {
			return nil
		}
		if h.abortChat(ev.Abort.ChatID) {
			h.logger.Info("miniagent abort requested", log.FieldChatID, ev.Abort.ChatID)
		}
		return nil
	}
	// TypeAnswer: the frontend relays a card click from an interactive picker
	// (e.g. /model). Route it to the goroutine blocked in askAndWait.
	if ev.Type == protocol.TypeAnswer {
		if ev.Answer != nil && ev.Answer.RequestID != "" {
			h.answers.Deliver(ev.Answer.RequestID, ev.Answer)
		}
		return nil
	}
	if ev.Type != protocol.TypePrompt || ev.Prompt == nil {
		h.logger.Debug("miniagent ignore non-prompt event", "event_type", ev.Type)
		return nil
	}
	chatID := ev.Prompt.ChatID
	promptID := ev.PromptID
	prompt := strings.TrimSpace(ev.Prompt.Text)
	h.logger.Info("miniagent prompt received",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"prompt_len", len(prompt))
	if chatID == "" {
		return fmt.Errorf("miniagent: prompt missing chatID")
	}
	if promptID == "" {
		return fmt.Errorf("miniagent: prompt missing promptID (frontend must assign one per message)")
	}
	if prompt == "" {
		h.logger.Info("miniagent empty prompt, noticing", log.FieldChatID, chatID)
		return h.notify(ctx, chatID, "warning", "空消息", "请发送需要处理的内容。")
	}

	// /session-abort: cancels the in-flight turn. Handled BEFORE session
	// commands and startTurn because the turn it must cancel is the one
	// currently holding the slot — startTurn would reject us as busy. The
	// aborted turn's runTurn emits the "已中止" notice as it unwinds.
	if prompt == "/session-abort" {
		if h.abortChat(chatID) {
			return h.notify(ctx, chatID, "success", "已请求中止", "正在停止当前任务。")
		}
		return h.notify(ctx, chatID, "info", "无可中止", "当前没有正在执行的任务。")
	}

	// Session management commands (/session-new, /session-list, /session-use,
	// /session-del, /current) are handled inline before the LLM turn and
	// replied to as a Notice. See commands.go.
	if isSessionCommand(prompt) {
		return h.handleSessionCommand(ctx, chatID, prompt)
	}

	// Busy-then-drop: a chat with an in-flight turn gets an immediate Notice
	// instead of a second concurrent goroutine. The latter would race on the
	// history jsonl, double-call the LLM, and emit out-of-order Results.
	turnCtx, mine, ok := h.startTurn(ctx, chatID)
	if !ok {
		h.logger.Info("miniagent prompt dropped: chat busy", log.FieldChatID, chatID, log.FieldPromptID, promptID)
		return h.notify(ctx, chatID, "warning", "处理中",
			"上一条消息还在处理，请等它结束后再发。")
	}
	go func() {
		defer h.endTurn(chatID, mine)
		h.runTurn(turnCtx, promptID, chatID, prompt)
	}()
	return nil
}

// runTurn runs the agent loop and emits the terminal Control. Always emits
// exactly one terminal Control (Result on success, Error on failure) so the
// frontend closes the turn card. A fresh short-lived ctx is used for the
// terminal emit so it still lands after the prompt ctx is cancelled.
// runTurn dispatches one turn: CLI subprocess mode (client != nil) or
// in-process loop mode (client == nil). Both share the same per-chat state
// (session/model/dir) and lifecycle (startTurn/endTurn/abort/close).
func (h *Handler) runTurn(ctx context.Context, promptID, chatID, prompt string) {
	if h.client != nil {
		h.runViaCLI(ctx, promptID, chatID, prompt)
		return
	}
	h.runViaLoop(ctx, promptID, chatID, prompt)
}

// runViaCLI forks miniagent-cli per turn, pumps its NDJSON stdout into
// Controls. The CLI process owns the loop/tools/LLM/memory; the bridge
// owns IPC + per-chat config + command dispatch.
func (h *Handler) runViaCLI(ctx context.Context, promptID, chatID, prompt string) {
	start := time.Now()
	model := h.activeModel(chatID)
	workdir := h.activeDir(chatID)
	perm := h.activePermission(chatID)
	h.logger.Info("miniagent-cli turn start",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"model", model,
		"workdir", workdir,
		"permission", perm)

	events, err := h.client.Run(ctx, miniclient.RunOptions{
		Prompt:     prompt,
		Model:      model,
		Workdir:    workdir,
		ChatID:     chatID,
		StateDir:   h.historyDir,
		Permission: perm,
	})
	if err != nil {
		h.logger.Warn("miniagent-cli start failed",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldError, err)
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeError,
			PromptID: promptID,
			ChatID:   chatID,
			Error:    &protocol.ErrorPayload{Message: "启动 miniagent-cli 失败：" + err.Error(), Recoverable: true},
		})
		return
	}

	var cancelled bool
	for ev := range events {
		if ev.IsTerminal {
			if ev.Kind == miniclient.KindError && ctx.Err() != nil {
				cancelled = true
			}
		}
		h.emitCLIEvent(chatID, promptID, ev, start)
	}
	if cancelled {
		h.logger.Info("miniagent-cli turn aborted",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldDuration, time.Since(start).Milliseconds())
	}
}

// emitCLIEvent translates one miniclient.Event into a protocol.Control and
// emits it to the frontend.
func (h *Handler) emitCLIEvent(chatID, promptID string, ev miniclient.Event, start time.Time) {
	switch ev.Kind {
	case miniclient.KindToolUse:
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeToolUse,
			PromptID: promptID,
			ChatID:   chatID,
			ToolUse:  &protocol.ToolUsePayload{Name: ev.Name, Input: ev.Input},
		})
	case miniclient.KindToolResult:
		h.sendCtrl(&protocol.Control{
			Type:       protocol.TypeToolResult,
			PromptID:   promptID,
			ChatID:     chatID,
			ToolResult: &protocol.ToolResultPayload{Name: ev.Name, Input: ev.Input, Output: ev.Output, IsError: ev.IsError},
		})
	case miniclient.KindResult:
		h.logger.Info("miniagent-cli turn done",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			"steps", ev.Steps,
			"input_tokens", ev.InputTokens,
			"output_tokens", ev.OutputTokens,
			log.FieldDuration, time.Since(start).Milliseconds())
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeResult,
			PromptID: promptID,
			ChatID:   chatID,
			Result: &protocol.ResultPayload{
				Text:        ev.Text,
				Model:       ev.Model,
				Tokens:      ev.InputTokens + ev.OutputTokens,
				Duration:    time.Since(start),
				Steps:       ev.Steps,
				TotalTokens: ev.InputTokens + ev.OutputTokens,
			},
		})
	case miniclient.KindError:
		h.logger.Warn("miniagent-cli turn failed",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			log.FieldError, errors.New(ev.Message),
			log.FieldDuration, time.Since(start).Milliseconds())
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeError,
			PromptID: promptID,
			ChatID:   chatID,
			Error:    &protocol.ErrorPayload{Message: ev.Message, Recoverable: true},
		})
	}
}

// runViaLoop runs the ReAct loop in-process (the original mode before P3).
func (h *Handler) runViaLoop(ctx context.Context, promptID, chatID, prompt string) {
	start := time.Now()
	// Load history for this chat (nil on first turn or when memory is off).
	// The loop sees prior turns and returns the new messages in result.History.
	hist := h.history.Load(chatID)
	// Resolve per-chat overrides: .model pin and .dir pin both override the
	// global defaults. LoopConfig is a value type; cfg.Tools is rebuilt only
	// when dir differs from workspace_root (toolsForDir zero-allocs otherwise).
	cfg := h.cfg
	cfg.Model = h.activeModel(chatID)
	dir := h.activeDir(chatID)
	cfg.Tools = h.toolsForDir(dir)
	h.logger.Info("miniagent turn start",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"model", cfg.Model,
		"workdir", dir,
		"history_msgs", len(hist),
		"prompt_preview", truncate(prompt, 80, "…"))

	result, err := Run(ctx, h.llm, cfg, promptID, prompt, hist, h.emitHook(chatID, promptID), h.logger)
	if err != nil {
		// ctx.Canceled means the turn was aborted (user /session-abort or
		// Close). Surface as an info notice rather than a scary error; the
		// turn produced no History, so nothing is appended (the err path
		// returns before the Append below), keeping aborted turns out of the
		// conversation log.
		if errors.Is(err, context.Canceled) {
			h.logger.Info("miniagent turn aborted",
				log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldDuration, time.Since(start).Milliseconds())
			h.sendCtrl(&protocol.Control{
				Type:     protocol.TypeNotice,
				PromptID: promptID,
				ChatID:   chatID,
				Notice:   &protocol.NoticePayload{Level: "info", Title: "已中止", Message: "本次任务已停止。"},
			})
			return
		}
		h.logger.Warn("miniagent turn failed",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			log.FieldError, err,
			log.FieldDuration, time.Since(start).Milliseconds())
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeError,
			PromptID: promptID,
			ChatID:   chatID,
			Error:    &protocol.ErrorPayload{Message: err.Error(), Recoverable: true},
		})
		return
	}
	h.logger.Info("miniagent turn done",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"steps", result.Steps,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		log.FieldDuration, time.Since(start).Milliseconds())

	// Persist this turn's new messages so the next turn remembers context.
	// The file is append-only (old turns stay on disk); Load trims what the
	// LLM actually sees, so unbounded growth only costs disk, not context.
	h.history.Append(chatID, result.History)

	h.sendCtrl(&protocol.Control{
		Type:     protocol.TypeResult,
		PromptID: promptID,
		ChatID:   chatID,
		Result: &protocol.ResultPayload{
			Text:        result.Text,
			Model:       cfg.Model,
			Tokens:      result.Usage.InputTokens + result.Usage.OutputTokens,
			Duration:    time.Since(start),
			Steps:       result.Steps,
			TotalTokens: result.Usage.InputTokens + result.Usage.OutputTokens,
			SessionID:   h.history.Current(chatID), // "" when memory is off / not yet created
		},
	})
}

// emitHook returns an EmitFunc that turns loop tool signals into frontend
// Controls (TypeToolUse when the LLM asks for a tool, TypeToolResult after
// execution) so the user sees the agent working. Both use the turn's
// promptID so the frontend folds them into the same card. Emits are
// best-effort: a failure is logged but never fails the turn.
func (h *Handler) emitHook(chatID, promptID string) EmitFunc {
	return func(sig Signal) {
		var ctrl *protocol.Control
		switch sig.Kind {
		case SignalToolUse:
			ctrl = &protocol.Control{
				Type:     protocol.TypeToolUse,
				PromptID: promptID,
				ChatID:   chatID,
				ToolUse:  &protocol.ToolUsePayload{Name: sig.Name, Input: sig.Input},
			}
		case SignalToolResult:
			ctrl = &protocol.Control{
				Type:       protocol.TypeToolResult,
				PromptID:   promptID,
				ChatID:     chatID,
				ToolResult: &protocol.ToolResultPayload{Name: sig.Name, Input: sig.Input, Output: sig.Output, IsError: sig.IsError},
			}
		default:
			h.logger.Debug("miniagent unknown signal kind", "kind", sig.Kind)
			return
		}
		h.sendCtrl(ctrl)
	}
}

// sendCtrl emits one Control on a fresh 10s ctx (decoupled from any turn ctx
// so terminal/status emits still land after the turn ctx is cancelled) and
// logs a Warn on failure. chatID/promptID are recorded for triage. Used by
// the terminal Result, tool signals, and error emits; notify is the
// exception since it rides the caller's ctx.
func (h *Handler) sendCtrl(ctrl *protocol.Control) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.rpc.SendControl(ctx, ctrl); err != nil {
		h.logger.Warn("miniagent emit failed",
			log.FieldChatID, ctrl.ChatID, log.FieldPromptID, ctrl.PromptID,
			log.FieldControlType, ctrl.Type, log.FieldError, err)
	}
}

// notify emits a non-terminal Notice (e.g. empty-prompt warning, session
// command reply). It uses a fresh short ctx rather than the caller's so a
// slow IPC POST cannot block the SSE event loop (session commands run inline
// in HandleEvent, on the event-loop goroutine). The ctx param is accepted
// for signature stability but ignored — see sendCtrl for the same pattern.
func (h *Handler) notify(_ context.Context, chatID, level, title, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.rpc.SendControl(ctx, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: message},
	})
}

// activeModel returns the model this chat should use: the per-chat pin
// (from .model file) if set, otherwise the global default (cfg.Model).
func (h *Handler) activeModel(chatID string) string {
	if m := h.history.Model(chatID); m != "" {
		return m
	}
	return h.cfg.Model
}

// activeDir returns the working directory this chat should use: the per-chat
// pin (from .dir file) if set, otherwise the global workspace_root.
func (h *Handler) activeDir(chatID string) string {
	if d := h.history.Directory(chatID); d != "" {
		return d
	}
	return h.workspaceRoot
}

// activePermission returns the permission mode this chat should use: the
// per-chat pin (from .perm file) if set, otherwise the global default.
func (h *Handler) activePermission(chatID string) string {
	if p := h.history.Permission(chatID); p != "" {
		return p
	}
	return h.cfgPermission
}

// toolsForDir returns a Tool slice with WorkspaceRoot-bearing tools cloned
// to use dir instead of the global root. WebFetch (no WorkspaceRoot) is
// passed through. If dir equals workspaceRoot, the original slice is returned
// unchanged (zero-alloc common path).
func (h *Handler) toolsForDir(dir string) []Tool {
	if dir == "" || dir == h.workspaceRoot {
		return h.cfg.Tools
	}
	out := make([]Tool, 0, len(h.cfg.Tools))
	for _, t := range h.cfg.Tools {
		switch v := t.(type) {
		case ReadFile:
			v.WorkspaceRoot = dir
			out = append(out, v)
		case WriteFile:
			v.WorkspaceRoot = dir
			out = append(out, v)
		case EditFile:
			v.WorkspaceRoot = dir
			out = append(out, v)
		case Shell:
			v.WorkspaceRoot = dir
			out = append(out, v)
		default:
			out = append(out, t) // WebFetch etc — no WorkspaceRoot
		}
	}
	return out
}
