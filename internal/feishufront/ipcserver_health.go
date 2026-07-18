package feishufront

import (
	"context"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// sseWriteTimeout bounds how long one SSE frame may take to write+flush. A
// healthy backend drains near-instantly; a stalled peer (full TCP window,
// paused debugger) would otherwise block the handler goroutine until the
// health checker evicts it (~2min), during which the eventCh fills and every
// subsequent Event — including new prompts — is dropped. The deadline lets
// the handler return early so the conn is unregistered and the backend can
// reconnect cleanly.
const sseWriteTimeout = 15 * time.Second

// healthPingWait is how long StartHealthCheck sleeps after sending a ping
// before re-reading lastSeen: the SSE handler updates lastSeen synchronously
// on flush, so a brief yield lets a healthy backend refresh its timestamp
// before the eviction pass.
const healthPingWait = 50 * time.Millisecond

// StartHealthCheck spawns a goroutine that, every interval, pings every
// registered backend (driving an SSE flush that updates lastSeen) and evicts
// any backend whose lastSeen is older than timeout. Blocks the caller until
// ctx is done (run it in its own goroutine).
func (s *IPCServer) StartHealthCheck(ctx context.Context, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.healthTick(ctx, timeout)
		}
	}
}

// healthTick pings each backend and evicts silent ones.
//
// Eviction has three correctness concerns:
//   - Stale snapshot: EachConn copies LastSeen at snapshot time, but a ping
//     flush updates it synchronously, so the timeout re-check must read the
//     live value (registry.Get → conn.LastSeen()), not the snapshot.
//   - Duplicate eviction: the SSE handler's deferred UnregisterIfMatch races
//     with this goroutine's Unregister, so onOffline could fire twice. We
//     only fire onOffline when Unregister actually removed the conn.
//   - EachConn snapshots IDs once; we iterate that stable ID list for both
//     the ping and the re-check so the two passes see the same set.
func (s *IPCServer) healthTick(ctx context.Context, timeout time.Duration) {
	now := time.Now()
	deadline := now.Add(-timeout)

	// Snapshot the IDs once; the ping and the re-check iterate the same set.
	var ids []string
	var types []string
	s.registry.EachConn(func(c connSnapshot) {
		ids = append(ids, c.ID)
		types = append(types, c.Type)
	})

	// Ping every backend whose lastSeen predates the deadline. A failed send
	// (closed/full channel) means the backend is unreachable now.
	for _, id := range ids {
		if c, ok := s.registry.Get(id); ok && c.LastSeen().After(deadline) {
			continue // still fresh from prior traffic; no ping needed
		}
		_ = s.registry.SendEvent(id, &protocol.Event{Type: protocol.TypePing, Ping: &protocol.PingPayload{}})
	}

	// Give the ping a moment to flush through the SSE handler (which updates
	// lastSeen synchronously), then re-check the live lastSeen values.
	select {
	case <-time.After(healthPingWait):
	case <-ctx.Done():
		return
	}

	// Evict: read the live lastSeen so a successful ping is not misjudged.
	for i, id := range ids {
		c, ok := s.registry.Get(id)
		if !ok {
			continue // already removed (e.g. by its own SSE handler exit)
		}
		if c.LastSeen().After(deadline) {
			continue // ping flushed; still healthy
		}
		typ := types[i]
		// Unregister returns false when the SSE handler already removed it;
		// only fire onOffline when we actually evicted, to avoid duplicates.
		if s.registry.Unregister(id) {
			s.wasOffline.Store(id, struct{}{})
			s.fireCallback(s.onOffline.Load(), id, typ, "offline")
		}
	}
}
