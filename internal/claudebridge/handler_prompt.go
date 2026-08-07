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

	binding = h.resolveRunBinding(chatID, binding)

	h.Logger.Debug("runPrompt start",
		log.FieldChatID, chatID,
		log.FieldSessionID, binding.SessionID,
		"prompt", bridgebase.TruncateForDebug(prompt, h.DebugRedact()))

	// Claude has no idle watchdog (the CLI exits on its own per turn), so
	// idleTimeout=0 makes onActivity a no-op. RunPrompt wires the shared
	// per-prompt prologue (WithCancelCause + PromptTimeout timer) and
	// guarantees the timer teardown order.
	err := h.RunPrompt(parent, 0, nil, func(ctx context.Context, _ func()) error {
		result := h.runClaudeWithStaleRetry(ctx, chatID, replyToID, binding, prompt)

		// RecordUsage before EmitTerminal: EmitTerminal reads the store to fill
		// the cumulative TotalTokens on the result card, so this turn must be
		// counted first. Add is an in-memory map update (the async save is
		// non-blocking), so this does not delay the terminal emit.
		h.RecordUsage(chatID, result)
		if emitErr := h.EmitTerminal(ctx, chatID, replyToID, "Claude", 0, result); emitErr != nil {
			bridgebase.HandleTerminalEmitError(h.Core, ctx, chatID, replyToID, emitErr)
		}
		return nil
	})
	if err != nil {
		h.Logger.Debug("runPrompt finished with error", log.FieldChatID, chatID, log.FieldError, err)
	}
}

// resolveRunBinding re-reads the binding here rather than trusting the
// snapshot the caller took in handlePromptEvent: a concurrent /cd,
// /session-del or /model command (run in a separate goroutine) could have
// mutated the router between ensureBinding and this point. Fall back to the
// passed snapshot only if the binding was removed entirely.
func (h *Handler) resolveRunBinding(chatID string, snapshot router.Binding) router.Binding {
	if fresh, ok := h.Router.Lookup(chatID); ok {
		return fresh
	}
	return snapshot
}

// buildClaudeRunOptions constructs the claude.RunOptions from the current
// binding and the user's prompt. Directory and settings file are expanded
// here so the subprocess layer only sees concrete paths.
func (h *Handler) buildClaudeRunOptions(binding router.Binding, prompt string) claude.RunOptions {
	return claude.RunOptions{
		Prompt:         prompt,
		Directory:      binding.Directory,
		SessionID:      binding.SessionID,
		Model:          binding.ModelSpec,
		PermissionMode: binding.PermissionMode,
		EffortLevel:    binding.EffortLevel,
		SettingsFile:   strutil.ExpandEnvVars(binding.SettingsFile),
	}
}

// runClaudeWithStaleRetry executes one Claude run and retries once without a
// session id if the CLI reports the persisted session stale. The clear is
// guarded by the binding generation so a /session-del + new prompt that
// replaced the binding mid-turn is not clobbered.
func (h *Handler) runClaudeWithStaleRetry(ctx context.Context, chatID, replyToID string, binding router.Binding, prompt string) bridgebase.PromptResult {
	opts := h.buildClaudeRunOptions(binding, prompt)
	modelSpec := binding.ModelSpec

	result := h.runClaude(ctx, chatID, replyToID, opts, modelSpec, binding.Generation)

	if result.Err != nil && result.Stale && binding.SessionID != "" && ctx.Err() == nil {
		h.Logger.Warn("stale claude session, retrying without --resume",
			log.FieldChatID, chatID,
			log.FieldSessionID, binding.SessionID)
		h.Router.SetSessionIDIfGeneration(chatID, "", binding.Generation)
		opts.SessionID = ""
		result = h.runClaude(ctx, chatID, replyToID, opts, modelSpec, binding.Generation)
	}
	return result
}

// runClaude starts one Claude subprocess, streams its events into Controls,
// and reduces the stream to a bridgebase.PromptResult. generation is the router
// binding generation observed by the caller; the stream loop only writes the
// lazily learned session id back when the binding generation still matches,
// preventing a replaced binding from being clobbered mid-turn.
func (h *Handler) runClaude(ctx context.Context, chatID, replyToID string, opts claude.RunOptions, modelSpec string, generation uint64) bridgebase.PromptResult {
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
	return h.streamRun(ctx, chatID, replyToID, events, modelSpec, generation)
}
