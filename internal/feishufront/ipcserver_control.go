package feishufront

import (
	"encoding/json"
	"net/http"

	"github.com/hu/lark-bridge/internal/protocol"
)

// maxControlBody bounds the size of a POSTed Control JSON. The schema is small
// and closed (protocol.Validate), so 1 MiB is ample even for long result text
// while preventing a runaway/compromised backend from driving the frontend OOM.
const maxControlBody = 1 << 20

// handleControl decodes a Control from the body, validates it, requires the
// backendID to be registered, backfills BackendID from the URL path, and
// enqueues it for the frontend main loop.
func (s *IPCServer) handleControl(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("backendID")
	if id == "" {
		http.Error(w, "missing backendID", http.StatusBadRequest)
		return
	}

	// Cap the body so an oversized POST cannot exhaust memory; Decode will
	// surface a "http: request body too large" error past the limit.
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBody)
	var ctrl protocol.Control
	if err := json.NewDecoder(r.Body).Decode(&ctrl); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := ctrl.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, ok := s.registry.Get(id); !ok {
		http.Error(w, "backend not registered", http.StatusServiceUnavailable)
		return
	}
	// BackendID is backfilled by the frontend from the URL path; the backend
	// leaves it empty when sending (see protocol.Control comment).
	ctrl.BackendID = id

	if err := s.registry.ReceiveControl(RoutedControl{BackendID: id, Control: &ctrl}); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// statusResponse is the JSON body returned by GET /v1/status. Kept minimal so
// a deploy script (curl + jq) can consume it cheaply; the operative field is
// InFlight — when >0, an in-progress conversation would be cut off by a
// restart. Backends lists registered backend IDs for operator visibility.
type statusResponse struct {
	InFlight int      `json:"inflight"`
	Backends []string `json:"backends"`
}

// handleStatus reports the current in-flight turn count and registered backends
// so an operator (deploy.sh) can decide whether it is safe to restart. It uses
// the same authOK gate as the other endpoints; an unauthenticated request gets
// 401, which deploy.sh interprets as "service up, check auth" — it must pass
// the configured secret to read the body. When inFlightTurns is unset (nil),
// InFlight is reported as 0 (the safe value for a deploy check).
func (s *IPCServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	inflight := 0
	if fn := s.inFlightTurns.Load(); fn != nil {
		inflight = (*fn)()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statusResponse{
		InFlight: inflight,
		Backends: s.registry.Registered(),
	})
}
