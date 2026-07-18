package bridgebase

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestStaticOptions_ReturnsList verifies StaticOptions wraps a fixed list
// into the listFn shape AskAndWait expects (no I/O, ctx is decorative).
func TestStaticOptions_ReturnsList(t *testing.T) {
	fn := StaticOptions([]string{"a", "b", "c"})
	got, err := fn(context.Background())
	if err != nil {
		t.Fatalf("StaticOptions: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("got=%v, want [a b c]", got)
	}
}

// TestStaticOptions_Empty verifies an empty option slice flows through
// (the picker caller must handle len==0 as "nothing to choose", but the
// wrapper itself should not panic or synthesize a placeholder).
func TestStaticOptions_Empty(t *testing.T) {
	fn := StaticOptions(nil)
	got, err := fn(context.Background())
	if err != nil {
		t.Fatalf("StaticOptions(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got=%v, want empty", got)
	}
}

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

// TestNewRequestID verifies the id is non-empty, has the documented prefix,
// and two consecutive calls differ (so a stale card click cannot collide
// with a fresh picker).
func TestNewRequestID(t *testing.T) {
	a, err := newRequestID()
	if err != nil {
		t.Fatalf("newRequestID: %v", err)
	}
	if !strings.HasPrefix(a, "q-") {
		t.Errorf("id=%q, want q- prefix", a)
	}
	if len(a) <= 2 {
		t.Errorf("id=%q too short", a)
	}
	b, _ := newRequestID()
	if a == b {
		t.Errorf("two ids identical: %q (must be unguessable)", a)
	}
}
