package feishufront

import (
	"encoding/json"
	"net/http"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// maxMetricsBody bounds a POSTed MetricsReport. A normal payload is well
// under 1 KiB; 64 KiB is a generous ceiling that still stops an abusive peer.
const maxMetricsBody = 64 << 10

// handleMetrics accepts a backend's periodic host/process report. It bypasses
// the Control dispatcher entirely (unknown control types are rejected there,
// so a separate endpoint keeps the change non-breaking: an old frontend
// answers 404 and the backend silently skips the push). The backendID must be
// SSE-registered, so a forged push cannot invent a row.
func (s *IPCServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }() // request fully read; close error not actionable
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("backendID")
	if id == "" {
		http.Error(w, "missing backendID", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMetricsBody)
	// Plain Decode (no DisallowUnknownFields): a newer backend may add fields
	// before this frontend upgrades; dropping them beats rejecting the push.
	var report protocol.MetricsReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.registry.SetMetrics(id, &report); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// mergeHostByIP inserts self into hosts, replacing an existing row with the
// same IP (the frontend host may also run a backend that self-reported).
// An empty-IP self row is appended only when no empty-IP row exists.
func mergeHostByIP(hosts []protocol.HostStats, self protocol.HostStats) []protocol.HostStats {
	for i, h := range hosts {
		if h.IP == self.IP {
			hosts[i] = self
			return hosts
		}
	}
	return append(hosts, self)
}
