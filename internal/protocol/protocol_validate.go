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
	TypeTodo:        {},
	TypeQuestion:    {},
	TypePermission:  {},
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
// payloadName names that field for the error message, needsChatID flags
// controls that must carry a destination chatID even when sent as standalone
// cards, and extraCheck runs type-specific semantic validation (enum fields
// like todo.status or notice.level) after the structural checks pass.
type controlRule struct {
	payloadIsNil func(*Control) bool
	payloadName  string
	needsChatID  bool
	extraCheck   func(*Control) error
}

// validTodoStatuses / validTodoPriorities / validNoticeLevels are the enum
// sets referenced by field-level comments in protocol_control.go. Kept here
// (next to the validator) so a contributor adding a new value updates the
// comment and the validator in one place. Empty Priority is valid (omitempty
// field), so it is checked as "either empty or in the set" rather than in.
var (
	validTodoStatuses   = map[string]struct{}{"pending": {}, "in_progress": {}, "completed": {}, "cancelled": {}}
	validTodoPriorities = map[string]struct{}{"high": {}, "medium": {}, "low": {}}
	validNoticeLevels   = map[string]struct{}{"info": {}, "success": {}, "warning": {}, "error": {}}
)

// validateTodo enumerates each Todo's Status and Priority against the enum
// sets. opencode's SDK emits these literal strings, but a backend bug or a
// future SDK schema change could ship an unknown value; failing here names
// the offending field instead of letting the renderer silently drop the
// row or render a placeholder status.
func validateTodo(c *Control) error {
	for _, t := range c.Todo.Todos {
		if _, ok := validTodoStatuses[t.Status]; !ok {
			return fmt.Errorf("todo.status %q must be one of pending/in_progress/completed/cancelled", t.Status)
		}
		if t.Priority != "" {
			if _, ok := validTodoPriorities[t.Priority]; !ok {
				return fmt.Errorf("todo.priority %q must be one of high/medium/low", t.Priority)
			}
		}
	}
	return nil
}

// validateNotice pins Notice.Level to the four levels the renderer's
// noticeTemplate switch understands; an unknown level would fall through to
// the default "grey" template, hiding the level the backend intended.
func validateNotice(c *Control) error {
	if _, ok := validNoticeLevels[c.Notice.Level]; !ok {
		return fmt.Errorf("notice.level %q must be one of info/success/warning/error", c.Notice.Level)
	}
	return nil
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
	TypeTodo:        {payloadIsNil: func(c *Control) bool { return c.Todo == nil }, payloadName: "todo", extraCheck: validateTodo},
	TypeQuestion:    {payloadIsNil: func(c *Control) bool { return c.Question == nil }, payloadName: "question", needsChatID: true},
	TypePermission:  {payloadIsNil: func(c *Control) bool { return c.Permission == nil }, payloadName: "permission", needsChatID: true},
	TypeNotice:      {payloadIsNil: func(c *Control) bool { return c.Notice == nil }, payloadName: "notice", needsChatID: true, extraCheck: validateNotice},
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
	if rule.extraCheck != nil {
		if err := rule.extraCheck(c); err != nil {
			return err
		}
	}
	if rule.needsChatID && c.ChatID == "" {
		return fmt.Errorf("protocol: %s control requires chatID", c.Type)
	}
	return nil
}
