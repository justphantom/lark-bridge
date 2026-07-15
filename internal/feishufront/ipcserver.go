package feishufront

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hu/lark-bridge/internal/log"
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

	// wasOffline tracks backend IDs that were evicted by the health checker,
	// so handleSSE can distinguish a reconnect from a first-time connect.
	//
	// Growth here is bounded by the number of distinct backend IDs ever seen
	// (typically 2-3 in practice). Each entry is consumed (LoadAndDelete) when
	// its backend reconnects, so only permanently-dead backends accumulate —
	// an acceptable, small leak that does not warrant a TTL sweep.
	wasOffline sync.Map // map[string]struct{}

	// logger is stored atomically because SetLogger (main goroutine) and the
	// SSE/callback goroutines read it concurrently; matches the pattern used by
	// onOffline/onOnline above. Defaults to a no-op until main.go wires the real
	// one via SetLogger.
	logger atomic.Pointer[log.Logger]
}

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
// comparison is constant-time to avoid timing oracles.
func (s *IPCServer) authOK(r *http.Request) bool {
	if s.secret == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), []byte(s.secret)) == 1
}

// Routes returns the mux serving /v1/events (SSE), /v1/control/{backendID}
// (POST), and /v1/status (GET). Use this with httptest.NewServer; Listen is
// for production.
func (s *IPCServer) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", s.handleSSE)
	mux.HandleFunc("POST /v1/control/{backendID}", s.handleControl)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	return mux
}

// ipcReadHeaderTimeout bounds request-header read time so a slowloris-style
// client cannot pin a connection on POST /v1/control or GET /v1/status. SSE is
// unaffected: its header is read once at connect, then the handler owns the
// long-lived stream under its own per-frame write deadline.
const ipcReadHeaderTimeout = 10 * time.Second

// Listen starts the HTTP server and blocks until it exits.
func (s *IPCServer) Listen(addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Routes(), ReadHeaderTimeout: ipcReadHeaderTimeout}
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
