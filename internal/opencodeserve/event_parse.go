package opencodeserve

import (
	"encoding/json"
	"fmt"
	"strings"
)

// sseFrame is the wire shape of one `data: {...}` line from /event. The
// opencode serve server emits one JSON object per frame with a top-level
// "type" and a "properties" payload whose shape varies per type.
type sseFrame struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// partFrame is the "properties" payload for message.part.* events.
type partFrame struct {
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
	MessageID string          `json:"messageID"` // part.delta only
	PartID    string          `json:"partID"`    // part.delta only
	Field     string          `json:"field"`     // part.delta only
	Delta     string          `json:"delta"`     // part.delta only
	Time      int64           `json:"time"`
}

// partBody is the "part" payload inside message.part.updated. The "type"
// field discriminates text / tool / step-start / step-finish / reason.
// messageID is required by the opencode Part schema and tags the event to
// the assistant message it belongs to.
type partBody struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"` // type=text
	ID        string          `json:"id"`   // part id
	MessageID string          `json:"messageID"`
	Tool      string          `json:"tool"`   // type=tool
	State     json.RawMessage `json:"state"`  // type=tool
	Reason    string          `json:"reason"` // type=step-finish
	Tokens    json.RawMessage `json:"tokens"` // type=step-finish
	Cost      float64         `json:"cost"`   // type=step-finish
}

// toolState is the "state" payload inside a type=tool part.
type toolState struct {
	Status string          `json:"status"` // pending|running|completed|failed
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"` // present on completion
	Error  json.RawMessage `json:"error"`  // present on failure
}

// tokenSet is the "tokens" payload inside a step-finish part.
type tokenSet struct {
	Total     int        `json:"total"`
	Input     int        `json:"input"`
	Output    int        `json:"output"`
	Reasoning int        `json:"reasoning"`
	Cache     tokenCache `json:"cache"`
}

type tokenCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// sessionInfo is the "info" payload inside session.* events. Only the
// fields the bridge reads are modelled; the rest are ignored.
type sessionInfo struct {
	ID    string `json:"id"`
	Model struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
}

// sessionFrame is the "properties" payload for session.* events.
type sessionFrame struct {
	SessionID string      `json:"sessionID"`
	Info      sessionInfo `json:"info"`
}

// parseFrame converts one SSE frame into at most one Event. Returns ok=false
// when the frame is not interesting (heartbeat, plugin.added, catalog.updated,
// user-message echo, etc.) and should be dropped by the caller.
//
// part-updated frames yield:
//   - step-start  → EventStepStart
//   - step-finish → EventStepFinish (reason != "stop") or EventResult
//     (reason == "stop"), carrying tokens/cost
//   - tool        → EventToolUse (state.status pending/running) or
//     EventToolResult (state.status completed/failed)
//   - text        → dropped; deltas already carry the text incrementally
//
// part-delta frames yield:
//   - field=text     → EventText
//   - field=reasoning→ EventThinking
//   - other fields   → dropped
//
// session.created yields EventSession. session.idle is handled by the client
// (it triggers the synthesised result on streams that did not see a
// step-finish reason=stop).
func parseFrame(frame sseFrame) (Event, bool) {
	switch frame.Type {
	case "session.created":
		ev := parseSessionCreated(frame.Properties)
		return ev, ev.kind != ""
	case "message.part.updated":
		ev := parsePartUpdated(frame.Properties)
		return ev, ev.kind != ""
	case "message.part.delta":
		ev := parsePartDelta(frame.Properties)
		return ev, ev.kind != ""
	}
	return Event{}, false
}

func parseSessionCreated(raw json.RawMessage) Event {
	var p sessionFrame
	if err := json.Unmarshal(raw, &p); err != nil {
		return Event{kind: EventError, text: "session.created: " + err.Error(), isError: true}
	}
	model := p.Info.Model.ID
	if model == "" {
		model = p.Info.Model.ProviderID
	}
	return Event{
		kind:      EventSession,
		sessionID: p.SessionID,
		text:      model,
	}
}

func parsePartUpdated(raw json.RawMessage) Event {
	var p partFrame
	if err := json.Unmarshal(raw, &p); err != nil {
		return Event{}
	}
	// part is required; an empty part is a malformed frame we drop silently
	// rather than turning into a spurious error event.
	var body partBody
	if err := json.Unmarshal(p.Part, &body); err != nil {
		return Event{}
	}
	switch body.Type {
	case "step-start":
		return Event{kind: EventStepStart, sessionID: p.SessionID, messageID: body.MessageID}
	case "step-finish":
		return parseStepFinish(p.SessionID, body.MessageID, body)
	case "tool":
		return parseToolPart(p.SessionID, body.MessageID, body)
	}
	return Event{}
}

func parseStepFinish(sessionID, messageID string, body partBody) Event {
	ev := Event{kind: EventStepFinish, sessionID: sessionID, messageID: messageID, cost: body.Cost}
	if len(body.Tokens) > 0 {
		var t tokenSet
		if err := json.Unmarshal(body.Tokens, &t); err == nil {
			ev.inputTokens = t.Input
			ev.outputTokens = t.Output
			ev.cacheRead = t.Cache.Read
			ev.cacheWrite = t.Cache.Write
		}
	}
	// A stop-terminated step is the terminal event for a turn; surface it as
	// EventResult so the bridge's stream loop reduces it to a terminal
	// promptResult (the result text is filled in by the client from the
	// accumulated text deltas — see stream.go).
	if body.Reason == "stop" || body.Reason == "" {
		ev.kind = EventResult
	}
	return ev
}

func parseToolPart(sessionID, messageID string, body partBody) Event {
	ev := Event{sessionID: sessionID, messageID: messageID, toolName: body.Tool}
	if len(body.State) == 0 {
		return Event{}
	}
	var st toolState
	if err := json.Unmarshal(body.State, &st); err != nil {
		return Event{}
	}
	ev.toolInput = compactJSON(body.State, st.Input)
	switch st.Status {
	case "pending", "running":
		ev.kind = EventToolUse
	case "completed":
		ev.kind = EventToolResult
		ev.text = compactJSON(body.State, st.Output)
	case "failed", "error":
		ev.kind = EventToolResult
		ev.isToolError = true
		ev.text = compactJSON(body.State, st.Error)
	default:
		return Event{}
	}
	return ev
}

// compactJSON returns the raw input/output/error sub-doc as a compact string.
// state is the parent state doc (passed for diagnostics on fallback); sub is
// the field we actually want. A nil/empty sub yields "" so callers render a
// blank cell rather than "null".
func compactJSON(_ json.RawMessage, sub json.RawMessage) string {
	if len(sub) == 0 || string(sub) == "null" {
		return ""
	}
	var dst any
	if err := json.Unmarshal(sub, &dst); err != nil {
		return strings.TrimSpace(string(sub))
	}
	out, err := json.Marshal(dst)
	if err != nil {
		return strings.TrimSpace(string(sub))
	}
	return string(out)
}

func parsePartDelta(raw json.RawMessage) Event {
	var p partFrame
	if err := json.Unmarshal(raw, &p); err != nil {
		return Event{}
	}
	switch p.Field {
	case "text":
		return Event{kind: EventText, sessionID: p.SessionID, messageID: p.MessageID, text: p.Delta}
	case "reasoning":
		return Event{kind: EventThinking, sessionID: p.SessionID, messageID: p.MessageID, text: p.Delta}
	}
	return Event{}
}

// parseEventLine parses one `data: ...` SSE line (without the "data: " prefix)
// into a frame and then an Event. ok=false means the line is uninteresting
// and should be dropped. A parse error on a non-empty line returns an
// EventError so a malformed stream surfaces immediately rather than stalling.
func parseEventLine(line string) (Event, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Event{}, false
	}
	var frame sseFrame
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		return Event{kind: EventError, text: fmt.Sprintf("parse sse frame: %s", err), isError: true}, true
	}
	if frame.Type == "" {
		return Event{}, false
	}
	return parseFrame(frame)
}

// ParseEvent decodes one SSE data-line payload into a slice of Events.
// Exported so tests in other packages can build Events from real serve
// frames without constructing Event structs (whose fields are unexported).
// Returns at most one Event per frame; the slice form mirrors the CLI
// bridge's ParseEvent signature for call-site parity. A frame that parses
// but is uninteresting (heartbeat, plugin.added, etc.) yields an empty slice
// and a nil error.
func ParseEvent(line string) ([]Event, error) {
	ev, ok := parseEventLine(line)
	if !ok {
		return nil, nil
	}
	if ev.kind == EventError && ev.isError {
		return nil, fmt.Errorf("%s", ev.text)
	}
	return []Event{ev}, nil
}
