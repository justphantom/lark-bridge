package claudebridge

import (
	"context"
	"errors"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// streamRun consumes a Claude event stream for one turn and translates each
// event into a protocol.Control emitted via h.emit, while reducing the stream
// to a promptResult.
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan claude.Event, modelSpec string) promptResult {
	var (
		text      strings.Builder
		sessionID string
		model     string

		// toolNames correlates a tool_use id to its name so the matching
		// tool_result (which carries only the id, not the name) can be
		// rendered with the right tool row in the progress card.
		toolNames = map[string]string{}

		// todoInputs buffers a TodoWrite call's input (`{"todos":[...]}`) by
		// tool_use id so the matching tool_result can rewrite the call to a
		// TypeTodo control. The tool_use itself emits no TypeToolUse: the row
		// would only need to be closed by the result, which the success path
		// replaces with TypeTodo (single-send, no row opened). The failure /
		// unparseable path falls back to a standalone TypeToolResult — the
		// renderer renders a no-prior-row result as its own row.
		todoInputs = map[string]string{}

		// taskKinds caches task_id→task_kind from task_started. Claude drops
		// task_type on task_progress/task_notification lines, so without this
		// the row name flips from "Shell" (local_bash at started) to "Agent"
		// (kind missing at notification), breaking the row's visual continuity.
		taskKinds = map[string]string{}

		// taskTitles caches task_id→title from task_started so the terminal
		// notification (whose desc is the summary, not the title) can still
		// render the stable title in the subagent zone. local_agent only.
		taskTitles = map[string]string{}

		// taskRunning caches task_id→latest progress meta so the terminal
		// notification (which drops usage) can still report the final
		// cumulative tool_uses / duration / tokens in the subagent zone.
		// local_agent only.
		taskRunning = map[string]taskRunningMeta{}
	)

	for ev := range events {
		h.Logger.Debug("bridge received claude event",
			log.FieldChatID, chatID,
			log.FieldEventType, ev.Type,
			log.FieldEventSubtype, ev.Subtype,
			log.FieldSessionID, ev.SessionID,
			"text_length", len(ev.Text),
			log.FieldToolName, ev.ToolName)

		// Stop early once the turn is cancelled.
		if ctx.Err() != nil {
			return promptResult{
				err:         ctx.Err(),
				isCancelled: true,
				model:       firstNonEmpty(model, modelSpec),
				sessionID:   sessionID,
			}
		}

		// Capture session id from system/init (before emitting so the binding
		// is updated regardless of downstream state). Guard against a concurrent
		// /session-del or /cd that may have Unbound this chat between turn
		// start and now: SetSessionID on a removed binding is a no-op, but on a
		// freshly recreated binding (a new prompt sneaking in) it would clobber
		// that new prompt's empty sessionID — so only write when the chat is
		// still bound.
		if sessionID == "" && ev.SessionID != "" {
			sessionID = ev.SessionID
			if _, ok := h.Router.Lookup(chatID); ok {
				h.Router.SetSessionID(chatID, sessionID)
			}
			h.Logger.Debug("captured claude session id",
				log.FieldChatID, chatID,
				log.FieldSessionID, sessionID)
		}
		if model == "" && ev.Model != "" {
			model = ev.Model
		}

		switch ev.Type {
		case claude.EventSystem:
			// Only init is actionable (carries session id + model). Other
			// system subtypes — chiefly thinking_tokens (the bulk of the
			// stream), but also any future internal signal — are ignored by
			// falling through this case to the loop.
			if ev.Subtype == claude.SubtypeInit && sessionID != "" {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeSessionInit,
					SessionInit: &protocol.SessionInitPayload{
						SessionID: sessionID,
						Model:     firstNonEmpty(model, modelSpec),
					},
				})
			}
		case claude.EventTaskStarted:
			// Cache task_kind by id: task_progress/task_notification drop
			// task_type, so without this the row name flips on close.
			if ev.TaskKind != "" && ev.TaskID != "" {
				taskKinds[ev.TaskID] = ev.TaskKind
			}
			if ev.TaskID != "" && ev.TaskDesc != "" {
				taskTitles[ev.TaskID] = ev.TaskDesc
			}
			kind := ev.TaskKind
			// local_agent (true AI subagent) routes to the dedicated subagent
			// zone via SubagentSummary; local_bash keeps the legacy leaf row.
			if isLocalAgentKind(kind) {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeToolUse,
					ToolUse: &protocol.ToolUsePayload{
						Name:       taskToolName(ev.TaskType, kind),
						Input:      ev.TaskDesc,
						IsSubagent: true,
						TaskID:     ev.TaskID,
						Subagent: &protocol.SubagentSummary{
							Status:       "running",
							TaskType:     kind,
							Type:         ev.TaskType,
							Title:        ev.TaskDesc,
							ChildSession: ev.TaskID,
						},
					},
				})
				break
			}
			// local_bash (or legacy missing kind): leaf-tool row, unchanged.
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: taskToolName(ev.TaskType, kind), Input: ev.TaskDesc, IsSubagent: true, TaskID: ev.TaskID},
			})
		case claude.EventTaskProgress:
			// Live subagent progress. Cache the cumulative usage so the
			// terminal notification can still report final stats.
			kind := ev.TaskKind
			if kind == "" {
				kind = taskKinds[ev.TaskID]
			}
			lastTool := extractTaskLastToolName(ev.Raw)
			if ev.TaskID != "" && isLocalAgentKind(kind) {
				taskRunning[ev.TaskID] = taskRunningMeta{
					desc:        ev.TaskDesc,
					toolUses:    ev.TaskSteps,
					durationMs:  ev.TaskMs,
					totalTokens: ev.TaskTokens,
					lastTool:    lastTool,
				}
				// True AI subagent: progressive update carries the live
				// description + cumulative usage so the subagent zone can
				// scroll the current action ("正在 Read internal/...").
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeToolUse,
					ToolUse: &protocol.ToolUsePayload{
						Name:       taskToolName(ev.TaskType, kind),
						Input:      taskProgressDesc(ev),
						IsSubagent: true,
						TaskID:     ev.TaskID,
						Subagent: &protocol.SubagentSummary{
							Status:       "running",
							TaskType:     kind,
							Type:         ev.TaskType,
							Title:        taskTitles[ev.TaskID],
							Description:  ev.TaskDesc,
							ChildSession: ev.TaskID,
							DurationMs:   ev.TaskMs,
							ToolUses:     ev.TaskSteps,
							TotalTokens:  ev.TaskTokens,
							LastToolName: lastTool,
						},
					},
				})
				break
			}
			// local_bash: re-emit as a ToolUse so the existing same-TaskID
			// row updates its description while staying running.
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: taskToolName(ev.TaskType, kind), Input: taskProgressDesc(ev), IsSubagent: true, TaskID: ev.TaskID},
			})
		case claude.EventTaskNotification:
			// Subagent finished: close the running row by TaskID. The
			// terminal summary (title + cumulative usage) rides on Input so
			// it lands in the tool-row description on the legacy path.
			kind := ev.TaskKind
			if kind == "" {
				kind = taskKinds[ev.TaskID]
			}
			if ev.TaskID != "" && isLocalAgentKind(kind) {
				// True AI subagent: terminal SubagentSummary carries the
				// preview (from task_notification.summary, which claude
				// inlines verbatim) plus the final cumulative usage. Newer
				// CLI lines carry usage on the notification itself; when
				// absent (the common case in real streams), fall back to
				// the last task_progress's cached usage.
				preview := ev.TaskDesc
				meta := taskRunning[ev.TaskID]
				toolUses := ev.TaskSteps
				if toolUses == 0 {
					toolUses = meta.toolUses
				}
				durationMs := ev.TaskMs
				if durationMs == 0 {
					durationMs = meta.durationMs
				}
				totalTokens := ev.TaskTokens
				if totalTokens == 0 {
					totalTokens = meta.totalTokens
				}
				status := "completed"
				if ev.IsToolError {
					status = "failed"
				}
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeToolResult,
					ToolResult: &protocol.ToolResultPayload{
						Name:       taskToolName(ev.TaskType, kind),
						Input:      taskProgressDesc(ev),
						IsError:    ev.IsToolError,
						IsSubagent: true,
						TaskID:     ev.TaskID,
						Subagent: &protocol.SubagentSummary{
							Status:       status,
							TaskType:     kind,
							Type:         ev.TaskType,
							Title:        taskTitles[ev.TaskID],
							ChildSession: ev.TaskID,
							DurationMs:   durationMs,
							ToolUses:     toolUses,
							TotalTokens:  totalTokens,
							LastToolName: meta.lastTool,
							Preview:      preview,
							OutputBytes:  len(preview),
						},
					},
				})
				delete(taskTitles, ev.TaskID)
				delete(taskRunning, ev.TaskID)
				break
			}
			// local_bash: leaf-tool row, unchanged.
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:       taskToolName(ev.TaskType, kind),
					Input:      taskProgressDesc(ev),
					IsError:    ev.IsToolError,
					IsSubagent: true,
					TaskID:     ev.TaskID,
				},
			})
		case claude.EventThinking:
			// A thinking content block arrives whole (parseContentBlocks emits
			// one Event per block, not a streaming delta), so Replace=true
			// mirrors opencode's reasoning handling: the thinking zone reflects
			// the latest block instead of concatenating every step's trace into
			// an unreadable wall.
			h.emitAsync(promptID, &protocol.Control{
				Type:   protocol.TypeThinking,
				ChatID: chatID,
				Thinking: &protocol.ThinkingPayload{
					Delta:   ev.Text,
					Replace: true,
				},
			})
		case claude.EventText:
			text.WriteString(ev.Text)
		case claude.EventToolUse:
			if id := ev.ToolID; id != "" {
				toolNames[id] = ev.ToolName
			}
			// TodoWrite: buffer the input and suppress the TypeToolUse that
			// would otherwise open a row. The matching tool_result decides
			// the outcome — TypeTodo on success (no row at all), TypeToolResult
			// on failure (AddToolResult renders a no-prior-row result as a
			// standalone row, so the failure still surfaces).
			if ev.ToolName == "TodoWrite" && ev.ToolID != "" {
				todoInputs[ev.ToolID] = ev.ToolInput
				continue
			}
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: ev.ToolName, Input: bridgebase.SummarizeToolInput(ev.ToolName, ev.ToolInput)},
			})
		case claude.EventToolResult:
			// claude tool_result carries only the id; look up the name
			// recorded at tool_use time so the card can match the row.
			name := ev.ToolName
			if name == "" {
				name = toolNames[ev.ToolID]
			}
			// TodoWrite success rewrites to TypeTodo (single-send, no row
			// opened at tool_use time). Failure or unparseable input falls
			// through to a TypeToolResult so the failure is still visible.
			if name == "TodoWrite" && !ev.IsToolError {
				if input, ok := todoInputs[ev.ToolID]; ok {
					if items, parsed := parseTodoItems(input); parsed {
						h.emitAsync(promptID, &protocol.Control{
							Type:   protocol.TypeTodo,
							ChatID: chatID,
							Todo:   &protocol.TodoPayload{Todos: items},
						})
						continue
					}
				}
			}
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:    name,
					Output:  ev.Text,
					IsError: ev.IsToolError,
				},
			})
		case claude.EventResult:
			return h.finalizeResult(ev, text.String(), sessionID, model, modelSpec, chatID)
		case claude.EventError:
			h.Logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", truncateForDebug(ev.Text, h.debugRedact()))
			return promptResult{
				err:       errors.New(nonEmpty(ev.Text, "Claude 运行出错")),
				model:     firstNonEmpty(model, modelSpec),
				sessionID: sessionID,
			}
		default:
			// Forward-compat: the parser forwards unknown line types verbatim
			// (raw retained). Log at debug so a schema change is observable
			// without dropping the event silently or breaking the turn.
			h.Logger.Debug("claude: unhandled event type",
				log.FieldChatID, chatID,
				log.FieldEventType, ev.Type,
				log.FieldEventSubtype, ev.Subtype)
		}
	}

	// Channel closed without a terminal event (defensive; the client normally
	// synthesises an EventError). If the context was cancelled (user abort
	// or prompt timeout), surface it as a cancellation rather than a generic
	// error so emitTerminal shows the right notice.
	if ctx.Err() != nil {
		return promptResult{
			err:         ctx.Err(),
			isCancelled: true,
			model:       firstNonEmpty(model, modelSpec),
			sessionID:   sessionID,
		}
	}
	return promptResult{
		err:       errors.New("claude 流意外结束，未收到结果事件"),
		model:     firstNonEmpty(model, modelSpec),
		sessionID: sessionID,
	}
}

// finalizeResult builds the promptResult from a result event. The reply
// comes from the result event's result field (the protocol truth), falling
// back to accumulated text blocks.
func (h *Handler) finalizeResult(ev claude.Event, accText, sessionID, model, modelSpec, chatID string) promptResult {
	h.Logger.Debug("bridge: result event",
		log.FieldChatID, chatID,
		"is_error", ev.IsError,
		"cost_usd", ev.CostUSD,
		log.FieldDuration, ev.DurationMs,
		log.FieldModel, firstNonEmpty(model, modelSpec),
		"result_preview", truncateForDebug(ev.Result, h.debugRedact()))

	result := promptResult{
		model:         firstNonEmpty(model, modelSpec),
		sessionID:     sessionID,
		durationMs:    ev.DurationMs,
		contextTokens: ev.InputTokens + ev.OutputTokens,
		costUSD:       ev.CostUSD,
		steps:         ev.NumTurns,

		inputTokens:   ev.InputTokens,
		outputTokens:  ev.OutputTokens,
		cacheRead:     ev.CacheRead,
		cacheCreation: ev.CacheCreation,
	}

	if ev.IsError {
		msg := ev.Result
		if strings.TrimSpace(msg) == "" {
			msg = "Claude 返回错误"
		}
		result.err = errors.New(msg)
		return result
	}

	reply := ev.Result
	if reply == "" {
		reply = bridgebase.StripThinking(accText, "> 💭 ")
	} else {
		reply = bridgebase.StripThinking(reply, "> 💭 ")
	}
	result.reply = reply
	return result
}

// maxDebugTextLen caps the preview length used in debug logs.
const maxDebugTextLen = 200

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// truncateForDebug returns a string for debug logging: optionally redacted
// (replaced wholesale) and always truncated to a bounded length.
func truncateForDebug(s string, redact bool) string {
	if redact {
		return "<redacted>"
	}
	return strutil.Truncate(s, maxDebugTextLen)
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
