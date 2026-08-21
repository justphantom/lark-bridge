package ipcserver

import (
	"encoding/json"
	"net/http"

	"github.com/justphantom/lark-bridge/internal/feishufront"
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
	// Per-backend session binding (M10-2): reject a peer impersonating this
	// backendID before decoding the body. SetMetrics re-checks registration.
	if conn, ok := s.registry.Get(id); !ok {
		http.Error(w, "backend not registered", http.StatusNotFound)
		return
	} else if !validateBackendToken(conn, r) {
		http.Error(w, "backend token mismatch", http.StatusForbidden)
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

// mergeHostByKey inserts the frontend's self row into the backend-aggregated
// hosts, replacing an existing row that represents the same physical host
// (the frontend host typically also runs a backend that self-reported).
//
// Matching uses the same priority as Snapshot's dedup (feishufront.HostDedupKey):
// machine-id first, then (IP, Hostname). HostStats carries no backendID, so a
// row that deduped purely by backendID (no machine-id, no IP, no hostname)
// yields key "" and only matches a self that is equally empty — a degenerate
// case that falls through to append. This keeps the self-merge consistent with
// the per-backend dedup so a co-located frontend+backend collapse to one row.
func mergeHostByKey(hosts []protocol.HostStats, self protocol.HostStats) []protocol.HostStats {
	selfKey := feishufront.HostDedupKey(self.IP, self.Hostname, self.MachineID, "")
	for i, h := range hosts {
		if selfKey == "" {
			break
		}
		if feishufront.HostDedupKey(h.IP, h.Hostname, h.MachineID, "") == selfKey {
			hosts[i] = self
			return hosts
		}
	}
	return append(hosts, self)
}
