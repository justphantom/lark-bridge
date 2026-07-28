package feishufront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// preflightResponse is the JSON body of GET /v1/deploy-preflight. Affected
// lists the backendIDs whose in-flight turns a deploy of the requested
// services would disrupt; empty means safe (HTTP 200). Non-empty comes with
// HTTP 409 and a pre-rendered Reason the deploy script prints verbatim.
type preflightResponse struct {
	InFlight int      `json:"inflight"`
	Affected []string `json:"affected"`
	Reason   string   `json:"reason,omitempty"`
}

// handleDeployPreflight reports whether deploying the comma-separated
// ?services= short names (feishu claude opencode miniagent) would disrupt
// in-flight turns: 200 when safe, 409 with the affected backends when not.
// The decision lives here — not in deploy.sh's sed/grep JSON parsing — so it
// is unit-testable and the bash side collapses to one curl + status code.
// Deploy-monitor turns are excluded (a /deploy's own turn must not
// self-block), mirroring TurnManager.InFlight. Frontends older than this
// endpoint answer 404; deploy.sh falls back to the conservative
// inflight>0 check there.
func (s *IPCServer) handleDeployPreflight(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	selected := map[string]bool{}
	for _, svc := range strings.Split(r.URL.Query().Get("services"), ",") {
		if svc = strings.TrimSpace(svc); svc != "" {
			selected[svc] = true
		}
	}
	if len(selected) == 0 {
		http.Error(w, "missing services query", http.StatusBadRequest)
		return
	}

	var turns []Turn
	if fn := s.inFlightDetail.Load(); fn != nil {
		turns = (*fn)()
	}
	considered := make([]Turn, 0, len(turns))
	for _, t := range turns {
		if s.registry.BackendType(t.BackendID) == "deploy-monitor" {
			continue
		}
		considered = append(considered, t)
	}

	affectedSet := map[string]bool{}
	if selected["feishu"] {
		// Restarting feishu-front severs every backend's IPC connection, so
		// every in-flight turn is disrupted regardless of backend.
		for _, t := range considered {
			affectedSet[t.BackendID] = true
		}
	} else {
		for _, t := range considered {
			if selected[stripInstanceSuffix(t.BackendID)] {
				affectedSet[t.BackendID] = true
			}
		}
	}
	affected := make([]string, 0, len(affectedSet))
	for id := range affectedSet {
		affected = append(affected, id)
	}
	sort.Strings(affected)

	resp := preflightResponse{InFlight: len(considered), Affected: affected}
	w.Header().Set("Content-Type", "application/json")
	if len(affected) > 0 {
		resp.Reason = fmt.Sprintf("deploy would disrupt %d in-flight session(s) on backend(s): %s",
			len(affected), strings.Join(affected, ", "))
		w.WriteHeader(http.StatusConflict)
	}
	// Best-effort write: the response is fire-and-forget.
	_ = json.NewEncoder(w).Encode(resp)
}

// stripInstanceSuffix drops a trailing "-<digits>" instance suffix
// ("claude-1" → "claude"), matching deploy.sh's backend_id→service mapping.
// An ID without a numeric suffix is returned unchanged, so unknown backends
// simply never match a service short name (non-conflicting).
func stripInstanceSuffix(backendID string) string {
	i := strings.LastIndexByte(backendID, '-')
	if i <= 0 || i == len(backendID)-1 {
		return backendID
	}
	for _, r := range backendID[i+1:] {
		if r < '0' || r > '9' {
			return backendID
		}
	}
	return backendID[:i]
}
