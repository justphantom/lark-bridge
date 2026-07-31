package ompbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// errIdleTimeout is the cancel-cause set by the idle watchdog (see runPrompt)
// when no stdout event arrives within IdleTimeout. streamRun checks it via
// context.Cause to tell an idle kill apart from a user /session-abort, so
// emitTerminal can show "响应超时" vs "已取消".
var errIdleTimeout = errors.New("omp idle: no stdout event within idle_timeout")

// streamRun consumes an omp event stream for one turn and translates each
// event into a protocol.Control emitted via h.emit, while reducing the stream
// to a bridgebase.PromptResult. onActivity (nil disables the watchdog) is invoked once
// per received event so runPrompt can reset its idle timer.
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan omp.Event, modelSpec string, onActivity func()) bridgebase.PromptResult {
	var (
		text      strings.Builder
		sessionID string
		stepCount int
		startTime time.Time

		// accTokens/accCost accumulate every role=assistant message_end so
		// the usage store records the full turn total. omp emits one
		// message_end per assistant message; agent_end carries no telemetry
		// (§A.1), so message_end is the sole source (§7.4).
		accInput, accOutput, accCacheRead, accCacheWrite int
		accCost                                          float64

		// F3 text_end fallback: deltaSeen tracks whether any text_delta was
		// received in the current round. lastTextEnd holds the full text from
		// a text_end event. If EventMessageEnd(role=assistant) arrives with
		// !deltaSeen && lastTextEnd != "", the text_end content is used as
		// fallback to prevent empty assistant replies.
		deltaSeen   bool
		lastTextEnd string

		// F4 auto_retry limit: the highest attempt number seen so far in this
		// turn, used to detect when the limit is exceeded.
		maxAttempt int

		// Tool start→end join. OMP splits one tool call into a start (carrying
		// args/intent) and an end (carrying only result/isError), unlike
		// opencode's single completed event. toolArgsByCallID stashes the
		// start's raw args keyed by toolCallId so the EventToolEnd handler can
		// drive todowrite (full todos list) and task (subagent_type) special
		// paths, and reuse the input summary for the result row.
		toolArgsByCallID  = make(map[string]string)
		toolInputByCallID = make(map[string]string)

		// lastTurnEndText holds the most recent turn_end assistant message
		// text, used as a fallback reply when the streaming path produced no
		// text (finalizeResult consults it after the accText path).
		lastTurnEndText string
	)

	for ev := range events {
		if onActivity != nil {
			onActivity()
		}
		h.Logger.Debug("bridge received omp event",
			log.FieldChatID, chatID,
			log.FieldEventType, ev.Type,
			log.FieldSessionID, ev.SessionID,
			"text_length", len(ev.Text),
			log.FieldToolName, ev.ToolName)

		if ctx.Err() != nil {
			idle := errors.Is(context.Cause(ctx), errIdleTimeout)
			return bridgebase.PromptResult{
				Err:           ctx.Err(),
				IsCancelled:   !idle,
				IsIdleTimeout: idle,
				Model:         bridgebase.ResolveModel("", modelSpec, "omp"),
				SessionID:     sessionID,
			}
		}

		// Only write the session id back when the chat is still bound: a
		// concurrent /session-del or /cd may have Unbound it, and a recreated
		// binding (new prompt) must not be clobbered with this turn's id.
		// The first `session` header line carries the id; back-fill it onto
		// the binding so the next turn --resume continues the session.
		if sessionID == "" && ev.SessionID != "" {
			sessionID = ev.SessionID
			if _, ok := h.Router.Lookup(chatID); ok {
				h.Router.SetSessionID(chatID, sessionID)
			}
			// Emit TypeSessionInit so the frontend footer renders Model +
			// SessionID instead of leaving those fields blank for the turn.
			h.EmitAsync(promptID, &protocol.Control{
				Type: protocol.TypeSessionInit,
				SessionInit: &protocol.SessionInitPayload{
					SessionID: sessionID,
					Model:     bridgebase.ResolveModel("", modelSpec, "omp"),
					Title:     ev.SessionTitle,
				},
			})
		}

		switch ev.Type {
		case omp.EventAgentStart:
			stepCount++
			// agent_start fires ONCE per turn (and once per auto_retry), NOT
			// once per assistant round. The per-round text reset happens on
			// EventTurnStart (verified against agnes-2.0-flash: a tool-call
			// turn emits agent_start → turn_start #1 → … → turn_start #2 →
			// …, and only resetting on turn_start discards round 1's
			// inline-thinking preamble correctly).
			if startTime.IsZero() {
				startTime = time.Now()
			}
			// Emit TypeProgress WITHOUT a Description: the dispatcher still
			// bumps stepCount (card title "第 N 轮"), but no banner is set —
			// the title already conveys the step, and a banner would
			// overwrite any standing gate/loading notice from a picker.
			h.EmitAsync(promptID, &protocol.Control{
				Type:     protocol.TypeProgress,
				ChatID:   chatID,
				Progress: &protocol.ProgressPayload{},
			})
		case omp.EventTurnStart:
			// New assistant round begins: discard any text accumulated in the
			// previous round. A tool-call turn streams one assistant message
			// per round — round N's preamble (for agnes/glm: inline thinking
			// text ending in a stray ` response` before the toolcall) must not
			// be concatenated onto round N+1's final answer. StripThinking
			// cannot rescue this because OMP emits the closing ` response`
			// without a matching open tag; turn_start is the only clean
			// boundary. (The first turn_start fires on an empty accumulator,
			// so the reset is harmless there.)
			text.Reset()
			// F3: reset per-round text_end fallback state.
			deltaSeen = false
			lastTextEnd = ""
		case omp.EventMessageUpdate:
			text.WriteString(ev.Text)
			// F3: any text_delta means the bridge has the full text via the
			// streaming path; text_end should not be used as fallback.
			deltaSeen = true
			lastTextEnd = ""
		case omp.EventTextEnd:
			// F3: record the full text block as fallback candidate. The
			// bridge only uses it if EventMessageEnd arrives with !deltaSeen.
			lastTextEnd = ev.Text
		case omp.EventMessageEnd:
			// Usage (input/output/cacheRead/cacheWrite/cost) appears ONLY on
			// role=assistant message_end events (camelCase fields, §A.6);
			// role=toolResult message_end carries none. The parser fills
			// zeros for toolResult, and gates extraction on role=assistant,
			// so this accumulate is safe for both. agent_end has no
			// telemetry (§A.1), so this is the sole usage source.
			if ev.Role == "assistant" {
				accInput += ev.InputTokens
				accOutput += ev.OutputTokens
				accCacheRead += ev.CacheRead
				accCacheWrite += ev.CacheWrite
				accCost += ev.CostUSD

				// F3: if no text_delta was received in this round, use the
				// text_end block as fallback to prevent empty assistant replies.
				if !deltaSeen && lastTextEnd != "" {
					h.Logger.Warn("omp: text_end fallback used — no text_delta in assistant round",
						log.FieldChatID, chatID)
					eventmetrics.OMPTextEndFallback.Inc()
					text.WriteString(lastTextEnd)
				}
			}
			// role=custom carries a system-injected nudge (e.g. customType
			// "mid-run-todo-nudge" — "Gentle reminder: N todos still open").
			// Route through TypeProgress (NOT TypeNotice): a mid-run notice
			// shares the per-PromptID terminals dedup key with TypeResult
			// (dispatcher_control.go) and would finalise the turn, silently
			// dropping the real TypeResult. Same root cause
			// ThinkingLevelChanged already avoids (B2 regression). Surfaces
			// the reminder on the progress card instead of a terminal notice.
			if ev.Role == "custom" && ev.Text == "mid-run-todo-nudge" {
				h.EmitAsync(promptID, &protocol.Control{
					Type:   protocol.TypeProgress,
					ChatID: chatID,
					Progress: &protocol.ProgressPayload{
						Description: "当前仍有未完成的 todo 项，可在适当时机更新状态。",
					},
				})
			}
		case omp.EventThinking:
			// omp streams thinking_delta increments; thinking_end carries the
			// whole block. Replace=true lets the renderer's thinking zone
			// reflect the latest state instead of concatenating every chunk
			// into an unreadable wall (claude/opencode parity).
			h.EmitAsync(promptID, &protocol.Control{
				Type:   protocol.TypeThinking,
				ChatID: chatID,
				Thinking: &protocol.ThinkingPayload{
					Delta:   ev.Text,
					Replace: true,
				},
			})
		case omp.EventNotice:
			// Runtime notice (e.g. "xd://: mounted mcp__codegraph_explore").
			// Forward via TypeProgress (NOT TypeNotice): a mid-run notice
			// shares the per-PromptID terminals dedup key with TypeResult
			// (dispatcher_control.go) and would finalise the turn, dropping
			// the real reply. Surfaces the message on the progress card so
			// CLI/runtime notices reach the user instead of being dropped.
			h.EmitAsync(promptID, &protocol.Control{
				Type:   protocol.TypeProgress,
				ChatID: chatID,
				Progress: &protocol.ProgressPayload{
					Description: ev.NoticeMessage,
				},
			})
		case omp.EventThinkingLevelChanged:
			// The CLI resolved a thinking effort (often configured=auto →
			// resolved=high). This event fires VERY early in the OMP stream
			// (right after the `session` header, before agent_start), so it
			// reaches the frontend well before the final TypeResult. Emitting
			// it as a TypeNotice would collide with the frontend's terminal
			// dedup: TypeNotice shares the per-PromptID `terminals` set with
			// TypeResult/Error (dispatcher_control.go), so the early notice
			// finalises the prompt and the real TypeResult is silently dropped
			// — the user sees only "当前实际使用 thinking level: high" and no
			// final reply (B2 regression from 6ba58fd). Log at debug only; if
			// the resolved level ever needs to reach the card it MUST go via a
			// NON-terminal channel (TypeProgress description or a TypeSessionInit
			// extension field), never a standalone TypeNotice.
			if ev.Text != "" {
				h.Logger.Debug("omp thinking level changed",
					log.FieldChatID, chatID,
					"resolved", ev.Text)
			}
		case omp.EventToolUpdate:
			// Intermediate tool output (tool_execution_update). Emitting a
			// real streaming tool-result row would need a protocol change
			// (Phase 2); for now surface a lightweight TypeProgress count so a
			// long-running tool (glob/bash) is not silent. The description is
			// derived from partialResult.details when it carries a file count.
			if desc := summarizeToolUpdate(ev); desc != "" {
				h.EmitAsync(promptID, &protocol.Control{
					Type:     protocol.TypeProgress,
					ChatID:   chatID,
					Progress: &protocol.ProgressPayload{Description: desc},
				})
			}
		case omp.EventTodoReminder, omp.EventTTSRTriggered:
			// In-run diagnostic reminders: the todowrite tool remains the
			// authoritative list source (rendered via TypeTodo), so these do
			// not emit a control. Logged at debug for observability and left
			// as named types so the unknown-event metric does not tick.
			h.Logger.Debug("omp diagnostic event",
				log.FieldChatID, chatID,
				log.FieldEventType, ev.Type)
		case omp.EventToolStart:
			// Stash args + input summary keyed by toolCallId: OMP's end event
			// carries only result/isError, so the EventToolEnd handler joins back
			// here to recover the input summary (for the result row) and the raw
			// args (todowrite's todos / task's subagent_type).
			if ev.ToolCallID != "" {
				toolArgsByCallID[ev.ToolCallID] = ev.ToolArgs
				toolInputByCallID[ev.ToolCallID] = ev.ToolInput
			}
			toolUse := &protocol.ToolUsePayload{
				Name:  ev.ToolName,
				Input: bridgebase.SummarizeToolInput(ev.ToolName, ev.ToolInput),
			}
			// A "task" tool call IS a subagent delegation: mark the row so the
			// renderer counts it in the subagent category even before the
			// terminal summary arrives (claude/opencode parity).
			if ev.ToolName == "task" {
				toolUse.IsSubagent = true
			}
			h.EmitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ChatID:  chatID,
				ToolUse: toolUse,
			})
		case omp.EventToolEnd:
			// Recover the start's input summary + raw args via toolCallId (the
			// end event carries neither). todowrite/task special paths key off
			// the raw args; the input summary feeds the result row's "Input"
			// column so it matches the start row.
			args := toolArgsByCallID[ev.ToolCallID]
			inputSummary := toolInputByCallID[ev.ToolCallID]
			delete(toolArgsByCallID, ev.ToolCallID)
			delete(toolInputByCallID, ev.ToolCallID)

			// todowrite → TypeTodo (single-send): a completed todowrite carries
			// the full todos list, rendered as the progress card's todo zone
			// (✅/⏳/⬜ rows). A failed todowrite falls through to the generic
			// TypeToolResult so the failure is still visible. The args come from
			// the start event (the end carries none).
			if ev.ToolName == "todowrite" && !ev.IsToolError {
				if items, ok := bridgebase.ParseTodoItems(args); ok {
					h.EmitAsync(promptID, &protocol.Control{
						Type:   protocol.TypeTodo,
						ChatID: chatID,
						Todo:   &protocol.TodoPayload{Todos: items},
					})
					continue
				}
			}

			toolResult := &protocol.ToolResultPayload{
				Name:    ev.ToolName,
				Input:   bridgebase.SummarizeToolInput(ev.ToolName, inputSummary),
				Output:  ev.ToolOutput,
				IsError: ev.IsToolError,
			}
			// task → subagent zone: lift the delegation into a dedicated
			// renderer zone (claude/opencode parity). OMP's task tool carries
			// agent/task in args; the result is the subagent's output (no
			// <task_result> wrapper, unlike opencode). IsSubagent marks the row
			// for category counting; Subagent drives the dedicated zone.
			if ev.ToolName == "task" {
				toolResult.IsSubagent = true
				if meta := extractTaskSubagent(args, ev.ToolOutput); meta != nil {
					status := "completed"
					if ev.IsToolError {
						status = "failed"
					}
					toolResult.Subagent = &protocol.SubagentSummary{
						Status:      status,
						TaskType:    "agent",
						Type:        meta.Type,
						Title:       meta.Title,
						Preview:     meta.Preview,
						OutputBytes: meta.OutputBytes,
					}
				}
			}
			h.EmitAsync(promptID, &protocol.Control{
				Type:       protocol.TypeToolResult,
				ChatID:     chatID,
				ToolResult: toolResult,
			})
		case omp.EventAutoRetry:
			// F4: track the highest attempt number and check the limit.
			if ev.Attempt > maxAttempt {
				maxAttempt = ev.Attempt
			}
			if h.maxAutoRetries > 0 && maxAttempt >= h.maxAutoRetries {
				h.Logger.Warn("omp: auto_retry exceeded limit, aborting turn",
					log.FieldChatID, chatID,
					"attempt", maxAttempt,
					"limit", h.maxAutoRetries)
				eventmetrics.OMPAutoRetryLimit.Inc()
				h.AbortChat(chatID)
				// No mid-run TypeNotice here (B2/S1): it would share the
				// frontend's per-PromptID terminal dedup with the real
				// terminal control and could drop the final card. The limit
				// message rides PromptResult.Err and is rendered once by
				// EmitTerminal's cancel branch.
				return bridgebase.PromptResult{
					Err:         fmt.Errorf("自动重试已达上限（%d次），终止回合", maxAttempt),
					IsCancelled: true,
					Model:       bridgebase.ResolveModel("", modelSpec, "omp"),
					SessionID:   sessionID,
				}
			}
			// omp began an automatic retry; surface it so the card is not
			// silent during the retry window. The turn continues (auto_retry
			// is non-terminal).
			h.EmitAsync(promptID, &protocol.Control{
				Type:     protocol.TypeProgress,
				ChatID:   chatID,
				Progress: &protocol.ProgressPayload{Description: fmt.Sprintf("自动重试 #%d", ev.Attempt)},
			})
		case omp.EventTurnEnd:
			// turn_end carries the round's complete assistant message. Capture
			// its text as a fallback reply source for finalizeResult, used when
			// the streaming path produced no text (no text_delta and no
			// text_end). It carries no usage and no terminal signal, so the
			// turn continues.
			if t := extractMessageText(ev.Raw); t != "" {
				lastTurnEndText = t
			}
		case omp.EventAgentEnd:
			return h.finalizeResult(text.String(), lastTurnEndText, sessionID, modelSpec, chatID, stepCount, startTime,
				accInput, accOutput, accCacheRead, accCacheWrite, accCost)
		case omp.EventError:
			h.Logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", bridgebase.TruncateForDebug(ev.Text, h.DebugRedact()))
			// Append the upstream error code (HTTP status / provider error id,
			// e.g. [429/135168]) so the user can distinguish rate-limit / auth /
			// model errors without grepping the raw message.
			msg := bridgebase.NonEmpty(ev.Text, "OMP 运行出错")
			if ev.ErrorStatus != 0 || ev.ErrorID != 0 {
				msg = fmt.Sprintf("[%d/%d] %s", ev.ErrorStatus, ev.ErrorID, msg)
			}
			return bridgebase.PromptResult{
				Err:       errors.New(msg),
				Model:     bridgebase.ResolveModel("", modelSpec, "omp"),
				SessionID: sessionID,
			}
		default:
			// Forward-compat: the parser forwards unknown line types verbatim.
			// Log at debug so a schema change is observable without breaking
			// the turn.
			eventmetrics.UnknownEvent("omp", ev.Type).Inc()
			h.Logger.Debug("omp: unhandled event type",
				log.FieldChatID, chatID,
				log.FieldEventType, ev.Type)
		}
	}

	// Channel closed without a terminal event (defensive; the client
	// normally synthesises an EventError). If the context was cancelled
	// (user abort or prompt timeout), surface it as a cancellation rather
	// than a generic error so emitTerminal shows the right notice.
	if ctx.Err() != nil {
		idle := errors.Is(context.Cause(ctx), errIdleTimeout)
		return bridgebase.PromptResult{
			Err:           ctx.Err(),
			IsCancelled:   !idle,
			IsIdleTimeout: idle,
			Model:         bridgebase.ResolveModel("", modelSpec, "omp"),
			SessionID:     sessionID,
		}
	}
	return bridgebase.PromptResult{
		Err:       errors.New("omp 流意外结束，未收到结果事件"),
		Model:     bridgebase.ResolveModel("", modelSpec, "omp"),
		SessionID: sessionID,
	}
}

// finalizeResult builds the bridgebase.PromptResult from an agent_end event per §7.4.
// agent_end carries no telemetry (§A.1), so usage is read entirely from the
// acc* accumulators (filled by the EventMessageEnd case in streamRun). The
// terminal round's text is the accumulated assistant text; turnEndText is the
// most recent turn_end assistant message text, used as a fallback reply when
// the streaming path produced no text (no text_delta / text_end).
func (h *Handler) finalizeResult(accText, turnEndText, sessionID, modelSpec, chatID string, stepCount int, startTime time.Time,
	accInput, accOutput, accCacheRead, accCacheWrite int, accCost float64) bridgebase.PromptResult {
	var durationMs int64
	if !startTime.IsZero() {
		durationMs = time.Since(startTime).Milliseconds()
	}

	result := bridgebase.PromptResult{
		Model:      bridgebase.ResolveModel("", modelSpec, "omp"),
		SessionID:  sessionID,
		DurationMs: durationMs,
		// contextTokens = accInput + accOutput (non-cache), aligned with the
		// claude/opencode result card so the token count stays comparable
		// across backends.
		ContextToken: accInput + accOutput,
		CostUSD:      accCost,
		Steps:        stepCount,

		InputTokens:  accInput,
		OutputTokens: accOutput,
		CacheRead:    accCacheRead,
		CacheWrite:   accCacheWrite,
	}

	reply := bridgebase.StripThinking(accText, "> ")
	// Fallback: when the streaming path produced no assistant text (no
	// text_delta and no text_end fallback fired), recover the reply from the
	// most recent turn_end assistant message so the card is not blank. This is
	// rare (the F3 text_end fallback covers the common no-delta case) but
	// guards against a turn where message_update was entirely absent.
	if strings.TrimSpace(reply) == "" {
		reply = bridgebase.StripThinking(turnEndText, "> ")
	}
	result.Reply = reply
	return result
}

// maxDebugTextLen / nonEmpty / truncateForDebug / resolveModel were local
// copies of helpers now shared from package bridgebase (util.go).

// summarizeToolUpdate derives a short progress description from a
// tool_execution_update event's partialResult.details. Only the fileCount
// field is surfaced (e.g. "glob 已返回 5 项"), so a long-running glob/bash is
// not silent. Returns "" when the event carries no usable count, in which
// case the bridge emits no TypeProgress (avoiding noise on updates without a
// details block).
func summarizeToolUpdate(ev omp.Event) string {
	var partial struct {
		PartialResult struct {
			Details struct {
				FileCount int  `json:"fileCount"`
				Truncated bool `json:"truncated"`
			} `json:"details"`
		} `json:"partialResult"`
	}
	if err := json.Unmarshal([]byte(ev.Raw), &partial); err != nil {
		return ""
	}
	if n := partial.PartialResult.Details.FileCount; n > 0 {
		s := fmt.Sprintf("%s 已返回 %d 项", ev.ToolName, n)
		if partial.PartialResult.Details.Truncated {
			s += "（已截断）"
		}
		return s
	}
	return ""
}

// extractMessageText pulls the concatenated text content from a message-bearing
// event's message.content array (used for turn_end's assistant message). Each
// content block's text is concatenated (matching strutil.StringifyContent), so
// a multi-paragraph assistant reply is recovered whole. Returns "" when the
// event has no message envelope or no text blocks.
func extractMessageText(rawLine string) string {
	var env struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(rawLine), &env); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range env.Message.Content {
		if blk.Type == "text" || blk.Type == "" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// taskSubagentMeta is the bridge-side view of a "task" tool delegation,
// flattened from the start event's args. The bridge converts it to a
// protocol.SubagentSummary at the matching tool_execution_end.
type taskSubagentMeta struct {
	Type        string // args.agent — the subagent type
	Title       string // args.i (intent) > args.task > args.agent
	Preview     string // the task tool's output text
	OutputBytes int    // byte length of the output
}

// extractTaskSubagent builds a taskSubagentMeta from the start event's raw args
// JSON and the end event's output text. OMP's task tool carries agent/task/i in
// args (no <task_result> wrapper, unlike opencode — verified against the stream
// archives), so the output is used verbatim as the preview. Returns nil when
// the args carry no agent/task/i (a non-delegation "task" call degrades to a
// plain leaf-tool row with IsSubagent still set).
func extractTaskSubagent(argsJSON, output string) *taskSubagentMeta {
	if argsJSON == "" {
		return nil
	}
	var input struct {
		Agent string `json:"agent"`
		Task  string `json:"task"`
		I     string `json:"i"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &input); err != nil {
		return nil
	}
	// Require at least one identifying field so a bare {} args does not
	// produce an empty subagent zone.
	if input.Agent == "" && input.Task == "" && input.I == "" {
		return nil
	}
	title := input.I
	if title == "" {
		title = input.Task
	}
	if title == "" {
		title = input.Agent
	}
	preview := strutil.Truncate(output, 200)
	return &taskSubagentMeta{
		Type:        input.Agent,
		Title:       title,
		Preview:     preview,
		OutputBytes: len(output),
	}
}
