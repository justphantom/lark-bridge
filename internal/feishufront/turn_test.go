package feishufront

import (
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
)

// InFlight backs the GET /v1/status deploy check: it returns the total count
// of in-flight turns across all backends so deploy.sh can refuse to restart
// while a conversation is mid-flight.
func TestInFlight(t *testing.T) {
	m := NewTurnManager()
	if got := m.InFlight(); got != 0 {
		t.Fatalf("empty InFlight = %d, want 0", got)
	}
	m.Start("p-a1", "c-1", "m-a1", "", "back-A")
	m.Start("p-a2", "c-2", "m-a2", "", "back-A")
	m.Start("p-b1", "c-3", "m-b1", "", "back-B")
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

// SweepInteractive evicts only bindings older than cardkit.InteractiveTimeout and reports
// their requestIDs so paired card state can be dropped. (M4)
func TestSweepInteractive_TTL(t *testing.T) {
	m := NewTurnManager()
	m.BindInteractive("fresh", "m-fresh", "", "")
	m.BindInteractive("stale", "m-stale", "", "")

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
	m.BindInteractive("r1", "m1", "", "")
	m.BindInteractive("r2", "m2", "", "")
	if expired := m.SweepInteractive(); len(expired) != 0 {
		t.Fatalf("want no expirations, got %v", expired)
	}
}

// Bind/UnbindInteractive roundtrip: a submitted card releases its binding so
// the requestID does not leak.
func TestUnbindInteractive(t *testing.T) {
	m := NewTurnManager()
	m.BindInteractive("r1", "m1", "", "")
	if _, ok := m.InteractiveMessageID("r1"); !ok {
		t.Fatal("binding missing after BindInteractive")
	}
	m.UnbindInteractive("r1")
	if _, ok := m.InteractiveMessageID("r1"); ok {
		t.Fatal("UnbindInteractive did not remove the binding")
	}
}

// ReclaimBackend (called by fireOfflineNotice) finishes every turn owned by one
// backend and returns them so the dispatcher can flip each progress card to a
// failure state. Turns on other backends are untouched. This is the only path
// that releases a turn without a terminal control, so it must be precise.
func TestReclaimBackend(t *testing.T) {
	m := NewTurnManager()
	m.Start("p-A1", "oc_a", "om_A1", "", "back-A")
	m.Start("p-A2", "oc_a", "om_A2", "", "back-A")
	m.Start("p-B1", "oc_b", "om_B1", "", "back-B")

	reclaimed := m.ReclaimBackend("back-A")
	if len(reclaimed) != 2 {
		t.Fatalf("reclaim back-A: want 2 turns, got %d", len(reclaimed))
	}
	ids := map[string]bool{}
	for _, tr := range reclaimed {
		ids[tr.PromptID] = true
	}
	if !ids["p-A1"] || !ids["p-A2"] {
		t.Fatalf("reclaim back-A: want {p-A1,p-A2}, got %v", ids)
	}
	if _, ok := m.Get("p-A1"); ok {
		t.Fatal("p-A1 must be finished after reclaim")
	}
	if _, ok := m.Get("p-A2"); ok {
		t.Fatal("p-A2 must be finished after reclaim")
	}
	// Other backend's turn is untouched.
	if _, ok := m.Get("p-B1"); !ok {
		t.Fatal("p-B1 (other backend) must survive reclaim of back-A")
	}
	// Reclaiming a backend with no turns returns nil, not an empty slice edge case.
	if got := m.ReclaimBackend("back-Z"); len(got) != 0 {
		t.Fatalf("reclaim unknown backend: want 0, got %d", len(got))
	}
	// InFlight count reflects the release (back-B's one turn remains).
	if m.InFlight() != 1 {
		t.Fatalf("InFlight after reclaim: want 1, got %d", m.InFlight())
	}
}
