package feishufront

import (
	"testing"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestBackendRegistry_RunningTurnsLifecycle exercises the running-session
// bookkeeping: TypeTurnStarted adds a turn, TypeTurnFinished removes it, and
// MetricsReport snapshots replace the whole set.
func TestBackendRegistry_RunningTurnsLifecycle(t *testing.T) {
	r := NewBackendRegistry()

	r.Register("b1", "miniagent")

	// Start two turns.
	if err := r.StartTurn("b1", protocol.TurnInfo{PromptID: "p1", ChatID: "c1", ElapsedS: 0}); err != nil {
		t.Fatalf("StartTurn p1: %v", err)
	}
	if err := r.StartTurn("b1", protocol.TurnInfo{PromptID: "p2", ChatID: "c2", ElapsedS: 5}); err != nil {
		t.Fatalf("StartTurn p2: %v", err)
	}
	if got := len(r.RunningTurns()); got != 2 {
		t.Fatalf("after start, RunningTurns = %d, want 2", got)
	}

	// Finish one.
	if err := r.FinishTurn("b1", "p1"); err != nil {
		t.Fatalf("FinishTurn p1: %v", err)
	}
	if got := len(r.RunningTurns()); got != 1 {
		t.Fatalf("after finish p1, RunningTurns = %d, want 1", got)
	}

	// MetricsReport snapshot replaces the remaining set.
	if err := r.SetMetrics("b1", &protocol.MetricsReport{
		Turns: []protocol.TurnInfo{
			{PromptID: "p3", ChatID: "c3", ElapsedS: 10},
		},
	}); err != nil {
		t.Fatalf("SetMetrics: %v", err)
	}
	turns := r.RunningTurns()
	if len(turns) != 1 || turns[0].PromptID != "p3" {
		t.Fatalf("after snapshot, RunningTurns = %+v, want one p3 turn", turns)
	}
}

// TestBackendRegistry_RunningTurnsExcludesDeployMonitor mirrors the
// deploy-monitor exclusion: turns owned by a deploy-monitor backend do not
// count as in-flight for /v1/status purposes.
func TestBackendRegistry_RunningTurnsExcludesDeployMonitor(t *testing.T) {
	r := NewBackendRegistry()

	r.Register("mini", "miniagent")
	r.Register("deploy", "deploy-monitor")

	_ = r.StartTurn("mini", protocol.TurnInfo{PromptID: "p1", ChatID: "c1"})
	_ = r.StartTurn("deploy", protocol.TurnInfo{PromptID: "p2", ChatID: "c2"})

	var normal int
	for _, turn := range r.RunningTurns() {
		if r.BackendType(turn.BackendID) != "deploy-monitor" {
			normal++
		}
	}
	if normal != 1 {
		t.Fatalf("non-deploy-monitor turns = %d, want 1", normal)
	}
}

// TestBackendRegistry_StartTurnUnknownBackend rejects updates for unregistered
// backends so a stale control cannot invent turns.
func TestBackendRegistry_StartTurnUnknownBackend(t *testing.T) {
	r := NewBackendRegistry()

	if err := r.StartTurn("ghost", protocol.TurnInfo{PromptID: "p1"}); err == nil {
		t.Fatal("StartTurn for unknown backend should fail")
	}
}

// TestBackendRegistry_ReclaimTurns drops every running turn of one backend
// while leaving other backends' turns intact — the mirror of
// TurnManager.ReclaimBackend so the two in-flight views cannot disagree after
// a stranded-turn reap (a dying backend never emits TypeTurnFinished, so its
// rows would otherwise persist until it reconnects and pushes a snapshot).
func TestBackendRegistry_ReclaimTurns(t *testing.T) {
	r := NewBackendRegistry()
	r.Register("dying", "claude")
	r.Register("healthy", "miniagent")

	_ = r.StartTurn("dying", protocol.TurnInfo{PromptID: "p1", ChatID: "c1"})
	_ = r.StartTurn("dying", protocol.TurnInfo{PromptID: "p2", ChatID: "c1"})
	_ = r.StartTurn("healthy", protocol.TurnInfo{PromptID: "p3", ChatID: "c2"})

	if dropped := r.ReclaimTurns("dying"); dropped != 2 {
		t.Fatalf("ReclaimTurns dropped = %d, want 2", dropped)
	}
	turns := r.RunningTurns()
	if len(turns) != 1 || turns[0].PromptID != "p3" {
		t.Fatalf("after reclaim, RunningTurns = %+v, want only healthy p3", turns)
	}
	// Idempotent: a second reclaim of the same backend drops nothing.
	if dropped := r.ReclaimTurns("dying"); dropped != 0 {
		t.Fatalf("second ReclaimTurns dropped = %d, want 0", dropped)
	}
	// Unknown backend is a safe no-op.
	if dropped := r.ReclaimTurns("ghost"); dropped != 0 {
		t.Fatalf("ReclaimTurns unknown backend dropped = %d, want 0", dropped)
	}
}
