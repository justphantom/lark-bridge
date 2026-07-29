package bridgebase

import (
	"strings"
	"testing"
)

func TestNonEmpty(t *testing.T) {
	if got := NonEmpty("x", "fallback"); got != "x" {
		t.Fatalf("got %q want %q", got, "x")
	}
	if got := NonEmpty("  ", "fallback"); got != "fallback" {
		t.Fatalf("got %q want %q", got, "fallback")
	}
}

func TestTruncateForDebug(t *testing.T) {
	if got := TruncateForDebug("secret", true); got != "<redacted>" {
		t.Fatalf("redact: got %q", got)
	}
	long := strings.Repeat("a", MaxDebugTextLen+50)
	if got := TruncateForDebug(long, false); len(got) > MaxDebugTextLen+3 {
		t.Fatalf("truncate len = %d, want <= %d", len(got), MaxDebugTextLen+3)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("a", "b"); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := FirstNonEmpty("", "b"); got != "b" {
		t.Fatalf("got %q", got)
	}
	if got := FirstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveModel(t *testing.T) {
	if got := ResolveModel("stream", "spec", "fallback"); got != "stream" {
		t.Fatalf("stream wins: got %q", got)
	}
	if got := ResolveModel("", "spec", "fallback"); got != "spec" {
		t.Fatalf("spec fallback: got %q", got)
	}
	if got := ResolveModel("", "", "omp"); got != "omp" {
		t.Fatalf("named fallback: got %q", got)
	}
}

func TestParseTodoItems(t *testing.T) {
	if _, ok := ParseTodoItems(""); ok {
		t.Fatal("empty → false")
	}
	if _, ok := ParseTodoItems("{}"); ok {
		t.Fatal("{} → false")
	}
	if _, ok := ParseTodoItems("not json"); ok {
		t.Fatal("invalid json → false")
	}
	if _, ok := ParseTodoItems(`{"todos":[]}`); ok {
		t.Fatal("empty array → false")
	}
	items, ok := ParseTodoItems(`{"todos":[{"content":"a","status":"pending"}]}`)
	if !ok {
		t.Fatal("valid → true")
	}
	if len(items) != 1 || items[0].Content != "a" {
		t.Fatalf("decoded wrong: %+v", items)
	}
}
