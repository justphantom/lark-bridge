package ompbridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/streamarchive"
)

// runPrompt drives one omp turn for chatID: it starts an `omp` CLI subprocess,
// streams its events, and emits the terminal control. The `session` header's
// session id is back-filled onto the binding so the next turn resumes it.
func (h *Handler) runPrompt(parent context.Context, chatID string, binding router.Binding, prompt, replyToID string, mine *bridgebase.PromptCancel) {
	// Recover so a panic in this goroutine never crashes the process.
	defer func() { bridgebase.RecoverPromptPanic(h.Core, chatID, replyToID, recover()) }()
	// Mark the prompt done after endPrompt/cancel unwind (LIFO) and before the
	// recover above, so Close's waitPrompts unblocks only when the goroutine
	// has fully released its slot — including the subprocess kill on cancel.
	defer h.Wg.Done()
	defer h.EndPrompt(chatID, mine)
	defer mine.Cancel()

	// Re-read the binding here rather than trusting the snapshot the caller
	// took in handlePromptEvent: a concurrent /cd, /session-del or /model
	// command (run in a separate goroutine) could have mutated the router
	// between ensureBinding and this point. Fall back to the passed snapshot
	// only if the binding was removed entirely.
	if fresh, ok := h.Router.Lookup(chatID); ok {
		binding = fresh
	}

	h.Logger.Debug("runPrompt start",
		log.FieldChatID, chatID,
		log.FieldSessionID, binding.SessionID,
		"prompt", bridgebase.TruncateForDebug(prompt, h.DebugRedact()))

	// Wire the shared per-prompt prologue (WithCancelCause + PromptTimeout
	// timer + idle watchdog). If the CLI goes silent for IdleTimeout the
	// timer fires Cancel(errIdleTimeout), which SIGKILLs the process group
	// (ApplyGroupCancel) so streamRun unblocks and returns IsIdleTimeout —
	// the user sees a "响应超时" notice instead of waiting forever on a
	// stuck subprocess.
	scaffold := h.RunPromptScaffold(parent, h.IdleTimeout, errIdleTimeout)
	defer scaffold.Stop()
	defer scaffold.Cancel(nil)
	ctx := scaffold.Ctx
	onActivity := scaffold.OnActivity

	modelSpec := binding.ModelSpec
	opts := omp.RunOptions{
		Prompt:        prompt,
		Directory:     binding.Directory,
		SessionID:     binding.SessionID,
		Model:         modelSpec,
		ApprovalMode:  mapApprovalMode(binding.PermissionMode, h.PermissionDefault),
		ThinkingLevel: mapThinkingLevel(binding.EffortLevel, h.thinkingDefault),
	}

	result := h.runOMP(ctx, chatID, replyToID, opts, modelSpec, onActivity)

	// Stale-session recovery (§10.7): if --resume hit a session the CLI no
	// longer knows, drop the binding's sessionID and retry once with a fresh
	// session. The exact报文 was confirmed empirically against omp/17.1.8:
	// a bad --resume id makes omp exit 1 with empty stdout and stderr
	// `Error: Session "<id>" not found.`, which the client's pump surfaces
	// as a synthesised EventError whose text contains "Session" + "not found".
	// isStaleSessionErr matches that signature so a real error (403, network,
	// …) with a non-empty SessionID does NOT trigger a spurious retry.
	if result.Err != nil && !result.IsCancelled && !result.IsIdleTimeout &&
		binding.SessionID != "" && ctx.Err() == nil && isStaleSessionErr(result.Err) {
		h.Logger.Warn("stale omp session, retrying without --resume",
			log.FieldChatID, chatID,
			log.FieldSessionID, binding.SessionID,
			log.FieldError, result.Err)
		h.Router.SetSessionID(chatID, "")
		opts.SessionID = ""
		result = h.runOMP(ctx, chatID, replyToID, opts, modelSpec, onActivity)
	}

	// RecordUsage before EmitTerminal: EmitTerminal reads the store to fill
	// the cumulative TotalTokens on the result card, so this turn must be
	// counted first. Add is an in-memory map update (the async save is
	// non-blocking), so this does not delay the terminal emit.
	h.RecordUsage(chatID, result)
	if err := h.EmitTerminal(ctx, chatID, replyToID, "omp", int(h.IdleTimeout.Seconds()), result); err != nil {
		bridgebase.HandleTerminalEmitError(h.Core, ctx, chatID, replyToID, err)
	}
}

// runOMP starts one omp subprocess, streams its events into Controls, and
// reduces the stream to a bridgebase.PromptResult. onActivity is wired through to
// streamRun so the idle watchdog in runPrompt resets per event.
func (h *Handler) runOMP(ctx context.Context, chatID, promptID string, opts omp.RunOptions, modelSpec string, onActivity func()) bridgebase.PromptResult {
	// Archive the raw stream for this run before launching the subprocess so
	// the sink is wired for the whole lifetime. Best-effort: nil sink = off.
	sink, closeSink := streamarchive.NewSink(h.Logger, h.StateDir, "omp", chatID, promptID, h.StreamHistory, h.StreamArchiveRedact)
	if sink != nil {
		opts.LineSink = sink
		defer func() { _ = closeSink() }() // archive already flushed
	}

	events, err := h.agent.Run(ctx, opts)
	if err != nil {
		return bridgebase.PromptResult{
			Err:   fmt.Errorf("启动 omp 失败: %w", err),
			Model: bridgebase.ResolveModel("", modelSpec, "omp"),
		}
	}
	return h.streamRun(ctx, chatID, promptID, events, modelSpec, onActivity)
}

// mapApprovalMode resolves the omp --approval-mode value from the chat's
// permission pin. OMP's native values are always-ask|write|yolo; the /perm
// picker offers those directly, so binding.PermissionMode is usually already
// omp-native. The claude-name aliases (bypassPermissions/acceptEdits/plan)
// are mapped defensively so a config or binding migrated from claude-back
// still produces a valid omp flag (§7.2). Empty falls back to def
// (PermissionDefault, itself omp-native from cfg.OMP.ApprovalMode).
func mapApprovalMode(pin, def string) string {
	switch pin {
	case "bypassPermissions":
		return "yolo"
	case "acceptEdits":
		return "write"
	case "plan":
		return "always-ask"
	case "":
		return def
	default:
		// omp-native (always-ask/write/yolo) or an operator-customised
		// value: pass through verbatim.
		return pin
	}
}

// mapThinkingLevel resolves the omp --thinking value. claude's effort values
// (low/medium/high/xhigh/max) are accepted by omp verbatim; omp additionally
// allows off/minimal/auto. Empty pin falls back to def (thinkingDefault),
// then to "auto" (§7.2).
func mapThinkingLevel(pin, def string) string {
	if pin != "" {
		return pin
	}
	if def != "" {
		return def
	}
	return "auto"
}

// isStaleSessionErr reports whether err looks like omp's "session not found"
// failure on a bad --resume. Confirmed signature (omp/17.1.8, exit 1, empty
// stdout): stderr is `Error: Session "<id>" not found.`, which the client's
// pump rolls into the synthesised EventError text alongside the exit status.
// Matching on "session" + "not found" (case-insensitive) keeps this stable
// across id values and the surrounding "Run `omp --resume` …" hint without
// over-matching unrelated errors (a 403 / network error never contains both).
func isStaleSessionErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "session") && strings.Contains(s, "not found")
}
