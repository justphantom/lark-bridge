// Package backendhost unifies the lifecycle of the three CLI agent backends
// (claude-back / opencode-back / omp-back): config load, router.New,
// usage.New, backendrpc.Connect, the SIGINT/SIGTERM context, the metrics
// loop, and backendrpc.Run with a HandleEvent callback. Backends supply the
// backend-specific bits (their Handler's constructor, the CLI --version
// ready check, the per-backend usage-store filename, and an optional
// per-component logger configurer) via the CLIRunner fields.
//
// It deliberately does NOT cover deploy-monitor / status-monitor /
// miniagent-back — those have no router, no usage store, no CLI client, and
// structurally different `run()` shapes. Forcing them into the same Runner
// would make every field "optional" and the abstraction non-binding.
package backendhost

import (
	"context"
	"fmt"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/usage"
)

// EventHandler is the per-event entry point backendrpc.Run invokes. The
// bridge's HandleEvent matches this shape exactly.
type EventHandler func(ctx context.Context, ev *protocol.Event) error

// CLIRunner carries the per-backend inputs that CLIRunner.Run cannot infer
// on its own. The Run method handles every other step of the lifecycle
// identically across claude/opencode/omp.
//
// H is the bridge's Handler type. BuildHandler returns it wired with its
// agent client + HandlerConfig; Run then drives its HandleEvent loop. H
// must expose Close() (every bridge Handler does — it forwards to its
// embedded Core).
type CLIRunner[H any] struct {
	// BackendType is sent to the frontend in the registration handshake
	// (e.g. "claude", "opencode", "omp").
	BackendType string
	// UnitName is the systemd unit name pushed in metrics reports (e.g.
	// "lark-claude-back.service").
	UnitName string
	// UsageFile is the filename (relative to cfg.StateDir) of the per-session
	// usage store. Each backend keeps its own file to avoid write contention
	// when multiple backends share a state_dir.
	UsageFile string
	// RouterFile is the filename (relative to cfg.StateDir) of this backend's
	// router persistence file. Each backend keeps its own file to avoid
	// lost-update when multiple backends share a state_dir — mirroring the
	// per-backend UsageFile split. Empty falls back to the shared
	// cfg.RouterPath (legacy behaviour, kept only for backward compat).
	RouterFile string
	// DefaultConfigPath is the -config default if the user supplies none.
	DefaultConfigPath string
	// ProgramPrefix is the binary's display prefix (e.g. "lark-claude-back")
	// used in --version output and error messages.
	ProgramPrefix string
	// LoggerComponent is the component tag for the base logger
	// (e.g. "claude-back").
	LoggerComponent string

	// BuildHandler constructs the bridge Handler from the router, IPC
	// client, config, and base logger. Run wires SetUsage / Close on it
	// automatically; BuildHandler just returns it ready to HandleEvent.
	BuildHandler func(r *router.Router, rpc *backendrpc.Client, cfg *config.Config, base *log.BaseLogger) (H, error)

	// RouterLogger returns the logger the router is constructed with. Nil
	// falls back to base.Logger (the simple path claude-back uses).
	// opencode-back / omp-back supply a per-component override here.
	LoggerForRouter func(cfg *config.Config, base *log.BaseLogger) *log.Logger

	// LoggerForRPC returns the logger the IPC client uses. Nil falls back to
	// base.Logger.
	LoggerForRPC func(cfg *config.Config, base *log.BaseLogger) *log.Logger

	// ReadyCheck runs the CLI --version health gate (failing fast when the
	// CLI is missing). Typically forwards to (*client).IsReady.
	ReadyCheck func(ctx context.Context, cfg *config.Config, base *log.BaseLogger) error

	// EventHandlerFactory extracts the per-event callback from H. Backends
	// supply `func(h H) backendhost.EventHandler { return h.HandleEvent }`.
	EventHandlerFactory func(h H) EventHandler

	// CloserFactory extracts the Close method from H. Backends supply
	// `func(h H) backendhost.Closer { return &hWrapper{h} }` or analogous
	// (most Handler types embed *bridgebase.Core, which exposes Close()).
	CloserFactory func(h H) func()

	// MetricsInterval pulls the metrics loop period out of cfg. Backends
	// typically pass `func(c) time.Duration { return
	// time.Duration(c.StatusMonitor.Interval) }`. Optional: nil skips the
	// metrics loop.
	MetricsInterval func(cfg *config.Config) time.Duration
}

// Run executes the backend's full lifecycle: parse flags, load config,
// build logger, validate, open router + usage store, ready-check the CLI,
// connect to the frontend, build the Handler, start the metrics loop, and
// finally drive backendrpc.Run until the process context is cancelled.
//
// Returns the error that terminated the run (typically ctx.Err() on
// SIGTERM).
func (r *CLIRunner[H]) Run(cfgPath, version string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	base, err := log.NewBaseLogger(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, r.LoggerComponent)
	if err != nil {
		return err
	}

	if err := backendrpc.ValidateBackendConfig(cfg.IPCSecret, cfg.BackendID, cfg.FrontendURL); err != nil {
		return err
	}

	// CLI mode never calls GetOrCreate (sessions are bound lazily from the
	// first run's session/init), so the router's SessionCreator is nil.
	routerLogger := base.Logger
	if r.LoggerForRouter != nil {
		if l := r.LoggerForRouter(cfg, base); l != nil {
			routerLogger = l
		}
	}
	// Per-backend router file (R2): each backend owns its own
	// router-<backend>.v5.json under state_dir so co-located backends stop
	// clobbering one shared file. RouterFile empty falls back to the shared
	// cfg.RouterPath for backward compat. MigrateLegacyBindings carries an
	// existing deployment's bindings forward on the first run after the split.
	routerPath := cfg.RouterPath
	if r.RouterFile != "" {
		routerPath = filepath.Join(cfg.StateDir, r.RouterFile)
		router.MigrateLegacyBindings(routerPath, routerLogger)
	}
	rr, err := router.New(routerPath, routerLogger)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer rr.Close()

	usageStore, err := usage.New(filepath.Join(cfg.StateDir, r.UsageFile), base.Logger, time.Duration(cfg.Timeouts.UsageSessionTTL))
	if err != nil {
		return fmt.Errorf("usage store: %w", err)
	}
	defer usageStore.Close()

	if r.ReadyCheck != nil {
		readyCtx, readyCancel := context.WithCancel(context.Background())
		defer readyCancel()
		if err := r.ReadyCheck(readyCtx, cfg, base); err != nil {
			return fmt.Errorf("%s health: %w", r.BackendType, err)
		}
	}

	connOpts := backendrpc.ConnectOptions{
		BackendID:   cfg.BackendID,
		BackendType: r.BackendType,
		FrontendURL: cfg.FrontendURL,
		Secret:      cfg.IPCSecret,
		Version:     version,
	}
	rpc, err := backendrpc.Connect(connOpts)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpcLogger := base.Logger
	if r.LoggerForRPC != nil {
		if l := r.LoggerForRPC(cfg, base); l != nil {
			rpcLogger = l
		}
	}
	rpc.SetLogger(rpcLogger)
	defer func() { _ = rpc.Close() }()

	h, err := r.BuildHandler(rr, rpc, cfg, base)
	if err != nil {
		return fmt.Errorf("build handler: %w", err)
	}
	if r.CloserFactory != nil {
		defer r.CloserFactory(h)()
	}

	// Wire the usage store on the Handler. Every CLI bridge Handler exposes
	// SetUsage(*usage.Store); we type-assert via an interface so H stays
	// unconstrained in the type parameter.
	if w, ok := any(h).(interface{ SetUsage(*usage.Store) }); ok {
		w.SetUsage(usageStore)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if r.MetricsInterval != nil {
		interval := r.MetricsInterval(cfg)
		if interval > 0 {
			bridgebase.GoSafe(base.Logger, "metrics-loop", func() {
				backendrpc.StartMetricsLoop(ctx, rpc, backendrpc.MetricsOptions{
					Interval: interval,
					StateDir: cfg.StateDir,
					UnitName: r.UnitName,
					Version:  version,
					Logger:   base.Logger,
				})
			})
		}
	}

	base.Logger.Info(r.LoggerComponent+" ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL)

	eventHandler := r.EventHandlerFactory(h)
	// ACK resolver: every CLI bridge Handler embeds *bridgebase.Core, which
	// owns the Acks registry. Type-assert (mirroring SetUsage above) so H
	// stays unconstrained. nil → the ACK is a no-op (EmitTerminal falls back
	// to pure-retry-on-send-error, the safe degradation).
	ackReg := ackRegistryFrom(h)
	eventErr := func(err error) {
		base.Logger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, connOpts,
		func(ctx context.Context, ev *protocol.Event) error {
			// Intercept terminal-delivery ACKs before the bridge sees them:
			// bridges have no ACK case and would error "unknown event type".
			// The ACK resolves the EmitTerminal retry wait keyed by PromptID.
			if ev.Type == protocol.TypeAck && ackReg != nil {
				ackReg.HandleAck(ev.PromptID)
				return nil
			}
			if err := eventHandler(ctx, ev); err != nil {
				base.Logger.Error("handle event", log.FieldEventType, ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
}

// ackRegistryFrom extracts the terminal-delivery ACK registry from a bridge
// Handler via type assertion. Every CLI bridge Handler embeds *bridgebase.Core,
// which exposes AckResolver(). Returns nil for a Handler without it (miniagent
// / future backends), in which case ACKs are silently dropped and the sender
// falls back to pure-retry-on-send-error — the safe degradation.
func ackRegistryFrom[H any](h H) *bridgebase.AckRegistry {
	type ackResolver interface {
		AckResolver() *bridgebase.AckRegistry
	}
	if r, ok := any(h).(ackResolver); ok {
		return r.AckResolver()
	}
	return nil
}
