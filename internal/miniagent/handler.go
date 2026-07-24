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
	"github.com/justphantom/lark-bridge/internal/router"
)

// controlSender is the subset of *backendrpc.Client the handler needs.
// Exists so tests substitute a fake capturing Controls instead of POSTing.
type controlSender interface {
	SendControl(ctx context.Context, ctrl *protocol.Control) error
}

// Handler owns the per-process bridge state: the IPC sender, the CLI
// subprocess client (one fork per turn), the per-chat binding router, and
// the picker/answer-broker machinery. One Handler per process; each turn
// runs on its own goroutine.
//
// miniagent is stateless: no sessions, no memory, no per-chat jsonl. The
// only persistent per-chat state is the router binding (Directory +
// ModelSpec); both are spliced into the miniagent CLI flags at fork time.
//
// cancelBy enforces busy-then-drop per chat: a chat with an in-flight turn
// rejects new prompts with a Notice instead of starting a second concurrent
// turn (which would race on the LLM and the emit ordering). wg tracks
// runTurn goroutines so Close can wait for them.
type Handler struct {
	rpc             controlSender
	logger          *log.Logger
	router          *router.Router // per-chat Directory/ModelSpec bindings; nil only in tests
	answers         *answerBroker
	workspaceRoot   string             // global default for the /cd picker scope
	cfgModel        string             // global default model (from config)
	client          *miniclient.Client // non-nil → CLI subprocess mode
	pickerPromptIDs sync.Map           // chatID → promptID, for async picker goroutines

	cancelMu  sync.Mutex
	cancelBy  map[string]*promptCancel // chatID → in-flight turn
	closed    bool                     // set under cancelMu by Close; rejects new startTurn
	wg        sync.WaitGroup           // tracks runTurn goroutines
	closeOnce sync.Once
}

// New builds a Handler. rpc emits Controls to the frontend; router holds
// per-chat Directory/ModelSpec bindings (nil acceptable only in tests where
// no slash command runs); client is the per-turn fork runner. cfgModel is
// the global default model used when a chat has no ModelSpec pin;
// workspaceRoot bounds the /cd picker and serves as the global default
// workdir when a chat has no Directory pin.
func New(rpc controlSender, logger *log.Logger, r *router.Router, workspaceRoot, cfgModel string, client *miniclient.Client) *Handler {
	if logger == nil {
		logger = log.Nop()
	}
	return &Handler{
		rpc:           rpc,
		logger:        logger,
		router:        r,
		answers:       newAnswerBroker(),
		workspaceRoot: workspaceRoot,
		cfgModel:      cfgModel,
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
	s, _ := v.(string)
	return s
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
		level, title, body := h.cmdRunning(ctx, chatID, "")
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

	// Slash commands (/current /model /models /cd /help) are handled inline
	// before the LLM turn and replied to as a Notice. See commands.go.
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

// runTurn dispatches one turn to the CLI subprocess. The bridge always runs
// in CLI subprocess mode; if client is nil, the prompt is dropped with a
// notice (misconfiguration: no miniclient wired).
func (h *Handler) runTurn(ctx context.Context, promptID, chatID, prompt string) {
	if h.client == nil {
		h.logger.Error("miniagent: no miniclient wired; dropping turn", log.FieldChatID, chatID, log.FieldPromptID, promptID)
		h.notifyWithPromptID(chatID, promptID, "error", "配置错误", "miniagent 客户端未初始化。")
		return
	}
	h.runViaCLI(ctx, promptID, chatID, prompt)
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

// notifyWithCardUpdate patches an existing card (identified by messageID) via
// the frontend's UpdateCard path, instead of binding to PromptID. Used by
// pickers to turn the picker card into a green/red result frame after the
// user answered — the card the user clicked is the one that should change,
// not the (now-finalised) progress card. Empty messageID falls back to a
// standalone notice, matching notify semantics.
func (h *Handler) notifyWithCardUpdate(chatID, messageID, level, title, message string) {
	h.sendCtrl(&protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: message, UpdateMessageID: messageID},
	})
}
