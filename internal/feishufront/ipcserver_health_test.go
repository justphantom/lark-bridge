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
