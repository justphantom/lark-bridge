package claudebridge

import (
	"context"
	"fmt"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/streamarchive"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// runPrompt drives one Claude turn for chatID: it starts a `claude` CLI
// subprocess, streams its events, and emits the terminal control. The
// system/init session id is back-filled onto the binding so the next turn
// resumes it.
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
	// timer + optional idle watchdog). Claude has no idle watchdog (the CLI
	// exits on its own per turn), so idleTimeout=0 makes OnActivity/Stop
	// no-ops. See bridgebase.RunPromptScaffold for the rationale.
	scaffold := h.RunPromptScaffold(parent, 0, nil)
	defer scaffold.Stop()
	defer scaffold.Cancel(nil)
	ctx := scaffold.Ctx

	modelSpec := binding.ModelSpec
	opts := claude.RunOptions{
		Prompt:         prompt,
		Directory:      binding.Directory,
		SessionID:      binding.SessionID,
		Model:          modelSpec,
		PermissionMode: binding.PermissionMode,
		EffortLevel:    binding.EffortLevel,
		SettingsFile:   strutil.ExpandEnvVars(binding.SettingsFile),
	}

	result := h.runClaude(ctx, chatID, replyToID, opts, modelSpec)

	// Stale-session recovery: if --resume hit a session the CLI no longer
	// knows, drop the binding's sessionID and retry once with a fresh session.
	// The stale match itself is centralised in claude.IsStaleSession (set on
	// the result by finalizeResult) so a CLI rewording fixes in one place.
	if result.Err != nil && result.Stale && binding.SessionID != "" &&
		ctx.Err() == nil {
		h.Logger.Warn("stale claude session, retrying without --resume",
			log.FieldChatID, chatID,
			log.FieldSessionID, binding.SessionID)
		h.Router.SetSessionID(chatID, "")
		opts.SessionID = ""
		result = h.runClaude(ctx, chatID, replyToID, opts, modelSpec)
	}

	// RecordUsage before EmitTerminal: EmitTerminal reads the store to fill
	// the cumulative TotalTokens on the result card, so this turn must be
	// counted first. Add is an in-memory map update (the async save is
	// non-blocking), so this does not delay the terminal emit.
	h.RecordUsage(chatID, result)
	if err := h.EmitTerminal(ctx, chatID, replyToID, "Claude", 0, result); err != nil {
		bridgebase.HandleTerminalEmitError(h.Core, ctx, chatID, replyToID, err)
	}
}

// runClaude starts one Claude subprocess, streams its events into Controls,
// and reduces the stream to a bridgebase.PromptResult.
func (h *Handler) runClaude(ctx context.Context, chatID, replyToID string, opts claude.RunOptions, modelSpec string) bridgebase.PromptResult {
	// Archive the raw stream for this run before launching the subprocess so
	// the sink is wired for the whole lifetime. Best-effort: nil sink = off.
	// The sink is wrapped to drop thinking_tokens lines: the bridge never
	// consumes them (event_parse.go classifies them as inert EventSystem) yet
	// they dominate the claude archive volume (~88% of lines), so keeping them
	// bloats replay/debug material with no functional value. closeSink still
	// targets the underlying file; the wrapper is transparent to its lifecycle.
	sink, closeSink := streamarchive.NewSink(h.Logger, h.StateDir, backendTag, chatID, replyToID, h.StreamHistory, h.StreamArchiveRedact)
	if sink != nil {
		opts.LineSink = wrapThinkingFilter(sink)
		defer func() { _ = closeSink() }() // archive already flushed
	}

	events, err := h.agent.Run(ctx, opts)
	if err != nil {
		return bridgebase.PromptResult{
			Err:   fmt.Errorf("启动 Claude 失败: %w", err),
			Model: modelSpec,
		}
	}
	return h.streamRun(ctx, chatID, replyToID, events, modelSpec)
}
