package miniagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/streamarchive"
)

// runViaCLI forks miniagent per turn, pumps its NDJSON stdout into
// Controls. The CLI process owns the loop/tools/LLM call; the bridge
// owns IPC + per-chat binding (Directory/ModelSpec) + command dispatch.
func (h *Handler) runViaCLI(ctx context.Context, promptID, chatID, prompt string) {
	start := time.Now()
	model, workdir, mode, thinking, config := h.activeTurnConfig(chatID)
	maxIter := h.activeMaxIter(chatID)
	h.logger.Info("miniagent turn start",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"model", model,
		"workdir", workdir)

	var sink io.Writer
	var closeSink func() error
	if h.streamHistory > 0 && h.stateDir != "" {
		s, c := streamarchive.NewSink(h.logger, h.stateDir, "miniagent", chatID, promptID, h.streamHistory, h.archiveRedact)
		sink = s
		closeSink = c
	}
	if closeSink != nil {
		// Closed after the events channel is drained below: the miniclient
		// pump writes to sink and closes the channel before exiting, so by
		// then no writer remains.
		defer func() { _ = closeSink() }()
	}

	// Per-chat session (v4.0.1+): resume if we have a persisted id, else
	// -save-session to create one (miniagent emits the id as a KindSession
	// event we persist on arrival). Stateless when sessionRoot is unset.
	sid := h.lookupSessionID(chatID)
	events, err := h.client.Run(ctx, miniclient.RunOptions{
		Prompt:        prompt,
		Model:         model,
		Workdir:       workdir,
		Mode:          mode,
		Thinking:      thinking,
		MaxIterations: maxIter,
		ConfigPath:    config,
		Session:       sid,
		SaveSession:   h.sessionRoot != "" && sid == "",
		Sink:          sink,
	})
	if err != nil {
		h.logger.Warn("miniagent start failed",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldError, err)
		h.sendTerminalCtrl(&protocol.Control{
			Type:     protocol.TypeError,
			PromptID: promptID,
			ChatID:   chatID,
			Error:    &protocol.ErrorPayload{Message: "启动 miniagent 失败：" + err.Error(), Recoverable: true},
		})
		return
	}

	var emittedTerminal bool
	for ev := range events {
		if ev.IsTerminal {
			emittedTerminal = true
		}
		h.emitCLIEvent(chatID, promptID, ev, start)
	}
	// If the CLI was killed by abort/close before emitting a terminal event,
	// the miniclient pump synthesizes a KindError — but the user should see
	// a friendly "已中止" notice, not a scary TypeError.
	if ctx.Err() != nil && !emittedTerminal {
		h.logger.Info("miniagent turn aborted (no terminal event)",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldDuration, time.Since(start).Milliseconds())
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeNotice,
			PromptID: promptID,
			ChatID:   chatID,
			Notice:   &protocol.NoticePayload{Level: "info", Title: "已中止", Message: "本次任务已停止。"},
		})
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
		h.emitToolResult(chatID, promptID, ev)
	case miniclient.KindReasoningDelta:
		// Streaming reasoning increment → live "思考中" zone. Append mode
		// (Replace=false): each delta is a chunk, the renderer caps the
		// trailing runes shown. text_delta is intentionally NOT forwarded:
		// the frontend dispatcher drops TypeText (live text preview was
		// removed; the full reply arrives in the terminal result event), so
		// emitting it would be a dead signal.
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeThinking,
			PromptID: promptID,
			ChatID:   chatID,
			Thinking: &protocol.ThinkingPayload{Delta: ev.Text},
		})
	case miniclient.KindResult:
		incomplete := ev.Finish == miniclient.FinishMaxIterations
		text := ev.Text
		// max_iterations leaves Text empty; without a hint the user sees a blank
		// green "已完成" card and cannot tell a truncated turn from a real empty
		// reply. Surface a reason and flip Incomplete so the card renders as
		// "未完成" (orange) via RenderResult.
		if incomplete && text == "" {
			text = "已达到最大推理步数，未产出最终回答。可尝试拆分任务或细化问题后重试。"
		}
		turnDur := time.Since(start)
		h.logger.Info("miniagent turn done",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			"steps", ev.Steps,
			"finish", ev.Finish,
			"incomplete", incomplete,
			"input_tokens", ev.InputTokens,
			"output_tokens", ev.OutputTokens,
			log.FieldDuration, turnDur.Milliseconds())
		// Per-turn metrics: surface turn duration, token counts, and
		// completion status for SLO aggregation (P1).
		eventmetrics.MiniAgentTurnCount.Inc()
		eventmetrics.MiniAgentTurnDurationMs.Add(turnDur.Milliseconds())
		eventmetrics.MiniAgentTurnInputTokens.Add(int64(ev.InputTokens))
		eventmetrics.MiniAgentTurnOutputTokens.Add(int64(ev.OutputTokens))
		if incomplete {
			eventmetrics.MiniAgentTurnIncomplete.Inc()
		}
		h.sendTerminalCtrl(&protocol.Control{
			Type:     protocol.TypeResult,
			PromptID: promptID,
			ChatID:   chatID,
			Result: &protocol.ResultPayload{
				Text:        text,
				Model:       ev.Model,
				Tokens:      ev.InputTokens + ev.OutputTokens,
				Duration:    time.Since(start),
				Steps:       ev.Steps,
				TotalTokens: ev.InputTokens + ev.OutputTokens,
				Incomplete:  incomplete,
			},
		})
	case miniclient.KindError:
		h.logger.Warn("miniagent turn failed",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			log.FieldError, errors.New(ev.Message),
			log.FieldDuration, time.Since(start).Milliseconds())
		h.sendTerminalCtrl(&protocol.Control{
			Type:     protocol.TypeError,
			PromptID: promptID,
			ChatID:   chatID,
			Error:    &protocol.ErrorPayload{Message: ev.Message, Recoverable: true},
		})
	case miniclient.KindSession:
		// -save-session turn: miniagent emitted the freshly-generated session
		// id as stdout NDJSON (v4.0.1+). Persist the chatID→id mapping so the
		// next turn resumes via -session. Not forwarded to the frontend — this
		// is internal bridge state, not a user-visible event.
		h.saveSessionID(chatID, ev.SessionID)
		h.logger.Debug("miniagent session created",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			"session_id", ev.SessionID)
	}
}

// emitToolResult translates a v2.0.0 tool_result event into a TypeToolResult
// control. The miniagent CLI truncates the full result to a 2000-char excerpt
// for the event (the complete output stays in its history for LLM re-feed);
// Truncated is surfaced as a suffix so the user knows there was more.
//
// The v2.0.0 breaking change lands here: the shell tool reports a non-zero
// exit as exit_code with is_error=false (a legitimate command result, not an
// execution failure). To preserve "did the command fail?" without a protocol
// change (decision D2), the exit code is prepended to Output ([exit N]) and
// IsError is set only for exit_code<0 (the CLI's timeout/startup-failure
// sentinel). Non-shell tools keep the CLI's is_error verbatim.
func (h *Handler) emitToolResult(chatID, promptID string, ev miniclient.Event) {
	output := ev.Output
	isErr := ev.IsError
	if ev.Name == "shell" && ev.ExitCode != nil {
		code := *ev.ExitCode
		if code != 0 {
			prefix := "[exit " + strconv.Itoa(code) + "]"
			if output == "" {
				output = prefix
			} else {
				output = prefix + " " + output
			}
		}
		isErr = code < 0
	}
	if ev.Truncated {
		const tag = "…（输出已截断）"
		if output == "" {
			output = tag
		} else {
			output += "\n" + tag
		}
	}
	h.sendCtrl(&protocol.Control{
		Type:       protocol.TypeToolResult,
		PromptID:   promptID,
		ChatID:     chatID,
		ToolResult: &protocol.ToolResultPayload{Name: ev.Name, Output: output, IsError: isErr},
	})
}

// clientDefaultMode is the global -mode fallback (config.MiniAgent.Mode via
// miniclient), used when a chat has no per-chat Mode pin. "default" when the
// client is nil (tests).
func (h *Handler) clientDefaultMode() string {
	if h.client != nil {
		return h.client.DefaultMode()
	}
	return "default"
}

// clientDefaultThinking is the global -thinking fallback. "off" when the
// client is nil (tests).
func (h *Handler) clientDefaultThinking() string {
	if h.client != nil {
		return h.client.DefaultThinking()
	}
	return "off"
}

// clientDefaultConfig is the global -config fallback (the path main.go resolved
// at startup via config.ResolveConfigPath and pinned on the client), used when
// a chat has no per-chat ConfigFile pin. "" when the client is nil (tests).
func (h *Handler) clientDefaultConfig() string {
	if h.client != nil {
		return h.client.DefaultConfigPath()
	}
	return ""
}

// activeConfig returns the -config path the CLI would be invoked with for this
// chat (used by /current display). Same precedence as activeTurnConfig.
func (h *Handler) activeConfig(chatID string) string {
	if h.router != nil {
		if b, ok := h.router.Lookup(chatID); ok && b.ConfigFile != "" {
			return b.ConfigFile
		}
	}
	return h.clientDefaultConfig()
}

// activeTurnConfig returns the (model, workdir, mode, thinking) the CLI
// subprocess should be invoked with for this chat. Per-chat binding fields
// (router.Lookup) win; empty fields fall back to the bridge's global defaults
// from config.
//
// When no binding exists the globals are returned directly — the binding is
// created lazily by /model, /cd, /mode or /thinking, not by the first prompt
// (miniagent has no session to seed).
func (h *Handler) activeTurnConfig(chatID string) (model, workdir, mode, thinking, config string) {
	model = h.cfgModel
	workdir = h.workspaceRoot
	mode = h.clientDefaultMode()
	thinking = h.clientDefaultThinking()
	config = h.clientDefaultConfig()
	if h.router == nil {
		return model, workdir, mode, thinking, config
	}
	b, ok := h.router.Lookup(chatID)
	if !ok {
		return model, workdir, mode, thinking, config
	}
	if b.ModelSpec != "" {
		model = b.ModelSpec
	}
	if b.Directory != "" {
		workdir = b.Directory
	}
	if b.Mode != "" {
		mode = b.Mode
	}
	if b.Thinking != "" {
		thinking = b.Thinking
	}
	if b.ConfigFile != "" {
		config = b.ConfigFile
	}
	return model, workdir, mode, thinking, config
}

// activeModel returns the model the CLI would be invoked with for this chat
// (used by /current and /models display). Same precedence as activeTurnConfig.
func (h *Handler) activeModel(chatID string) string {
	if h.router != nil {
		if b, ok := h.router.Lookup(chatID); ok && b.ModelSpec != "" {
			return b.ModelSpec
		}
	}
	return h.cfgModel
}

// activeDir returns the workdir the CLI would be invoked with for this chat
// (used by /current display).
func (h *Handler) activeDir(chatID string) string {
	if h.router != nil {
		if b, ok := h.router.Lookup(chatID); ok && b.Directory != "" {
			return b.Directory
		}
	}
	return h.workspaceRoot
}

// activeMode returns the -mode the CLI would be invoked with for this chat
// (used by /current and /mode display). Same precedence as activeTurnConfig.
func (h *Handler) activeMode(chatID string) string {
	if h.router != nil {
		if b, ok := h.router.Lookup(chatID); ok && b.Mode != "" {
			return b.Mode
		}
	}
	return h.clientDefaultMode()
}

// activeThinking returns the -thinking the CLI would be invoked with for this
// chat (used by /current and /thinking display). Same precedence as
// activeTurnConfig.
func (h *Handler) activeThinking(chatID string) string {
	if h.router != nil {
		if b, ok := h.router.Lookup(chatID); ok && b.Thinking != "" {
			return b.Thinking
		}
	}
	return h.clientDefaultThinking()
}

// activeMaxIter returns the -max-iterations the CLI would be invoked with for
// this chat (used by /current, /maxiter display, and runViaCLI). Same precedence
// as activeTurnConfig: per-chat pin (>0) > client default. 0 means "do not pass
// the flag" (upstream CLI default of 20).
func (h *Handler) activeMaxIter(chatID string) int {
	if h.router != nil {
		if b, ok := h.router.Lookup(chatID); ok && b.MaxIterations > 0 {
			return b.MaxIterations
		}
	}
	return h.clientDefaultMaxIter()
}

// clientDefaultMaxIter is the global -max-iterations fallback (config.
// MiniAgent.MaxIterations via miniclient). 0 when the client is nil (tests) or
// the operator left it unset.
func (h *Handler) clientDefaultMaxIter() int {
	if h.client != nil {
		return h.client.DefaultMaxIterations()
	}
	return 0
}

// ensureBinding returns the binding for chatID, creating one on first use.
// Required because Router.SetModelSpec/SetDirectory are no-ops on missing
// bindings; /model and /cd must create the binding before mutating it.
func (h *Handler) ensureBinding(chatID string) {
	if h.router == nil {
		return
	}
	if _, ok := h.router.Lookup(chatID); ok {
		return
	}
	h.router.Bind(chatID, "", "", "", "", "")
}

// sessionIDFile returns the path to chatID's session-id mapping file, or ""
// when no session root is configured (stateDir unset, e.g. some tests) →
// runViaCLI runs the turn stateless. The file holds the miniagent-generated
// session id (one line, no extension) so a later turn resumes via -session.
//
// chatID is external input (Feishu chat id), so it is hashed (sha256 hex) —
// never concatenated raw — to prevent path traversal/collision under
// sessionRoot. Same-chat write serialisation is guaranteed by startTurn
// busy-then-drop (R4), not here. The session jsonl itself lives under
// miniagent's own session.dir (the bridge does NOT configure session.dir) and
// is managed by miniagent; this file is only the chatID→id indirection.
func (h *Handler) sessionIDFile(chatID string) string {
	if h.sessionRoot == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(chatID))
	return filepath.Join(h.sessionRoot, hex.EncodeToString(sum[:])+".id")
}

// lookupSessionID returns the persisted miniagent session id for chatID, or ""
// if none yet (first turn for this chat, or mapping file missing/corrupt). A
// corrupt or non-whitelist id is treated as "no session" so the turn creates a
// fresh one via -save-session instead of failing upstream on a bad id.
func (h *Handler) lookupSessionID(chatID string) string {
	p := h.sessionIDFile(chatID)
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(b))
	if !validSessionID(id) {
		return ""
	}
	return id
}

// saveSessionID persists the miniagent-generated session id for chatID, called
// when a KindSession event arrives (only on -save-session turns). Overwrites
// any prior mapping. Same-chat serialisation (startTurn busy-then-drop, R4)
// means this never races another write for the same chatID.
func (h *Handler) saveSessionID(chatID, id string) {
	p := h.sessionIDFile(chatID)
	if p == "" || !validSessionID(id) {
		return
	}
	if err := os.WriteFile(p, []byte(id), 0o600); err != nil {
		h.logger.Warn("miniagent: persist session id failed",
			log.FieldChatID, chatID, log.FieldPath, p, log.FieldError, err)
	}
}

// validSessionID mirrors miniagent's id whitelist (latin letters/digits/'-')
// so a corrupt or tampered mapping file is caught locally rather than failing
// upstream. Matches miniagent's ValidateSessionID.
func validSessionID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}
