package miniagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// maxIterations bounds the ReAct loop so a misbehaving LLM cannot cycle
// forever burning tokens.
const maxIterations = 20

// SignalKind tags what a Signal reports to the handler.
type SignalKind string

const (
	SignalToolUse    SignalKind = "tool_use"    // LLM requested a tool call
	SignalToolResult SignalKind = "tool_result" // tool finished (ok or error)
)

// Signal is one out-of-band event the loop fires so the handler can emit a
// matching Control (TypeToolUse / TypeToolResult) to the frontend. Name is
// the tool name; Input is the LLM's argument summary (for tool_use) or the
// call's path/input summary (for tool_result); Output is the tool's result
// text (tool_result only); IsError marks a failed call.
type Signal struct {
	Kind    SignalKind
	Name    string
	Input   string
	Output  string
	IsError bool
}

// EmitFunc receives out-of-band signals from the loop as it runs. promptID
// scopes the signal to the in-flight turn. May be nil (tests).
type EmitFunc func(promptID string, sig Signal)

// Result is what loop.Run returns to the handler: the terminal assistant
// text plus the cumulative token usage across every LLM call this run.
// History is the messages this turn added (the user message, any
// assistant tool_call messages, the matching tool results, and the final
// assistant text reply) — the handler persists it so the next turn carries
// the conversation context. Empty on error (failed turns do not pollute
// history).
type Result struct {
	Text    string
	Usage   Usage
	Steps   int // number of LLM calls made
	History []Message
}

// Run drives the ReAct loop for one turn.
//   - LLM returns plain text → loop terminates with that text.
//   - LLM returns tool_calls → loop executes each via cfg.Tools, emits
//     ToolUse/ToolResult signals, appends the assistant tool_calls message
//     plus one tool message per call, and continues. Bounded by
//     maxIterations.
//
// history is the prior conversation loaded for this chat (nil for the first
// turn or when memory is off); the userPrompt is appended to it to form the
// messages sent to the LLM. ctx bounds the whole turn. promptID scopes
// emits. logger/emit may be nil.
func Run(ctx context.Context, llm Client, cfg LoopConfig, promptID, userPrompt string, history []Message, emit EmitFunc, logger *log.Logger) (Result, error) {
	if llm == nil {
		return Result{}, errors.New("miniagent: llm client is nil")
	}
	if logger == nil {
		logger = log.Nop()
	}
	// Advertise the tools' schemas to the LLM on every call (cheap).
	toolSpecs := make([]ToolSpec, 0, len(cfg.Tools))
	toolByName := make(map[string]Tool, len(cfg.Tools))
	for _, t := range cfg.Tools {
		spec := t.Spec()
		toolSpecs = append(toolSpecs, spec)
		toolByName[spec.Name] = t
	}
	emitSignal := func(sig Signal) {
		if emit != nil {
			emit(promptID, sig)
		}
	}

	// msgs = loaded history + this turn's user message. newMsgs tracks only
	// what this turn adds, so the handler can append it to history without
	// re-saving the prior turns.
	userMsg := Message{Role: "user", Content: userPrompt}
	msgs := make([]Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, userMsg)
	newMsgs := []Message{userMsg}

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
			Tools:     toolSpecs,
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
			finalMsg := Message{Role: "assistant", Content: resp.Text}
			newMsgs = append(newMsgs, finalMsg)
			return Result{Text: resp.Text, Usage: total, Steps: step, History: newMsgs}, nil
		}

		// Tool branch: record the assistant's tool_calls verbatim, then
		// execute each and append a tool-role message carrying the result.
		// OpenAI requires tool_call_id on each tool message to match the
		// assistant's call id; a missing/mismatched id yields a 400.
		assistantMsg := Message{Role: "assistant", ToolCalls: resp.ToolCalls}
		msgs = append(msgs, assistantMsg)
		newMsgs = append(newMsgs, assistantMsg)
		for _, tc := range resp.ToolCalls {
			emitSignal(Signal{Kind: SignalToolUse, Name: tc.Name, Input: tc.Args})
			tool, ok := toolByName[tc.Name]
			var tres ToolResult
			if !ok {
				tres = ToolResult{IsError: true, Output: fmt.Sprintf("未知工具 %q", tc.Name)}
			} else {
				tres = tool.Call(ctx, tc.Args)
			}
			logger.Info("miniagent tool executed",
				log.FieldPromptID, promptID, "step", step,
				"tool", tc.Name, "is_error", tres.IsError,
				"output_len", len(tres.Output))
			// emit the FULL result to the frontend; feed only a trimmed copy
			// back to the LLM / history so one huge tool_result cannot crowd
			// out the rest of the conversation.
			emitSignal(Signal{Kind: SignalToolResult, Name: tc.Name, Input: tc.Args, Output: tres.Output, IsError: tres.IsError})
			toolMsg := Message{Role: "tool", ToolCallID: tc.ID, Content: truncateToolResult(tres.Output)}
			msgs = append(msgs, toolMsg)
			newMsgs = append(newMsgs, toolMsg)
		}
	}
	logger.Warn("miniagent loop exhausted max iterations",
		log.FieldPromptID, promptID, "max", maxIterations)
	return Result{Usage: total, Steps: maxIterations}, errors.New("miniagent: max iterations exceeded")
}

// LoopConfig carries the per-turn LLM parameters. Tools is the executable
// tool set (each Tool also contributes its Spec to the LLM schema).
type LoopConfig struct {
	Model     string
	System    string
	MaxTokens int
	Tools     []Tool
}
