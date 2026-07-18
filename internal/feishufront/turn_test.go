package feishufront

import (
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
)

// TurnsByBackend backs OnBackendOffline cleanup: it returns exactly the
// in-flight promptIDs owned by one backend so a disconnecting backend's turns
// can be released. (M3)
func TestTurnsByBackend(t *testing.T) {
	m := NewTurnManager()
	m.Start("p-a1", "c-1", "m-a1", "back-A")
	m.Start("p-a2", "c-2", "m-a2", "back-A")
	m.Start("p-b1", "c-3", "m-b1", "back-B")

	got := m.TurnsByBackend("back-A")
	if len(got) != 2 {
		t.Fatalf("back-A: want 2 promptIDs, got %v", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen["p-a1"] || !seen["p-a2"] {
		t.Fatalf("back-A: want {p-a1,p-a2}, got %v", got)
	}
	if len(m.TurnsByBackend("back-B")) != 1 {
		t.Fatal("back-B: want 1 promptID")
	}
	if len(m.TurnsByBackend("back-C")) != 0 {
		t.Fatal("unknown backend: want 0 promptIDs")
	}
}

// InFlight backs the GET /v1/status deploy check: it returns the total count
// of in-flight turns across all backends so deploy.sh can refuse to restart
// while a conversation is mid-flight.
func TestInFlight(t *testing.T) {
	m := NewTurnManager()
	if got := m.InFlight(); got != 0 {
		t.Fatalf("empty InFlight = %d, want 0", got)
	}
	m.Start("p-a1", "c-1", "m-a1", "back-A")
	m.Start("p-a2", "c-2", "m-a2", "back-A")
	m.Start("p-b1", "c-3", "m-b1", "back-B")
	if got := m.InFlight(); got != 3 {
		t.Fatalf("after 3 starts InFlight = %d, want 3", got)
	}
	m.Finish("p-a1")
	if got := m.InFlight(); got != 2 {
		t.Fatalf("after finish InFlight = %d, want 2", got)
	}
	m.Finish("p-a2")
	m.Finish("p-b1")
	if got := m.InFlight(); got != 0 {
		t.Fatalf("after all finished InFlight = %d, want 0", got)
	}
}

// InFlight must exclude a deploy-monitor backend's own turn: a /deploy prompt
// runs `make deploy`, which queries /v1/status — counting it back would block
// every deploy. Other backends' turns still count.
func TestInFlight_ExcludesDeployMonitor(t *testing.T) {
	types := map[string]string{
		"back-claude":   "claude",
		"back-deploy":   "deploy-monitor",
		"back-opencode": "opencode",
	}
	m := NewTurnManager()
	m.SetTypeResolver(func(id string) string { return types[id] })

	m.Start("p-c1", "c-1", "m-c1", "back-claude")
	m.Start("p-d1", "c-2", "m-d1", "back-deploy")
	m.Start("p-o1", "c-3", "m-o1", "back-opencode")
	if got := m.InFlight(); got != 2 {
		t.Fatalf("InFlight = %d, want 2 (exclude deploy-monitor)", got)
	}

	// Without a resolver the count falls back to total (back-compat for
	// callers/tests that never wire SetTypeResolver).
	m.SetTypeResolver(nil)
	if got := m.InFlight(); got != 3 {
		t.Fatalf("InFlight without resolver = %d, want 3", got)
	}
}

// SweepInteractive evicts only bindings older than cardkit.InteractiveTimeout and reports
// their requestIDs so paired card state can be dropped. (M4)
func TestSweepInteractive_TTL(t *testing.T) {
	m := NewTurnManager()
	m.BindInteractive("fresh", "m-fresh", "")
	m.BindInteractive("stale", "m-stale", "")

	// Age the "stale" entry past the TTL by rewriting its boundAt directly
	// (same-package test can reach the unexported field).
	m.mu.Lock()
	e := m.interactive["stale"]
	e.boundAt = time.Now().Add(-cardkit.InteractiveTimeout - time.Second)
	m.interactive["stale"] = e
	m.mu.Unlock()

	expired := m.SweepInteractive()
	if len(expired) != 1 || expired[0] != "stale" {
		t.Fatalf("want expired=[stale], got %v", expired)
	}
	if _, ok := m.InteractiveMessageID("stale"); ok {
		t.Fatal("stale binding should have been evicted")
	}
	if _, ok := m.InteractiveMessageID("fresh"); !ok {
		t.Fatal("fresh binding should have been retained")
	}
}

// SweepInteractive is a no-op when every binding is within the TTL.
func TestSweepInteractive_AllFresh(t *testing.T) {
	m := NewTurnManager()
	m.BindInteractive("r1", "m1", "")
	m.BindInteractive("r2", "m2", "")
	if expired := m.SweepInteractive(); len(expired) != 0 {
		t.Fatalf("want no expirations, got %v", expired)
	}
}

// Bind/UnbindInteractive roundtrip: a submitted card releases its binding so
// the requestID does not leak.
func TestUnbindInteractive(t *testing.T) {
	m := NewTurnManager()
	m.BindInteractive("r1", "m1", "")
	if _, ok := m.InteractiveMessageID("r1"); !ok {
		t.Fatal("binding missing after BindInteractive")
	}
	m.UnbindInteractive("r1")
	if _, ok := m.InteractiveMessageID("r1"); ok {
		t.Fatal("UnbindInteractive did not remove the binding")
	}
}

// InteractiveByPromptID backs the result-card finalisation: it returns the
// pending interactive cards linked to a turn so sendResult can flip them to a
// finished state. Bindings with an empty promptID (standalone cards) never
// match, and bindings for other prompts are excluded.
func TestInteractiveByPromptID(t *testing.T) {
	m := NewTurnManager()
	m.BindInteractive("r1", "m1", "p-a")
	m.BindInteractive("r2", "m2", "p-a")
	m.BindInteractive("r3", "m3", "p-b")
	m.BindInteractive("r4", "m4", "") // standalone, no link

	got := m.InteractiveByPromptID("p-a")
	if len(got) != 2 {
		t.Fatalf("p-a: want 2 cards, got %v", got)
	}
	seen := map[string]bool{}
	for _, pair := range got {
		seen[pair[0]] = true
	}
	if !seen["r1"] || !seen["r2"] {
		t.Fatalf("p-a: want {r1,r2}, got %v", got)
	}
	if len(m.InteractiveByPromptID("p-b")) != 1 {
		t.Fatal("p-b: want 1 card")
	}
	if len(m.InteractiveByPromptID("p-c")) != 0 {
		t.Fatal("p-c: want 0 cards")
	}
}
