package miniagent

import (
	"testing"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestPickAnswerValue_CustomWins verifies a custom-typed value overrides a
// selected option — the user explicitly typed something not in the list.
func TestPickAnswerValue_CustomWins(t *testing.T) {
	ans := &protocol.AnswerPayload{Choices: []string{"listed"}, Custom: "typed"}
	if got := PickAnswerValue(ans); got != "typed" {
		t.Errorf("custom should win; got=%q", got)
	}
}

// TestPickAnswerValue_FirstChoice verifies a single-select answer carries
// its value at Choices[0].
func TestPickAnswerValue_FirstChoice(t *testing.T) {
	ans := &protocol.AnswerPayload{Choices: []string{"only"}}
	if got := PickAnswerValue(ans); got != "only" {
		t.Errorf("got=%q, want only", got)
	}
}

// TestPickAnswerValue_Nil verifies a nil AnswerPayload yields "" rather
// than panicking (callers feed inbound answers directly).
func TestPickAnswerValue_Nil(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panicked on nil: %v", r)
		}
	}()
	if got := PickAnswerValue(nil); got != "" {
		t.Errorf("got=%q, want empty", got)
	}
}

// TestPickAnswerValue_Empty verifies an empty AnswerPayload yields "".
func TestPickAnswerValue_Empty(t *testing.T) {
	if got := PickAnswerValue(&protocol.AnswerPayload{}); got != "" {
		t.Errorf("got=%q, want empty", got)
	}
}
