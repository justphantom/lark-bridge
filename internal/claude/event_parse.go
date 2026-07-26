//go:build linux || darwin

package claude

import (
	"encoding/json"
	"fmt"
	"strings"
)

// lineHead is the common envelope decoded from every stream-json line.
// It carries the union of fields used across all event types so a single
// unmarshal suffices; per-type readers pick the fields they need.
type lineHead struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	SessionID  string          `json:"session_id"`
	Message    json.RawMessage `json:"message"`
	Result     string          `json:"result"`
	IsError    bool            `json:"is_error"`
	Errors     []string        `json:"errors"`
	CostUSD    float64         `json:"total_cost_usd"`
	DurationMs int64           `json:"duration_ms"`
	NumTurns   int             `json:"num_turns"`
	Usage      lineUsage       `json:"usage"`
	Model      string          `json:"model"`

	// Result-line metadata from CLI 2.x. DurationAPIMs is the API-only
	// wall time; StopReason is the model's stop reason.
	StopReason    string `json:"stop_reason"`
	DurationAPIMs int64  `json:"duration_api_ms"`
}

// lineUsage carries token accounting on a result line.
type lineUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// ParseEvent decodes one stream-json line into zero or more Events.
// Exported so callers can replay captured raw lines (e.g. from an
// archive) through the same parser the client uses.
func ParseEvent(line string) ([]Event, error) {
	return parseEvent(line)
}

// parseEvent decodes one stream-json line into zero or more Events.
// Unknown line types are forwarded as a single Event carrying the raw
// type/subtype so future CLI output is not silently dropped.
func parseEvent(line string) ([]Event, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}

	var head lineHead
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		// A result line carries the user's final answer; recover it
		// when the full decode fails (e.g. a numeric field overflowed
		// or a usage field changed type) so the answer is never
		// silently dropped over a malformed statistic. Non-result lines
		// keep the strict path.
		if ev, ok := parseResultLenient(line); ok {
			return []Event{ev}, nil
		}
		return nil, fmt.Errorf("parse json: %w", err)
	}

	switch head.Type {
	case "system":
		return parseSystemEvents(head, line)
	case "result":
		// When is_error is true the detail is in the errors[] array,
		// not the result field; surface it so the caller shows the real
		// cause (e.g. "No conversation found with session ID: …").
		detail := head.Result
		if head.IsError && strings.TrimSpace(detail) == "" && len(head.Errors) > 0 {
			detail = strings.Join(head.Errors, "; ")
		}
		return []Event{{
			Type:          EventResult,
			Subtype:       head.Subtype,
			SessionID:     head.SessionID,
			Result:        detail,
			IsError:       head.IsError,
			CostUSD:       head.CostUSD,
			DurationMs:    head.DurationMs,
			StopReason:    head.StopReason,
			DurationAPIMs: head.DurationAPIMs,
			NumTurns:      head.NumTurns,
			InputTokens:   head.Usage.InputTokens,
			OutputTokens:  head.Usage.OutputTokens,
			CacheRead:     head.Usage.CacheReadInputTokens,
			CacheCreation: head.Usage.CacheCreationInputTokens,
			Raw:           line,
		}}, nil
	case "assistant", "user":
		return parseContentBlocks(head.Type, head.SessionID, head.Message, line)
	default:
		// Forward-compat: surface unrecognised line types verbatim.
		return []Event{{Type: head.Type, Subtype: head.Subtype, SessionID: head.SessionID, Raw: line}}, nil
	}
}

// parseResultLenient recovers a result event from a line whose full
// unmarshal failed. It decodes only the high-value text fields (type,
// subtype, session_id, result, is_error, errors) and leaves the numeric
// accounting (cost/duration/tokens) at zero. Returns ok=false when the
// line is not a result or even the text fields are unusable, so the
// caller falls back to the original strict error.
func parseResultLenient(line string) (Event, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return Event{}, false
	}
	if probe.Type != "result" {
		return Event{}, false
	}
	var minimal struct {
		Subtype   string   `json:"subtype"`
		SessionID string   `json:"session_id"`
		Result    string   `json:"result"`
		IsError   bool     `json:"is_error"`
		Errors    []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(line), &minimal); err != nil {
		return Event{}, false
	}
	detail := minimal.Result
	if minimal.IsError && strings.TrimSpace(detail) == "" && len(minimal.Errors) > 0 {
		detail = strings.Join(minimal.Errors, "; ")
	}
	return Event{
		Type:      EventResult,
		Subtype:   minimal.Subtype,
		SessionID: minimal.SessionID,
		Result:    detail,
		IsError:   minimal.IsError,
		Raw:       line,
	}, true
}

// parseContentBlocks, stringifyContent and stringifyJSON live in
// event_parse_content.go — they form the content-block extraction group.

// parseSystemEvents decodes a system line. init populates the session/model;
// task_* subtypes (subagent lifecycle) are decoded into EventTask* carrying
// the live description and cumulative usage; any other subtype (e.g.
// thinking_tokens) is forwarded as a base EventSystem for the caller to ignore.
func parseSystemEvents(head lineHead, rawLine string) ([]Event, error) {
	switch head.Subtype {
	case EventTaskStarted, EventTaskProgress, EventTaskNotification:
		return parseTaskEvent(head, rawLine)
	default:
		return []Event{{
			Type:      EventSystem,
			Subtype:   head.Subtype,
			SessionID: head.SessionID,
			Model:     head.Model,
			Raw:       rawLine,
		}}, nil
	}
}

// taskLine decodes only the fields carried by task_* system lines. Kept local
// since these fields appear nowhere else in the stream-json schema.
type taskLine struct {
	TaskID       string `json:"task_id"`
	SubagentType string `json:"subagent_type"`
	TaskType     string `json:"task_type"`
	Description  string `json:"description"`
	Summary      string `json:"summary"`
	Status       string `json:"status"`
	Usage        struct {
		TotalTokens int   `json:"total_tokens"`
		ToolUses    int   `json:"tool_uses"`
		DurationMs  int64 `json:"duration_ms"`
	} `json:"usage"`
}

// parseTaskEvent decodes a task_* system line into the matching EventTask*.
// task_progress carries a live description plus cumulative usage; task_started
// carries the task title; task_notification carries the terminal summary.
func parseTaskEvent(head lineHead, rawLine string) ([]Event, error) {
	var t taskLine
	if err := json.Unmarshal([]byte(rawLine), &t); err != nil {
		return nil, fmt.Errorf("parse task event: %w", err)
	}
	ev := Event{
		Type:       head.Subtype,
		Subtype:    head.Subtype,
		SessionID:  head.SessionID,
		TaskID:     t.TaskID,
		TaskType:   t.SubagentType,
		TaskKind:   t.TaskType,
		TaskTokens: t.Usage.TotalTokens,
		TaskSteps:  t.Usage.ToolUses,
		TaskMs:     t.Usage.DurationMs,
		Raw:        rawLine,
	}
	// task_notification's terminal text is in summary; the others use the
	// live description field (which changes per progress tick).
	if head.Subtype == EventTaskNotification {
		ev.TaskDesc = t.Summary
		if t.Status != "" && t.Status != "completed" {
			ev.IsToolError = true
		}
	} else {
		ev.TaskDesc = t.Description
	}
	return []Event{ev}, nil
}
