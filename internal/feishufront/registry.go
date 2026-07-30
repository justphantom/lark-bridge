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
}

func newBackendConn(id, typ string) *BackendConn {
	c := &BackendConn{
		id:      id,
		typ:     typ,
		eventCh: make(chan *protocol.Event, connEventChanBuf),
	}
	c.lastSeen.Store(time.Now().UnixNano())
	return c
}

// Touch marks the connection as seen (a successful SSE flush). Read by the
// health checker to evict silent backends.
func (c *BackendConn) Touch() {
	c.lastSeen.Store(time.Now().UnixNano())
}

// LastSeen returns the last successful-flush time.
func (c *BackendConn) LastSeen() time.Time {
	return time.Unix(0, c.lastSeen.Load())
}

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

// Register registers a new backend connection. If backendID already exists,
// the old connection is closed first and replaced. Returns the new conn.
func (r *BackendRegistry) Register(id, typ string) *BackendConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.conns[id]; ok {
		old.Close()
	}
	conn := newBackendConn(id, typ)
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
	return nil
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
				hosts[i] = h // same host: latest push wins
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
