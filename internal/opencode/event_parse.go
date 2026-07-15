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
	Error     string          `json:"error"`
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
			msg = head.Error
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
		result.toolInput = stringifyJSON(p.State.Input)
	}
	result.text = stringifyContent(p.State.Output)
	// Only explicit failure statuses flag an error; "completed" and an
	// in-progress "running" (or an absent status) do not.
	if p.State.Status == "error" || p.State.Status == "failed" {
		result.isToolError = true
	}
	return []Event{result}
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
