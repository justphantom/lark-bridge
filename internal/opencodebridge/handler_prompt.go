package opencodebridge

import (
	"context"
	"fmt"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencode"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/streamarchive"
)

// runPrompt drives one opencode turn for chatID: it starts an `opencode` CLI
// subprocess, streams its events, and emits the terminal control. The
// session.created session id is back-filled onto the binding so the next turn
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
	// timer + idle watchdog). If the CLI goes silent for IdleTimeout the
	// timer fires Cancel(errIdleTimeout), which SIGKILLs the process group
	// (ApplyGroupCancel) so streamRun unblocks and returns IsIdleTimeout —
	// the user sees a "响应超时" notice instead of waiting forever on a
	// stuck subprocess (observed: glm-5.2 build agent hangs mid-step on
	// upstream LLM stalls).
	scaffold := h.RunPromptScaffold(parent, h.IdleTimeout, errIdleTimeout)
	defer scaffold.Stop()
	defer scaffold.Cancel(nil)
	ctx := scaffold.Ctx
	onActivity := scaffold.OnActivity

	modelSpec := binding.ModelSpec
	opts := opencode.RunOptions{
		Prompt:    prompt,
		Directory: binding.Directory,
		SessionID: binding.SessionID,
		Model:     modelSpec,
		Agent:     binding.Agent,
	}

	result := h.runOpencode(ctx, chatID, replyToID, opts, modelSpec, onActivity)

	// RecordUsage before EmitTerminal: EmitTerminal reads the store to fill
	// the cumulative TotalTokens on the result card, so this turn must be
	// counted first. Add is an in-memory map update (the async save is
	// non-blocking), so this does not delay the terminal emit.
	h.RecordUsage(chatID, result)
	h.EmitTerminal(ctx, chatID, replyToID, "opencode", int(h.IdleTimeout.Seconds()), result)
}

// runOpencode starts one opencode subprocess, streams its events into
// Controls, and reduces the stream to a bridgebase.PromptResult. onActivity is wired
// through to streamRun so the idle watchdog in runPrompt resets per event.
func (h *Handler) runOpencode(ctx context.Context, chatID, promptID string, opts opencode.RunOptions, modelSpec string, onActivity func()) bridgebase.PromptResult {
	// Archive the raw stream for this run before launching the subprocess so
	// the sink is wired for the whole lifetime. Best-effort: nil sink = off.
	sink, closeSink := streamarchive.NewSink(h.Logger, h.StateDir, "opencode", chatID, promptID, h.StreamHistory, h.StreamArchiveRedact)
	if sink != nil {
		opts.LineSink = sink
		defer func() { _ = closeSink() }() // archive already flushed
	}

	events, err := h.agent.Run(ctx, opts)
	if err != nil {
		return bridgebase.PromptResult{
			Err:   fmt.Errorf("启动 opencode 失败: %w", err),
			Model: bridgebase.ResolveModel("", modelSpec, "opencode"),
		}
	}
	return h.streamRun(ctx, chatID, promptID, events, modelSpec, onActivity)
}
