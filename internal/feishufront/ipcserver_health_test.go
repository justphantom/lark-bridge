package feishufront

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestHealthTick_EvictsStaleBackend drives the health-check eviction loop
// (ipcserver_health.go) directly — the path StartHealthCheck runs every
// interval. It had zero direct coverage: the existing onOffline tests exercise
// the SSE-disconnect path, not the periodic ping+evict tick.
//
// Contract: a backend whose lastSeen predates the deadline is pinged and, if
// still silent after the ping flush window, evicted (onOffline fires exactly
// once, with its backend type); a fresh backend is retained.
func TestHealthTick_EvictsStaleBackend(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")

	stale := reg.Register("stale-1", "omp")
	reg.Register("fresh-1", "claude")
	// Backdate the stale conn past the eviction deadline. lastSeen is the
	// atomic a live SSE flush updates; there is no public backdate API, so the
	// in-package test pokes it directly.
	stale.lastSeen.Store(time.Now().Add(-2 * time.Minute).UnixNano())

	var (
		mu  sync.Mutex
		got []evict
	)
	srv.SetOnOffline(func(id, typ string) {
		mu.Lock()
		got = append(got, evict{id: id, typ: typ})
		mu.Unlock()
	})

	srv.healthTick(context.Background(), time.Minute)

	if _, ok := reg.Get("stale-1"); ok {
		t.Error("stale backend was not evicted")
	}
	if _, ok := reg.Get("fresh-1"); !ok {
		t.Error("fresh backend was wrongly evicted")
	}
	// fireCallback dispatches the callback on its own goroutine, so poll for it.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].id != "stale-1" || got[0].typ != "omp" {
		t.Errorf("onOffline = %+v, want exactly one [{stale-1 omp}]", got)
	}
}

// TestHealthTick_KeepsFreshBackend asserts a backend whose
// lastSeen is within the deadline is NOT pinged or evicted even when it has
// seen no traffic during the tick — the freshness cutoff is the deadline, not
// "any inactivity". Guards against a regression where the ping loop evicted a
// merely-idle backend.
func TestHealthTick_KeepsFreshBackend(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	conn := reg.Register("b1", "omp")
	// Idle but within deadline: last seen 10s ago, deadline 60s.
	conn.lastSeen.Store(time.Now().Add(-10 * time.Second).UnixNano())

	evicted := false
	srv.SetOnOffline(func(id, typ string) { evicted = true })

	srv.healthTick(context.Background(), time.Minute)

	if _, ok := reg.Get("b1"); !ok {
		t.Error("fresh-within-deadline backend was evicted")
	}
	if evicted {
		t.Error("onOffline fired for a backend within the deadline")
	}
}

type evict struct {
	id, typ string
}

// TestHealthTick_EvictsDeadlockedBackend (C2): a backend whose SSE pipe stays
// writable (lastSeen keeps refreshing — simulated by Touch) but whose
// consumer loop never answers a TypePong is wedged. The old lastSeen-only
// check could NEVER evict it (its own ping's flush kept lastSeen fresh);
// the missed-pong counter must evict it after maxMissedPongs pings.
func TestHealthTick_EvictsDeadlockedBackend(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")

	dead := reg.Register("dead-1", "omp")
	// Start stale so the first tick pings it.
	dead.lastSeen.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	// The SSE pipe stays writable the whole time: the flush handler Touches
	// lastSeen on every write. Simulate that with a background Toucher —
	// this is exactly why the lastSeen-only check could never evict a
	// wedged backend.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				dead.Touch()
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	var (
		mu  sync.Mutex
		got []evict
	)
	srv.SetOnOffline(func(id, typ string) {
		mu.Lock()
		got = append(got, evict{id: id, typ: typ})
		mu.Unlock()
	})

	for round := 1; round <= maxMissedPongs; round++ {
		srv.healthTick(context.Background(), time.Minute)
		if round < maxMissedPongs {
			if _, ok := reg.Get("dead-1"); !ok {
				t.Fatalf("evicted after %d missed pongs, want survival until %d", round, maxMissedPongs)
			}
		}
	}
	if _, ok := reg.Get("dead-1"); ok {
		t.Error("deadlocked backend not evicted after maxMissedPongs pings")
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].id != "dead-1" {
		t.Errorf("onOffline = %+v, want exactly one [dead-1]", got)
	}
}

// TestHealthTick_KeepsAliveRespondingBackend (C2): a backend that answers
// every ping with a pong (missed-pong counter reset, flush Touch) is never
// evicted, no matter how many ticks pass.
func TestHealthTick_KeepsAliveRespondingBackend(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")

	live := reg.Register("live-1", "omp")
	live.lastSeen.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				live.Touch()
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	srv.SetOnOffline(func(id, _ string) {
		t.Errorf("live backend wrongly evicted: %s", id)
	})

	for range maxMissedPongs + 2 {
		srv.healthTick(context.Background(), time.Minute)
		live.ResetMissedPongs() // TypePong came back on the control channel
	}
	if _, ok := reg.Get("live-1"); !ok {
		t.Error("pong-responding backend was evicted")
	}
}
