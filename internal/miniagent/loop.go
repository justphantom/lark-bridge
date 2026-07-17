package miniagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// maxIterations bounds the ReAct loop so a misbehaving LLM cannot cycle
// forever burning tokens. P0 exits on the first call (no tools wired), so
// this only bites once P1 introduces tools.
const maxIterations = 20

// EmitFunc receives out-of-band signals from the loop as it runs: each
// tool_use (when the LLM asks for a tool) and each tool_result (after
// execution). P0 has no tools, so nothing fires; the signature is here so
// P1 wires the protocol.TypeToolUse / TypeToolResult emits without changing
// loop.Run's callers.
//
// promptID scopes the emit to the in-flight turn. kind is "tool_use" |
// "tool_result" | "thinking"; name/payload carry the tool name and a summary.
type EmitFunc func(promptID, kind, name, payload string)

// Result is what loop.Run returns to the handler: the terminal assistant
// text plus the cumulative token usage across every LLM call this run.
type Result struct {
	Text  string
	Usage Usage
	Steps int // number of LLM calls made (1 in P0)
}

// Run drives the ReAct loop for one turn. P0: a single LLM call with the
// user's prompt; if the LLM asks for a tool, the loop returns an error
// (tools land in P1). P1+: tool_calls trigger execution + emit + another
// LLM call, bounded by maxIterations.
//
// ctx bounds the whole turn (cancelled on /abort or process shutdown).
// promptID scopes emits to this turn. logger may be nil (tests). emit may
// be nil (tests).
func Run(ctx context.Context, llm Client, cfg LoopConfig, promptID, userPrompt string, emit EmitFunc, logger *log.Logger) (Result, error) {
	if llm == nil {
		return Result{}, errors.New("miniagent: llm client is nil")
	}
	if logger == nil {
		logger = log.Nop()
	}
	msgs := []Message{{Role: "user", Content: userPrompt}}
	var total Usage
	for step := 1; step <= maxIterations; step++ {
		if err := ctx.Err(); err != nil {
			logger.Info("miniagent loop ctx cancelled before call",
				log.FieldPromptID, promptID, "step", step)
			return Result{Usage: total, Steps: step - 1}, err
		}
		logger.Debug("miniagent llm call start",
			log.FieldPromptID, promptID, "step", step, "model", cfg.Model)
		callStart := time.Now()
		resp, err := llm.Do(ctx, Request{
			Model:     cfg.Model,
			System:    cfg.System,
			Messages:  msgs,
			MaxTokens: cfg.MaxTokens,
			Tools:     cfg.Tools,
		})
		callDur := time.Since(callStart)
		if err != nil {
			logger.Warn("miniagent llm call failed",
				log.FieldPromptID, promptID, "step", step,
				log.FieldError, err, "duration", callDur)
			return Result{Usage: total, Steps: step - 1}, fmt.Errorf("llm call %d: %w", step, err)
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		logger.Info("miniagent llm call done",
			log.FieldPromptID, promptID, "step", step,
			"duration", callDur,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"tool_calls", len(resp.ToolCalls),
			"reply_len", len(resp.Text))

		if len(resp.ToolCalls) == 0 {
			logger.Info("miniagent loop terminal (text reply)",
				log.FieldPromptID, promptID, "step", step, "total_steps", step)
			return Result{Text: resp.Text, Usage: total, Steps: step}, nil
		}
		// P0: no tools wired. Surface as an error so the handler emits
		// TypeError instead of silently dropping the turn. P1 replaces this
		// branch with: dispatch each call → emit tool_use/tool_result →
		// append assistant(tool_calls) + tool(role=tool) messages → continue.
		logger.Warn("miniagent tool calls rejected (P0)",
			log.FieldPromptID, promptID, "step", step, "tool_calls", len(resp.ToolCalls))
		return Result{Usage: total, Steps: step}, fmt.Errorf("miniagent: tool calls not yet supported (P0); LLM requested %d call(s)", len(resp.ToolCalls))
	}
	logger.Warn("miniagent loop exhausted max iterations",
		log.FieldPromptID, promptID, "max", maxIterations)
	return Result{Usage: total, Steps: maxIterations}, errors.New("miniagent: max iterations exceeded")
}

// LoopConfig carries the per-turn LLM parameters. The handler builds it
// from config.MiniAgent once at startup; memory (P2) appends to Messages
// outside this struct.
type LoopConfig struct {
	Model     string
	System    string
	MaxTokens int
	Tools     []ToolSpec
}
