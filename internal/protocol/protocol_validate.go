package protocol

import "fmt"

// allowedEventTypes is the set of valid Event.Type values.
var allowedEventTypes = map[string]struct{}{
	TypePrompt: {},
	TypeAnswer: {},
	TypeAbort:  {},
	TypePing:   {},
}

// allowedControlTypes is the set of valid Control.Type values.
var allowedControlTypes = map[string]struct{}{
	TypeSessionInit: {},
	TypeText:        {},
	TypeThinking:    {},
	TypeToolUse:     {},
	TypeToolResult:  {},
	TypeResult:      {},
	TypeError:       {},
	TypeProgress:    {},
	TypeQuestion:    {},
	TypeNotice:      {},
}

// Validate checks Event consistency:
//   - Type is in the allowed set.
//   - The matching payload is non-nil (TypePing is exempt: payload may be nil).
//   - PromptID is non-empty, except TypePing (heartbeat, no business link) and
//     TypeAnswer (keyed by Answer.RequestID, not PromptID).
//   - Prompt mode requires Prompt.Text and Prompt.ChatID non-empty.
//   - Answer mode requires Answer.RequestID and Answer.ChatID non-empty.
//   - Abort mode requires Abort.ChatID non-empty.
func (e *Event) Validate() error {
	if _, ok := allowedEventTypes[e.Type]; !ok {
		return fmt.Errorf("protocol: invalid event type %q", e.Type)
	}
	// TypePing allows an empty payload AND an empty PromptID (heartbeat).
	if e.Type == TypePing {
		return nil
	}
	// TypeAnswer is keyed by RequestID, so PromptID may be empty.
	if e.Type != TypeAnswer && e.PromptID == "" {
		return fmt.Errorf("protocol: %s event requires promptID", e.Type)
	}
	switch e.Type {
	case TypePrompt:
		if e.Prompt == nil {
			return fmt.Errorf("protocol: %s event requires prompt payload", e.Type)
		}
		if e.Prompt.Text == "" {
			return fmt.Errorf("protocol: prompt requires text")
		}
		if e.Prompt.ChatID == "" {
			return fmt.Errorf("protocol: prompt requires chatID")
		}
	case TypeAnswer:
		if e.Answer == nil {
			return fmt.Errorf("protocol: %s event requires answer payload", e.Type)
		}
		if e.Answer.RequestID == "" {
			return fmt.Errorf("protocol: answer requires requestID")
		}
		if e.Answer.ChatID == "" {
			return fmt.Errorf("protocol: answer requires chatID")
		}
	case TypeAbort:
		if e.Abort == nil {
			return fmt.Errorf("protocol: %s event requires abort payload", e.Type)
		}
		if e.Abort.ChatID == "" {
			return fmt.Errorf("protocol: abort requires chatID")
		}
	}
	return nil
}

// controlRule describes the per-type validation for Control.Validate. The
// payloadIsNil predicate checks that the matching payload field is present,
// payloadName names that field for the error message, and needsChatID flags
// controls that must carry a destination chatID even when sent as standalone
// cards.
type controlRule struct {
	payloadIsNil func(*Control) bool
	payloadName  string
	needsChatID  bool
}

// controlRules maps every allowed control type to its validation rule.
// Keeping the table next to the validator makes the requirements explicit
// and avoids the long switch that was previously needed.
var controlRules = map[string]controlRule{
	TypeSessionInit: {payloadIsNil: func(c *Control) bool { return c.SessionInit == nil }, payloadName: "sessionInit"},
	TypeText:        {payloadIsNil: func(c *Control) bool { return c.Text == nil }, payloadName: "text"},
	TypeThinking:    {payloadIsNil: func(c *Control) bool { return c.Thinking == nil }, payloadName: "thinking"},
	TypeToolUse:     {payloadIsNil: func(c *Control) bool { return c.ToolUse == nil }, payloadName: "toolUse"},
	TypeToolResult:  {payloadIsNil: func(c *Control) bool { return c.ToolResult == nil }, payloadName: "toolResult"},
	TypeResult:      {payloadIsNil: func(c *Control) bool { return c.Result == nil }, payloadName: "result"},
	TypeError:       {payloadIsNil: func(c *Control) bool { return c.Error == nil }, payloadName: "error"},
	TypeProgress:    {payloadIsNil: func(c *Control) bool { return c.Progress == nil }, payloadName: "progress"},
	TypeQuestion:    {payloadIsNil: func(c *Control) bool { return c.Question == nil }, payloadName: "question", needsChatID: true},
	TypeNotice:      {payloadIsNil: func(c *Control) bool { return c.Notice == nil }, payloadName: "notice", needsChatID: true},
}

// Validate checks Control consistency:
//   - Type is in the allowed set.
//   - The matching payload is non-nil.
//   - TypeQuestion / TypeNotice require ChatID (they
//     may be sent as standalone cards not tied to a turn's progress card).
//   - BackendID is NOT checked: it is backfilled by the frontend POST handler
//     from the URL path, so it is empty when the backend calls SendControl.
func (c *Control) Validate() error {
	if _, ok := allowedControlTypes[c.Type]; !ok {
		return fmt.Errorf("protocol: invalid control type %q", c.Type)
	}
	// Every allowed type must have a rule; the map and the allowed set are
	// kept in sync, so a missing rule is a programming error.
	rule, ok := controlRules[c.Type]
	if !ok {
		return fmt.Errorf("protocol: missing validation rule for type %q", c.Type)
	}
	if rule.payloadIsNil(c) {
		return fmt.Errorf("protocol: %s control requires %s payload", c.Type, rule.payloadName)
	}
	if rule.needsChatID && c.ChatID == "" {
		return fmt.Errorf("protocol: %s control requires chatID", c.Type)
	}
	return nil
}
