package miniagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// controlSender is the subset of *backendrpc.Client the handler needs.
// Exists so tests substitute a fake capturing Controls instead of POSTing.
type controlSender interface {
	SendControl(ctx context.Context, ctrl *protocol.Control) error
}

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
	llm             Client
	modelLister     ModelLister // for /models + /model picker; nil in CLI mode when no LLM creds at bridge
	cfg             LoopConfig
	rpc             controlSender
	logger          *log.Logger
	history         *History  // nil → stateless (MemoryEnabled=false)
	facts           FactStore // nil → long-term memory disabled
	answers         *answerBroker
	workspaceRoot   string             // global default for tools + /cd picker scope
	cfgPermission   string             // global default permission mode (from config)
	client          *miniclient.Client // non-nil → CLI subprocess mode (P3)
	historyDir      string             // state dir for CLI's --state-dir flag
	pickerPromptIDs sync.Map           // chatID → promptID, for async picker goroutines

	cancelMu  sync.Mutex
	cancelBy  map[string]*promptCancel // chatID → in-flight turn
	closed    bool                     // set under cancelMu by Close; rejects new startTurn
	wg        sync.WaitGroup           // tracks runTurn goroutines
	closeOnce sync.Once
}

func New(llm Client, cfg LoopConfig, rpc controlSender, logger *log.Logger, history *History, facts FactStore, workspaceRoot string, client *miniclient.Client, cfgPermission string, ml ModelLister) *Handler {
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
		facts:         facts,
		answers:       newAnswerBroker(),
		workspaceRoot: workspaceRoot,
		cfgPermission: cfgPermission,
		client:        client,
		cancelBy:      make(map[string]*promptCancel),
	}
}

// SetPromptIDForPickers stores the promptID for a chat so async picker
// goroutines can find it later. Keyed by chatID for concurrency safety
// (multiple chats can run pickers simultaneously).
func (h *Handler) SetPromptIDForPickers(chatID, promptID string) {
	if promptID == "" {
		h.pickerPromptIDs.Delete(chatID)
	} else {
		h.pickerPromptIDs.Store(chatID, promptID)
	}
}

func (h *Handler) PromptIDForPickers(chatID string) string {
	v, ok := h.pickerPromptIDs.Load(chatID)
	if !ok {
		return ""
	}
	return v.(string)
}

// SetHistoryDir sets the state directory passed to miniagent as
// --state-dir so the CLI subprocess can load/save per-chat history.
func (h *Handler) SetHistoryDir(dir string) {
	h.historyDir = dir
}

// HandleEvent dispatches Prompt events. Each prompt launches runTurn on its
// own goroutine (the SSE event loop must not block on a multi-second LLM
// call). ctx is the process-lifetime ctx from backendrpc.Run; per-prompt
// cancellation arrives via TypeAbort, which targets one chat's turn ctx.
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
		h.notifyWithPromptID(chatID, promptID, "warning", "空消息", "请发送需要处理的内容。")
		return nil
	}

	// /running: read-only query for in-flight turns. Handled before session
	// commands so it does not itself occupy a turn slot (which would make it
	// appear as a running session).
	if prompt == "/running" {
		level, title, body := h.cmdRunning(chatID, "")
		h.notifyWithPromptID(chatID, promptID, level, title, body)
		return nil
	}

	// /session-abort: cancels the in-flight turn. Handled BEFORE session
	// commands and startTurn because the turn it must cancel is the one
	// currently holding the slot — startTurn would reject us as busy. The
	// aborted turn's runTurn emits the "已中止" notice as it unwinds.
	if prompt == "/session-abort" {
		if h.abortChat(chatID) {
			h.notifyWithPromptID(chatID, promptID, "success", "已请求中止", "正在停止当前任务。")
		} else {
			h.notifyWithPromptID(chatID, promptID, "info", "无可中止", "当前没有正在执行的任务。")
		}
		return nil
	}

	// Session management commands (/session-new, /session-list, /session-use,
	// /session-del, /current) are handled inline before the LLM turn and
	// replied to as a Notice. See commands.go.
	if isSessionCommand(prompt) {
		return h.handleSessionCommand(ctx, chatID, promptID, prompt)
	}

	// Busy-then-drop: a chat with an in-flight turn gets an immediate Notice
	// instead of a second concurrent goroutine. The latter would race on the
	// history jsonl, double-call the LLM, and emit out-of-order Results.
	turnCtx, mine, ok := h.startTurn(ctx, chatID)
	if !ok {
		h.logger.Info("miniagent prompt dropped: chat busy", log.FieldChatID, chatID, log.FieldPromptID, promptID)
		h.notifyWithPromptID(chatID, promptID, "warning", "处理中",
			"上一条消息还在处理，请等它结束后再发。")
		return nil
	}
	go func() {
		defer h.endTurn(chatID, mine)
		h.runTurn(turnCtx, promptID, chatID, prompt)
	}()
	return nil
}

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
		// PromptID intentionally empty: notify is used for both prompt-bound
		// replies (where the frontend replaces the progress card) and
		// standalone notices (abort ack, picker async replies) that have no
		// progress card to replace. Callers that want replacement should use
		// notifyWithPromptID instead.
	})
}

// notifyWithPromptID is like notify but sets PromptID so the frontend can
// replace the progress card (the "🔄 正在启动…" placeholder) in place via
// UpdateCard, instead of leaving the stale card and sending a new one.
func (h *Handler) notifyWithPromptID(chatID, promptID, level, title, message string) {
	h.sendCtrl(&protocol.Control{
		Type:     protocol.TypeNotice,
		PromptID: promptID,
		ChatID:   chatID,
		Notice:   &protocol.NoticePayload{Level: level, Title: title, Message: message},
	})
}
