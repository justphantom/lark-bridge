package feishufront

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// IPCServer is the frontend's HTTP server: it serves the SSE endpoint that
// backends long-connect to receive Events, and the POST endpoint backends
// push Controls through.
type IPCServer struct {
	registry *BackendRegistry
	// server is set by Listen (main goroutine) and read by Shutdown (signal
	// goroutine), so it is stored atomically to avoid a data race.
	server atomic.Pointer[http.Server]
	secret string // shared bearer token; empty disables auth (loopback-only)

	// onOffline, when set, is invoked when the health checker evicts a
	// backend (id, typ). Used by the Dispatcher to post offline notices.
	onOffline atomic.Pointer[func(backendID, backendType string)]

	// onOnline, when set, is invoked when a backend re-connects after being
	// offline (i.e. it was previously evicted and now registers again).
	// First-time connects do not fire onOnline.
	onOnline atomic.Pointer[func(backendID, backendType string)]

	// inFlightTurns, when set, reports the number of currently in-flight turns.
	// Used by GET /v1/status so an operator (e.g. deploy.sh) can avoid
	// restarting the frontend while a conversation is mid-flight. nil when not
	// wired (e.g. unit tests) — the endpoint then reports 0.
	inFlightTurns atomic.Pointer[func() int]

	// inFlightDetail, when set, returns the per-turn detail behind that count
	// (promptID/chatID/backendID/elapsed). GET /v1/status exposes it so a turn
	// stranded by a crashed backend is visible by name, not just as a stale
	// inflight number. nil in unit tests — the endpoint then omits the list.
	inFlightDetail atomic.Pointer[func() []Turn]

	// selfMetrics, when set, returns feishu-front's own host/process snapshot
	// for GET /v1/status (the frontend does not POST /v1/metrics to itself —
	// it reads its own host directly in the handler). nil in unit tests —
	// the endpoint then omits the self row.
	selfMetrics atomic.Pointer[func() (protocol.HostStats, protocol.ServiceStat)]

	// wasOffline tracks backend IDs that were evicted by the health checker,
	// so handleSSE can distinguish a reconnect from a first-time connect.
	//
	// Growth here is bounded by maxWasOffline: when the dead-ID set hits the
	// cap, markOffline resets the map wholesale rather than letting a
	// dynamic-backend-id deployment leak forever. The loss is cosmetic
	// (the next reconnect after a reset looks first-time, so onOnline fires
	// as "online" instead of a "recovered" framing) — NOT a correctness
	// invariant. typical steady-state count is 2-3, far below the cap.
	wasOffline sync.Map // map[string]struct{}

	// logger is stored atomically because SetLogger (main goroutine) and the
	// SSE/callback goroutines read it concurrently; matches the pattern used by
	// onOffline/onOnline above. Defaults to a no-op until main.go wires the real
	// one via SetLogger.
	logger atomic.Pointer[log.Logger]

	// authFailures tracks per-client-IP bearer-auth failure counts for rate
	// limiting (brute-force defence on a short or compromised secret). Bounded
	// by authMaxFailures per IP + a periodic reset on success; map size is
	// further capped by authFailuresCap (defence against an IP-spoofing flood
	// filling the map, since client IP comes from RemoteAddr).
	authFailures sync.Map // map[string]*authFailureState
}

// authFailureState is the per-IP bearer-auth failure tracker.
type authFailureState struct {
	mu       sync.Mutex
	count    int
	lastFail time.Time
}

const (
	// authMaxFailures is the per-IP failure count that triggers a lockout.
	// The shared secret is a 256-bit token in production, so online brute force
	// is infeasible anyway; this is a defence-in-depth rate cap.
	authMaxFailures = 10
	// authLockout is how long an IP stays blocked after hitting authMaxFailures.
	authLockout = 1 * time.Minute
	// authFailuresCap bounds the tracker map size so a connection flood from
	// many spoofed RemoteAddrs cannot leak memory. LRU would be nicer but the
	// legit steady-state set is tiny (a handful of backend IPs).
	authFailuresCap = 256
)

// NewIPCServer wraps a BackendRegistry. secret is the shared bearer token
// every backend must present in its Authorization header; when non-empty,
// SSE and POST endpoints reject requests without it. Pass "" only when the
// listener is bound to loopback and no untrusted process can reach it.
func NewIPCServer(registry *BackendRegistry, secret string) *IPCServer {
	s := &IPCServer{registry: registry, secret: secret}
	s.logger.Store(log.Nop())
	return s
}

// SetLogger wires the component logger. Called by main.go after NewIPCServer;
// nil is rejected to keep s.logger always usable.
func (s *IPCServer) SetLogger(l *log.Logger) {
	if l != nil {
		s.logger.Store(l)
	}
}

// fireCallback invokes a backend online/offline callback in its own goroutine.
// A panic (e.g. inside the Feishu send path) is recovered and logged so a
// transient SDK quirk cannot crash the whole frontend process.
func (s *IPCServer) fireCallback(fn *func(backendID, backendType string), id, typ, kind string) {
	if fn == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Load().Error("backend online/offline callback panic",
					"backend_id", id,
					"notice", kind,
					log.FieldPanic, r)
			}
		}()
		(*fn)(id, typ)
	}()
}

// authOK reports whether r carries the configured bearer token. When no
// secret is configured (loopback-only) every request is accepted. The
// comparison is constant-time to avoid timing oracles, and per-IP failure
// rate limiting blocks an IP for authLockout after authMaxFailures bad
// attempts (brute-force defence on a short or compromised secret).
func (s *IPCServer) authOK(r *http.Request) bool {
	if s.secret == "" {
		return true
	}
	ip := clientIPFromRequest(r)
	if s.isLockedOut(ip) {
		return false
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	ok := strings.HasPrefix(h, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), []byte(s.secret)) == 1
	if ok {
		s.authFailures.Delete(ip)
	} else {
		s.recordAuthFailure(ip)
	}
	return ok
}

// isLockedOut reports whether ip has exceeded authMaxFailures within the
// lockout window.
func (s *IPCServer) isLockedOut(ip string) bool {
	v, ok := s.authFailures.Load(ip)
	if !ok {
		return false
	}
	f, ok := v.(*authFailureState)
	if !ok {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count >= authMaxFailures && time.Since(f.lastFail) < authLockout
}

// recordAuthFailure increments ip's failure counter (resetting it if the
// lockout window has elapsed) and caps the map size.
func (s *IPCServer) recordAuthFailure(ip string) {
	now := time.Now()
	v, _ := s.authFailures.LoadOrStore(ip, &authFailureState{})
	f, ok := v.(*authFailureState)
	if !ok {
		return
	}
	f.mu.Lock()
	if now.Sub(f.lastFail) > authLockout {
		f.count = 0 // window elapsed: fresh count
	}
	f.count++
	f.lastFail = now
	f.mu.Unlock()
	// Bound the map: if it grew past the cap, drop this entry's bookkeeping
	// for stale IPs opportunistically (cheap scan; the legit set is tiny).
	if n := authFailuresMapSize(&s.authFailures); n > authFailuresCap {
		s.authFailures.Range(func(k any, vv any) bool {
			ff, ok := vv.(*authFailureState)
			if !ok {
				return true
			}
			ff.mu.Lock()
			stale := now.Sub(ff.lastFail) > authLockout
			ff.mu.Unlock()
			if stale {
				s.authFailures.Delete(k)
			}
			return true
		})
	}
}

func authFailuresMapSize(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
}

// clientIPFromRequest extracts the peer IP for rate-limiting. It does NOT
// honour X-Forwarded-For (a remote attacker would otherwise pin failures on a
// victim IP to DoS it, or rotate XFF to evade the cap). The listener is
// expected to be loopback or behind a trusted reverse proxy whose rate
// limiting happens upstream.
func clientIPFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoopbackAddr reports whether addr binds to a loopback interface (so a
// missing bearer is acceptable). Empty host ("") counts as loopback since the
// default listen is ":6060" which on Go's listener still means all-interfaces
// — but the project default config explicitly sets 127.0.0.1, and a ":port"
// bind with an empty secret is rejected by deploy-time validation upstream.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// Routes returns the mux serving /v1/events (SSE), /v1/control/{backendID}
// (POST), /v1/metrics/{backendID} (POST), /v1/status (GET), and
// /v1/deploy-preflight (GET). Use this with httptest.NewServer; Listen is
// for production.
func (s *IPCServer) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", s.handleSSE)
	mux.HandleFunc("POST /v1/control/{backendID}", s.handleControl)
	mux.HandleFunc("POST /v1/metrics/{backendID}", s.handleMetrics)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/deploy-preflight", s.handleDeployPreflight)
	return mux
}

// ipcReadHeaderTimeout bounds request-header read time so a slowloris-style
// client cannot pin a connection on POST /v1/control or GET /v1/status. SSE is
// unaffected: its header is read once at connect, then the handler owns the
// long-lived stream under its own per-frame write deadline.
const ipcReadHeaderTimeout = 10 * time.Second

// ipcReadTimeout bounds full request read (header + body) for non-streaming
// endpoints. SSE is unaffected: by the time its handler enters the flush
// loop the request body is already drained, so this deadline never fires on
// a live SSE connection (verified by TestSSESurvivesReadTimeout).
const ipcReadTimeout = 30 * time.Second

// ipcIdleTimeout bounds how long a keep-alive connection may sit idle before
// the server closes it. Backends reconnect promptly, so 120s comfortably
// tolerates a healthy poll cycle while reaping abandoned connections.
const ipcIdleTimeout = 120 * time.Second

// Listen starts the HTTP server and blocks until it exits.
//
// WriteTimeout is intentionally NOT set: it would kill SSE long-polls, which
// write for the connection's whole lifetime. The SSE handler already applies
// a per-frame write deadline (sseWriteTimeout), which is the correct shape
// for streaming endpoints.
func (s *IPCServer) Listen(addr string) error {
	// Refuse to start without auth on a non-loopback address: the bearer
	// token would travel in cleartext and any reachable host could connect.
	// Loopback deployments (the default :6060 on 127.0.0.1) may legitimately
	// run with an empty secret.
	if s.secret == "" && !isLoopbackAddr(addr) {
		return fmt.Errorf("feishufront: ipc_secret is required when IPC binds non-loopback %q (bearer would be cleartext and the endpoint unauthenticated)", addr)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: ipcReadHeaderTimeout,
		ReadTimeout:       ipcReadTimeout,
		IdleTimeout:       ipcIdleTimeout,
	}
	s.server.Store(srv)
	return srv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *IPCServer) Shutdown(ctx context.Context) error {
	srv := s.server.Load()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// SetOnOffline registers a callback invoked when the health checker evicts a
// silent backend. The callback receives the backendID and its type.
func (s *IPCServer) SetOnOffline(fn func(backendID, backendType string)) {
	s.onOffline.Store(&fn)
}

// SetOnOnline registers a callback invoked when a backend reconnects after
// being offline (evicted by the health checker). First-time connects do not
// fire the callback.
func (s *IPCServer) SetOnOnline(fn func(backendID, backendType string)) {
	s.onOnline.Store(&fn)
}

// SetInFlightTurns wires the in-flight turn counter queried by GET /v1/status.
// Pass the Dispatcher/TurnManager's InFlight method (or any func returning the
// current count). When unset, /v1/status reports inflight=0 — deploy-time
// checks treat 0 as "safe to restart".
func (s *IPCServer) SetInFlightTurns(fn func() int) {
	s.inFlightTurns.Store(&fn)
}

// SetInFlightDetail wires the per-turn snapshot queried by GET /v1/status.
// Pass TurnManager.InFlightTurns. When unset, the endpoint omits the turns
// list (back-compat for callers/tests that only need the count).
func (s *IPCServer) SetInFlightDetail(fn func() []Turn) {
	s.inFlightDetail.Store(&fn)
}

// SetSelfMetrics wires feishu-front's own host/process collector for
// GET /v1/status. Called once per status request, so the collector runs
// on-demand rather than on a ticker. When unset, the endpoint omits the
// frontend's self row.
func (s *IPCServer) SetSelfMetrics(fn func() (protocol.HostStats, protocol.ServiceStat)) {
	s.selfMetrics.Store(&fn)
}

// maxWasOffline bounds the dead-backend-ID set. Steady state is 2-3
// (typical fixed backend_id deployment); the cap exists purely to keep a
// dynamic-backend_id deployment from leaking. Hitting it is a deployment
// smell, not a normal mode — but the cap guarantees memory stays bounded
// even in that smell.
const maxWasOffline = 64

// markOffline records id as evicted, with a hard cap on map size. When the
// cap is reached the map is reset wholesale before the new entry is stored;
// the cost is cosmetic (the next reconnect after a reset fires onOnline as
// "online" rather than a "recovered" framing) — never correctness, since
// wasOffline only decides which user-facing notice wording applies.
//
// Range is used for both the count and the reset because sync.Map has no
// Len: each Range is O(n) but n stays small in healthy deployments, and the
// reset path runs at most once per maxWasOffline offline events.
func (s *IPCServer) markOffline(id string) {
	count := 0
	s.wasOffline.Range(func(_, _ any) bool {
		count++
		return count < maxWasOffline // early-exit once we know we are at cap
	})
	if count >= maxWasOffline {
		s.wasOffline.Range(func(k, _ any) bool {
			s.wasOffline.Delete(k)
			return true
		})
	}
	s.wasOffline.Store(id, struct{}{})
}
