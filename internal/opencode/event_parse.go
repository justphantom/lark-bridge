//go:build linux || darwin

package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ndjsonLine is the flexible envelope decoded from every opencode stdout line.
type ndjsonLine struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
	Message   string          `json:"message"`
	// Error is sent as json.RawMessage because opencode's schema changed
	// across versions: legacy CLI wrote a plain string here, while 1.18+
	// writes a structured {name, data:{message, statusCode,...}} object.
	// extractErrorMessage handles both shapes.
	Error json.RawMessage `json:"error"`
}

// partShape is the decoded "part" object nested inside an event line.
type partShape struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
	Tool   string `json:"tool"`
	Title  string `json:"title"`
	// tool output lives under state, not at top level.
	State struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"output"`
		// Error carries the failure description when status="error".
		// opencode swaps state.output for state.error on tool failure
		// (read of a missing file, write to a read-only path, ...),
		// so without this field the bridge sees an empty result and
		// drops the actual cause ("File not found", "PermissionDenied").
		Error string `json:"error"`
		// Metadata is populated by shell-style tools (bash): its exit
		// field carries the command's exit code. status stays
		// "completed" even on non-zero exit, so exit!=0 is the only
		// signal that the underlying command failed. FileDiff is
		// populated by edit and carries the additions/deletions count
		// surfaced as a "+N -M" suffix on the tool row.
		Metadata struct {
			Exit      int  `json:"exit"`
			Truncated bool `json:"truncated"`
			FileDiff  struct {
				Additions int `json:"additions"`
				Deletions int `json:"deletions"`
			} `json:"filediff"`
		} `json:"metadata"`
	} `json:"state"`
	// step-finish carries token/cost accounting.
	Tokens struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Cost float64 `json:"cost"`
}

// ParseEvent decodes one NDJSON line into zero or more Events. Exported so
// tests in other packages can build Events from real CLI output lines instead
// of constructing Event structs (whose fields are unexported).
func ParseEvent(line string) ([]Event, error) {
	return parseEvent(line)
}

// parseEvent decodes one NDJSON line into zero or more Events.
func parseEvent(line string) ([]Event, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}

	var head ndjsonLine
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	var p partShape
	if len(head.Part) > 0 {
		// A present-but-malformed part indicates schema drift; surfacing the
		// error lets pump log it (matching claude's parseContentBlocks). The
		// event types that carry the meaningful payload (text/reasoning/
		// tool_use/step_finish) all rely on a correctly-decoded part, so a
		// silent zero-value would emit an empty/degraded card with no signal.
		if err := json.Unmarshal(head.Part, &p); err != nil {
			return nil, fmt.Errorf("parse part: %w", err)
		}
	}

	base := Event{sessionID: head.SessionID, raw: line}

	switch head.Type {
	// Session lifecycle
	case "session.created", "session.updated":
		return []Event{{kind: EventSession, sessionID: head.SessionID, raw: line}}, nil

	// Step start — signals a new agent step; the bridge emits a progress card.
	case "step_start":
		ev := base
		ev.kind = EventStepStart
		return []Event{ev}, nil

	// Text output (assistant reply)
	case "text":
		ev := base
		ev.kind = EventText
		ev.text = p.Text
		return []Event{ev}, nil

	// Reasoning / thinking
	case "reasoning":
		ev := base
		ev.kind = EventThinking
		ev.text = p.Text
		return []Event{ev}, nil

	// Tool use — opencode emits a single tool_use event with state.status
	// indicating completion. The tool name is in part.tool; the command
	// summary in part.title; the output in part.state.output.
	case "tool_use":
		return parseToolEvent(base, p), nil

	// Step finish — only terminal when reason is "stop" (not "tool-calls").
	// reason="tool-calls" means the model called tools and will continue;
	// reason="stop" means the turn is truly complete. Both carry token
	// accounting: a turn with N tool-calls steps plus a final stop step
	// accumulates N+1 step_finish lines, and only by capturing every one
	// (the tool-calls steps as EventStepFinish, the stop step as EventResult)
	// does the usage total stay accurate. Previously the tool-calls steps
	// were dropped, losing ~96% of input tokens on tool-heavy turns.
	case "step_finish":
		if p.Reason == "stop" {
			ev := base
			ev.kind = EventResult
			ev.inputTokens = p.Tokens.Input
			ev.outputTokens = p.Tokens.Output
			ev.cacheRead = p.Tokens.Cache.Read
			ev.cacheWrite = p.Tokens.Cache.Write
			ev.cost = p.Cost
			return []Event{ev}, nil
		}
		// tool-calls or other non-stop reasons: emit a StepFinish carrying
		// this step's tokens so the bridge can accumulate them; it does not
		// terminate the turn.
		ev := base
		ev.kind = EventStepFinish
		ev.inputTokens = p.Tokens.Input
		ev.outputTokens = p.Tokens.Output
		ev.cacheRead = p.Tokens.Cache.Read
		ev.cacheWrite = p.Tokens.Cache.Write
		ev.cost = p.Cost
		return []Event{ev}, nil

	// Explicit result/finish/end line (forward-compat)
	case "result", "finish", "end":
		ev := base
		ev.kind = EventResult
		return []Event{ev}, nil

	// Explicit error
	case "error":
		msg := head.Message
		if msg == "" {
			msg = extractErrorMessage(head.Error)
		}
		if msg == "" {
			msg = head.Type + " error"
		}
		return []Event{{kind: EventError, sessionID: head.SessionID, text: msg, isError: true, raw: line}}, nil

	default:
		// Forward-compat: surface unrecognised line types for debugging.
		return []Event{{kind: head.Type, sessionID: head.SessionID, raw: line}}, nil
	}
}

// parseToolEvent maps a tool_use event to a single EventToolResult. opencode
// emits one completed event per tool call (state.status is already
// "completed"/"error"), so splitting it into a synthetic use+result pair only
// created a transient running row that flipped to done a frame later, and
// mismatched when the same tool ran back-to-back. Emitting just the result
// carries the input summary (for the "Read: /path" prefix) and the output in
// one shot.
func parseToolEvent(base Event, p partShape) []Event {
	result := base
	result.kind = EventToolResult
	result.toolName = p.Tool
	// Input summary drives the tool-row description on the card.
	if p.Title != "" {
		result.toolInput = p.Title
	} else if len(p.State.Input) > 0 {
		// opencode does not always populate part.title (notably edit on
		// some versions), and dumping the whole input via stringifyJSON
		// pollutes the row — edit's oldString/newString can run to hundreds
		// of runes. Pick the most informative single field instead, mirroring
		// bridgebase.SummarizeToolInput's priority table. The helper lives
		// here (not in bridgebase) because the SDK is a lower layer.
		if s := extractToolInputField(p.State.Input); s != "" {
			result.toolInput = s
		} else {
			result.toolInput = stringifyJSON(p.State.Input)
		}
	}
	result.text = stringifyContent(p.State.Output)
	// Three failure signals, any one of which flags the result as an error:
	//   - status "error"/"failed": the tool framework itself failed
	//     (timeout, permission denied, missing file, ...)
	//   - metadata.exit != 0: the command exited non-zero. opencode sets
	//     status="completed" even when bash returns 1, so without this
	//     check a failed `cat /nonexistent` renders identically to a
	//     successful `ls` on the card.
	if p.State.Status == "error" || p.State.Status == "failed" || p.State.Metadata.Exit != 0 {
		result.isToolError = true
		// On failure opencode swaps state.output for state.error; the
		// cause ("File not found", "PermissionDenied", ...) lives there.
		// Surface it as the result text so the card shows why, not just
		// that, the tool failed.
		if p.State.Error != "" {
			result.text = p.State.Error
		}
	}
	// Append the diffstat to edit's tool-input summary so a tool row reads
	// "tmp/x.txt (+1 -1)" instead of just the path. Cheap, single-place
	// formatting; not promoted to a structured field because the renderer
	// has no +N/-M slot today.
	if p.Tool == "edit" {
		fd := p.State.Metadata.FileDiff
		if fd.Additions > 0 || fd.Deletions > 0 {
			result.toolInput = fmt.Sprintf("%s (+%d -%d)", result.toolInput, fd.Additions, fd.Deletions)
		}
	}
	return []Event{result}
}

// extractToolInputField picks the most informative single string field from a
// tool input JSON, mirroring bridgebase.SummarizeToolInput's priority list.
// Used when opencode did not populate part.title, so a fallback to
// stringifyJSON does not dump the whole input (notably edit's oldString/
// newString, which can run to hundreds of runes). Returns "" when no priority
// field is present; the caller falls back to stringifyJSON for unknown tool
// shapes.
func extractToolInputField(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, key := range []string{"file_path", "filePath", "command", "pattern", "path", "query", "description"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// stringifyContent normalises a tool output field (string or content-block array).
func stringifyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" || blk.Type == "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return strings.TrimSpace(string(raw))
}

// stringifyJSON returns a compacted JSON string for a raw input payload.
func stringifyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}

// extractErrorMessage decodes the "error" field of an error event, which
// opencode emits in two shapes depending on version:
//
//   - legacy: a plain string ("err field msg")
//   - 1.18+:  a structured object {"name":"APIError","data":{
//     "message":"model not allowed for this key",
//     "statusCode":403, "isRetryable":false, ...}}
//
// Returns the most informative message available: data.message (with the
// HTTP status appended when present), falling back to name, then "". An empty
// return lets the caller fall back to head.Message / head.Type as before.
func extractErrorMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try the legacy string form first; older CLIs and some error subtypes
	// still send a bare string.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Structured form: {name, data:{message, statusCode,...}}.
	var obj struct {
		Name string `json:"name"`
		Data struct {
			Message    string `json:"message"`
			StatusCode int    `json:"statusCode"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		switch {
		case obj.Data.Message != "" && obj.Data.StatusCode != 0:
			return fmt.Sprintf("%s (HTTP %d)", obj.Data.Message, obj.Data.StatusCode)
		case obj.Data.Message != "":
			return obj.Data.Message
		case obj.Name != "":
			return obj.Name
		}
	}
	return ""
}
