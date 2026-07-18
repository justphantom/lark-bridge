package feishufront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// handleSSE registers the backend on connect, then streams protocol.Events as
// `data: <json>\n\n` frames until the request context is cancelled or the conn
// is closed. On exit it UnregisterIfMatch so a reconnect's new conn is never
// evicted by the old handler.
func (s *IPCServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.URL.Query().Get("backendID")
	typ := r.URL.Query().Get("backendType")
	if id == "" || typ == "" {
		http.Error(w, "missing backendID or backendType", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Register before sending the 200 response: the client's Connect returns
	// as soon as it reads the response headers, so any caller that immediately
	// dispatches (e.g. a test, or a backend whose first POST loses the race
	// with the SSE handler goroutine) must observe the backend as registered.
	conn := s.registry.Register(id, typ)
	defer func() {
		// If UnregisterIfMatch actually removed the conn (the backend
		// genuinely disconnected, not a reconnect-overwrite), fire onOffline
		// to release this backend's in-flight turns. Without this a deploy
		// that stops the backend strands turns until the 90s health-check
		// eviction — /v1/status reports stale inflight while /running is
		// empty (the backend restarted with a fresh cancelByChat).
		//
		// Store wasOffline so a quick reconnect (before the health check
		// would evict) still fires onOnline — without this the health path
		// and the SSE-exit path diverge, and a crash-restart leaves chats
		// with an "offline" notice but no matching "recovered" notice.
		if s.registry.UnregisterIfMatch(id, conn) {
			s.wasOffline.Store(id, struct{}{})
			s.fireCallback(s.onOffline.Load(), id, typ, "offline")
		}
	}()

	// If this backend was previously evicted (offline), notify that it's back.
	if _, was := s.wasOffline.LoadAndDelete(id); was {
		s.fireCallback(s.onOnline.Load(), id, typ, "online")
	}

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// rc wraps w so we can apply a per-frame write deadline. A stalled peer
	// (full TCP window but live socket) would otherwise block Fprintf/Flush
	// indefinitely, wedging this goroutine and filling the 256-slot eventCh
	// until the health checker eventually evicts. The deadline bounds that
	// window to sseWriteTimeout per frame.
	rc := http.NewResponseController(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-conn.eventCh:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				// A non-marshalable Event would otherwise vanish silently;
				// log so the dropped frame is observable.
				s.logger.Load().Warn("marshal sse event",
					log.FieldEventType, ev.Type,
					log.FieldError, err)
				continue
			}
			if err := rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
				// Writer doesn't support deadlines (e.g. httptest recycler):
				// proceed best-effort without a bound.
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				// The write failed or timed out: the connection is broken, so
				// do NOT Touch (the health checker will evict this conn).
				return
			}
			flusher.Flush()
			// A successful flush proves the backend is reachable: mark it seen.
			conn.Touch()
		}
	}
}
