package feishufront

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// Channel buffer sizes. connEventChanBuf absorbs SSE Event bursts for one
// backend connection; controlChanBuf is the global inbound Control queue. Both
// surface backpressure (a full channel is an error at the producer) rather than
// unbounded queuing.
const (
	connEventChanBuf = 256
	controlChanBuf   = 1024
)

// RoutedControl attaches backendID to a backend-produced Control so the
// frontend dispatcher can route it.
type RoutedControl struct {
	BackendID string
	Control   *protocol.Control
}

// BackendConn represents one registered backend's SSE long connection.
// Exported so that BackendRegistry.Register/Get can return a nameable type;
// all fields remain unexported, accessed only via the methods below.
type BackendConn struct {
	id      string
	typ     string
	eventCh chan *protocol.Event
	mu      sync.Mutex
	closed  bool
	// token is the per-backend session token presented at SSE-register time
	// (see ipcserver). A POST must carry the same token or the frontend
	// rejects it as an impersonation: under the shared bearer secret any one
	// backend could otherwise POST /v1/control/{peerID} and act as a peer
	// (M10-2). Empty for a pre-token (old) backend; the POST handler treats
	// an empty recorded token as "not opted in" and still accepts.
	token string
	// lastSeen is a lock-free scalar (unix-nanos) updated on every successful
	// SSE flush. Kept atomic so Touch (hot flush path) and LastSeen (health
	// check) do not contend with mu, which protects only closed + the channel.
	lastSeen atomic.Int64

	// version is the backend's build version, set once at SSE handshake
	// (empty for pre-metrics backends → rendered "unknown"). Atomic because
	// SetVersion (SSE goroutine) races Snapshot (status handler goroutine).
	version atomic.Pointer[string]
	// metrics is the latest periodic MetricsReport pushed via
	// POST /v1/metrics. Atomic pointer swap: writers are the metrics HTTP
	// handlers, readers are /v1/status. nil until the first push.
	metrics atomic.Pointer[protocol.MetricsReport]

	// runningMu protects runningTurns. The map is keyed by promptID and holds
	// the backend's view of in-flight turns, synchronized by TypeTurnStarted /
	// TypeTurnFinished controls and periodic MetricsReport snapshots.
	runningMu    sync.RWMutex
	runningTurns map[string]protocol.TurnInfo

	// missedPongs counts health pings sent without a TypePong reply (C2
	// app-level heartbeat). lastSeen only proves the SSE pipe is writable —
	// a backend whose consumer loop is wedged still ACKs TCP writes, so the
	// health checker needs this app-level signal to evict it. Reset by the
	// control handler when a TypePong arrives. Lives on the conn (not the
	// server) so a re-registered backend starts at zero automatically.
	missedPongs atomic.Int64
}

func newBackendConn(id, typ, token string) *BackendConn {
	c := &BackendConn{
		id:           id,
		typ:          typ,
		token:        token,
		eventCh:      make(chan *protocol.Event, connEventChanBuf),
		runningTurns: make(map[string]protocol.TurnInfo),
	}
	c.lastSeen.Store(time.Now().UnixNano())
	return c
}

// Token returns the per-backend session token recorded at SSE-register time.
// Empty for a pre-token (old) backend.
func (c *BackendConn) Token() string { return c.token }

// Touch marks the connection as seen (a successful SSE flush). Read by the
// health checker to evict silent backends.
func (c *BackendConn) Touch() {
	c.lastSeen.Store(time.Now().UnixNano())
}

// LastSeen returns the last successful-flush time.
func (c *BackendConn) LastSeen() time.Time {
	return time.Unix(0, c.lastSeen.Load())
}

// BumpMissedPongs records one health ping sent without (yet) a pong reply.
func (c *BackendConn) BumpMissedPongs() { c.missedPongs.Add(1) }

// ResetMissedPongs clears the counter on a TypePong reply.
func (c *BackendConn) ResetMissedPongs() { c.missedPongs.Store(0) }

// MissedPongs returns the current unanswered-ping count.
func (c *BackendConn) MissedPongs() int64 { return c.missedPongs.Load() }

// SendEvent pushes ev onto the connection's event channel. Non-blocking: a
// full channel returns an error so a slow backend cannot stall the caller.
func (c *BackendConn) SendEvent(ev *protocol.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("backend %s disconnected", c.id)
	}
	select {
	case c.eventCh <- ev:
		return nil
	default:
		return fmt.Errorf("backend %s event channel full", c.id)
	}
}

// Close shuts the connection down. Idempotent.
func (c *BackendConn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.eventCh)
}

// BackendRegistry maintains backendID → BackendConn and is the single entry
// point for Control delivery to the frontend.
type BackendRegistry struct {
	mu     sync.RWMutex
	conns  map[string]*BackendConn
	ctrlCh chan RoutedControl
}

// NewBackendRegistry creates an empty registry.
func NewBackendRegistry() *BackendRegistry {
	return &BackendRegistry{
		conns:  make(map[string]*BackendConn),
		ctrlCh: make(chan RoutedControl, controlChanBuf),
	}
}

// Register registers a new backend connection WITHOUT a session token — the
// legacy/pre-token shape used by tests and any path that does not need POST
// impersonation protection. For the live SSE handshake use RegisterWithToken.
// If backendID already exists, the old connection is closed first and replaced.
// Returns the new conn.
func (r *BackendRegistry) Register(id, typ string) *BackendConn {
	return r.RegisterWithToken(id, typ, "")
}

// RegisterWithToken registers a new backend connection bound to a per-backend
// session token. The token binds subsequent POSTs to this connection (M10-2):
// under the shared bearer secret any one compromised backend could otherwise
// POST /v1/control/{peerID} and impersonate a peer, so the POST handler
// rejects a request whose token does not match the conn that registered the
// backendID. An empty token (Register / pre-token backend) opts out — the POST
// handler treats an empty recorded token as "legacy" and still accepts.
func (r *BackendRegistry) RegisterWithToken(id, typ, token string) *BackendConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.conns[id]; ok {
		old.Close()
	}
	conn := newBackendConn(id, typ, token)
	r.conns[id] = conn
	return conn
}

// Unregister removes and closes the connection for id (forced; used by health
// checks to evict a backend). Returns true when a connection was actually
// removed, false when id was already gone — so callers (e.g. the health
// checker firing onOffline) can avoid acting on a stale eviction.
func (r *BackendRegistry) Unregister(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	conn, ok := r.conns[id]
	if !ok {
		return false
	}
	conn.Close()
	delete(r.conns, id)
	return true
}

// UnregisterIfMatch removes and closes the connection for id ONLY when the
// conn currently bound to id is the same pointer as conn. Used by the SSE
// handler on exit: a reconnect calls Register with a NEW conn that overwrites
// map[id], so the old handler's deferred UnregisterIfMatch sees cur != conn
// and does nothing, leaving the new connection intact. Returns true when a
// connection was actually removed (the backend genuinely disconnected), so
// the SSE handler can fire onOffline to release that backend's in-flight
// turns — without this, a deploy that stops the backend leaves the turns
// stranded until the 90s health-check eviction catches up.
func (r *BackendRegistry) UnregisterIfMatch(id string, conn *BackendConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.conns[id]; ok && cur == conn {
		conn.Close()
		delete(r.conns, id)
		return true
	}
	return false
}

// Get looks up the connection for id.
func (r *BackendRegistry) Get(id string) (*BackendConn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	conn, ok := r.conns[id]
	return conn, ok
}

// BackendType returns the registered type ("claude"/"opencode") for id, used
// by the dispatcher to render the card header. Returns "" when id is unknown.
func (r *BackendRegistry) BackendType(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if conn, ok := r.conns[id]; ok {
		return conn.typ
	}
	return ""
}

// Registered returns the IDs of every currently-connected backend, for listing
// in commands like /backend list.
func (r *BackendRegistry) Registered() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.conns))
	for id := range r.conns {
		out = append(out, id)
	}
	return out
}

// SetVersion records the backend's build version from the SSE handshake.
// Unknown id is a no-op (a handshake whose Register was superseded).
func (r *BackendRegistry) SetVersion(id, v string) {
	r.mu.RLock()
	conn, ok := r.conns[id]
	r.mu.RUnlock()
	if !ok || v == "" {
		return
	}
	conn.version.Store(&v)
}

// SetMetrics stores the latest MetricsReport for id. An unregistered id (SSE
// not connected) is rejected so a forged push cannot invent a backend row.
func (r *BackendRegistry) SetMetrics(id string, m *protocol.MetricsReport) error {
	r.mu.RLock()
	conn, ok := r.conns[id]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("backend %s not registered", id)
	}
	conn.metrics.Store(m)
	// MetricsReport carries the authoritative running-sessions snapshot for
	// this backend; replace the local view so lost TypeTurnStarted/Finished
	// controls self-heal on the next tick. Backfill BackendID because older
	// backends may leave it empty.
	turns := make([]protocol.TurnInfo, len(m.Turns))
	for i, t := range m.Turns {
		t.BackendID = id
		turns[i] = t
	}
	conn.replaceRunningTurns(turns)
	return nil
}

// StartTurn records one in-flight turn for backend id. Idempotent for the same
// promptID: a retried TypeTurnStarted does not duplicate the row.
func (r *BackendRegistry) StartTurn(id string, t protocol.TurnInfo) error {
	r.mu.RLock()
	conn, ok := r.conns[id]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("backend %s not registered", id)
	}
	t.BackendID = id
	conn.startTurn(t)
	return nil
}

// FinishTurn removes one in-flight turn from backend id by promptID.
func (r *BackendRegistry) FinishTurn(id, promptID string) error {
	r.mu.RLock()
	conn, ok := r.conns[id]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("backend %s not registered", id)
	}
	conn.finishTurn(promptID)
	return nil
}

// RunningTurns returns every in-flight turn reported by online backends.
// The result is a snapshot; callers must not mutate it.
func (r *BackendRegistry) RunningTurns() []protocol.TurnInfo {
	r.mu.RLock()
	conns := make([]*BackendConn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.RUnlock()
	var out []protocol.TurnInfo
	for _, c := range conns {
		out = append(out, c.runningTurnsSnapshot()...)
	}
	return out
}

// startTurn records a running turn under the write lock.
func (c *BackendConn) startTurn(t protocol.TurnInfo) {
	c.runningMu.Lock()
	defer c.runningMu.Unlock()
	if c.runningTurns == nil {
		c.runningTurns = make(map[string]protocol.TurnInfo)
	}
	c.runningTurns[t.PromptID] = t
}

// finishTurn removes a running turn under the write lock.
func (c *BackendConn) finishTurn(promptID string) {
	c.runningMu.Lock()
	defer c.runningMu.Unlock()
	if c.runningTurns == nil {
		return
	}
	delete(c.runningTurns, promptID)
}

// ReclaimTurns drops every in-flight turn recorded for backend id and returns
// how many were dropped. Called when a backend is reclaimed after going
// offline for the whole notice-debounce window: its stranded turns can never
// finish, and leaving them in runningTurns would keep them visible (e.g. via
// RunningTurns) after TurnManager already reaped its own view. Unknown id is a
// no-op (0).
func (r *BackendRegistry) ReclaimTurns(id string) int {
	r.mu.RLock()
	conn, ok := r.conns[id]
	r.mu.RUnlock()
	if !ok {
		return 0
	}
	conn.runningMu.Lock()
	defer conn.runningMu.Unlock()
	n := len(conn.runningTurns)
	conn.runningTurns = nil
	return n
}

// replaceRunningTurns atomically replaces the stored turn set with the
// authoritative snapshot from a MetricsReport.
func (c *BackendConn) replaceRunningTurns(turns []protocol.TurnInfo) {
	c.runningMu.Lock()
	defer c.runningMu.Unlock()
	next := make(map[string]protocol.TurnInfo, len(turns))
	for _, t := range turns {
		next[t.PromptID] = t
	}
	c.runningTurns = next
}

// runningTurnsSnapshot returns a copy of the current turn set.
func (c *BackendConn) runningTurnsSnapshot() []protocol.TurnInfo {
	c.runningMu.RLock()
	defer c.runningMu.RUnlock()
	if len(c.runningTurns) == 0 {
		return nil
	}
	out := make([]protocol.TurnInfo, 0, len(c.runningTurns))
	for _, t := range c.runningTurns {
		out = append(out, t)
	}
	return out
}

// Snapshot aggregates the registry's metrics into per-host rows deduped by a
// host identity key, and per-service rows. feishu-front itself is NOT
// included; the status handler merges its own row separately (it does not
// POST to itself).
//
// Dedup key priority: machine-id (stable per physical host, identical for
// every backend on it) → (IP, Hostname) for hosts without a machine-id (rare:
// non-systemd / stripped image) → backendID when both are empty (keeps rows
// distinct instead of collapsing unrelated backends). Same key = same host:
// the latest push wins, matching the old IP-only behavior on a single host.
func (r *BackendRegistry) Snapshot() (hosts []protocol.HostStats, services []protocol.ServiceStat) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	hostIdx := map[string]int{}
	for id, conn := range r.conns {
		var version string
		if v := conn.version.Load(); v != nil {
			version = *v
		}
		svc := protocol.ServiceStat{BackendID: id, Version: version}
		m := conn.metrics.Load()
		if m != nil {
			svc.IP = m.IP
			svc.CgroupMemBytes = m.CgroupMemBytes
			svc.ReportedAt = m.ReportedAt
			if svc.Version == "" {
				svc.Version = m.Version // fallback for a handshake-less deploy
			}
			h := m.Host
			h.IP = m.IP
			if h.Hostname == "" {
				h.Hostname = m.Hostname
			}
			if h.MachineID == "" {
				h.MachineID = m.MachineID // 顶层与 Host 同值；防御性回填
			}
			if h.ReportedAt == 0 {
				h.ReportedAt = m.ReportedAt
			}
			key := hostDedupKey(h.IP, h.Hostname, h.MachineID, id)
			if i, ok := hostIdx[key]; ok {
				// Same host: the push with the highest ReportedAt wins.
				// r.conns is a map (random iteration order), so "latest" must
				// be decided by ReportedAt, not by which backend the range
				// happens to visit last — otherwise the dedup winner is
				// nondeterministic across runs.
				if h.ReportedAt >= hosts[i].ReportedAt {
					hosts[i] = h
				}
			} else {
				hostIdx[key] = len(hosts)
				hosts = append(hosts, h)
			}
		}
		services = append(services, svc)
	}
	return hosts, services
}

// hostDedupKey derives the per-host dedup key. machine-id wins (stable per
// physical host); absent → (IP, Hostname); both empty → backendID so distinct
// backends never collapse into one row. Shared with mergeHostByKey so the
// frontend's self-row uses the identical priority.
func hostDedupKey(ip, hostname, machineID, backendID string) string {
	if machineID != "" {
		return machineID
	}
	if ip != "" || hostname != "" {
		return ip + "|" + hostname
	}
	return backendID
}

// SendEvent pushes an Event to the named backend.
func (r *BackendRegistry) SendEvent(id string, ev *protocol.Event) error {
	conn, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("backend %s not registered", id)
	}
	return conn.SendEvent(ev)
}

// ReceiveControl enqueues a RoutedControl for the frontend main loop.
// Non-blocking: a full ctrlCh returns an error.
func (r *BackendRegistry) ReceiveControl(rc RoutedControl) error {
	select {
	case r.ctrlCh <- rc:
		return nil
	default:
		return fmt.Errorf("global control channel full")
	}
}

// Controls returns the read-only channel the frontend main loop consumes.
func (r *BackendRegistry) Controls() <-chan RoutedControl { return r.ctrlCh }

// connSnapshot is one entry returned by EachConn for the health checker.
type connSnapshot struct {
	ID   string
	Type string
}

// EachConn invokes fn for every currently-registered backend connection. Used
// by the health checker to ping and to evict silent backends.
func (r *BackendRegistry) EachConn(fn func(s connSnapshot)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, c := range r.conns {
		fn(connSnapshot{ID: id, Type: c.typ})
	}
}
