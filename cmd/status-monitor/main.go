// Command lark-status-monitor runs the status-monitor backend.
//
// It registers as a backend (backendType "status-monitor") over SSE and, on a
// fixed interval (config status_monitor.interval, default 60s), queries the
// frontend's GET /v1/status and emits a TypeStatusReport Control. The frontend
// broadcasts a standing overview card to every chat bound to this backend,
// PATCHing it each tick and re-sending if a user deleted it. Push-only:
// prompts get an explanatory notice. Configuration is read from -config.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/statusmonitor"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "./status-monitor-config.json", "path to JSON config file")
	showVer := flag.Bool("version", false, "show version information")
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-status-monitor %s\n", version)
		os.Exit(0)
	}
	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-status-monitor: %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, cfgWarns, err := config.LoadWithWarnings(cfgPath)
	if err != nil {
		return err
	}
	logger, err := log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "status-monitor")
	if err != nil {
		return err
	}
	for _, w := range cfgWarns {
		logger.Warn("config warning", "warning", w)
	}

	if err := backendrpc.ValidateBackendConfig(cfg.IPCSecret, cfg.BackendID, cfg.FrontendURL); err != nil {
		return err
	}

	connOpts := backendrpc.ConnectOptions{
		BackendID:   cfg.BackendID,
		BackendType: "status-monitor",
		FrontendURL: cfg.FrontendURL,
		Secret:      cfg.IPCSecret,
		Version:     version,
		// 进程级钉扎一个后端令牌：本进程所有 client（含重连派生的）与所有
		// POST 携带同一令牌，否则 SSE 握手注册的令牌会与 POST 令牌互异，
		// 被前端 validateBackendToken 以 403 拒绝（见 docs/STATUS_CARD_BACKEND_METRICS_FIX.md）。
		BackendToken: backendrpc.NewBackendToken(),
		// M10-1: TLS client config for https frontend_url (CA pinning +
		// optional mTLS client certificate).
		TLSCAFile:         cfg.IPCTLSCAFile,
		TLSClientCertFile: cfg.IPCTLSClientCertFile,
		TLSClientKeyFile:  cfg.IPCTLSClientKeyFile,
	}
	rpc, err := backendrpc.Connect(connOpts)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(logger)
	defer rpc.Close()

	interval := time.Duration(cfg.StatusMonitor.Interval)
	h := statusmonitor.New(statusmonitor.Config{Interval: interval}, rpc, rpc, cfg.BackendID, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("status-monitor ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"interval", interval.String())

	// Refresh loop on its own goroutine: the ticker must keep firing while
	// SSE idles (no inbound events between ticks), and backendrpc.Run must not
	// be blocked. Both share ctx; main's defer stop() cancels the loop on exit.
	go func() { _ = h.Run(ctx) }()

	// Host/process metrics push: this backend is also a metrics producer for
	// the card it renders.
	go backendrpc.StartMetricsLoop(ctx, rpc, backendrpc.MetricsOptions{
		Interval: interval,
		StateDir: cfg.StateDir,
		UnitName: "lark-status-monitor.service",
		Version:  version,
		Logger:   logger,
	})

	eventErr := func(err error) { logger.Warn("ipc", log.FieldError, err) }
	// RunWithClient（而非 Run）：复用上方已 Connect 出的 rpc 作为 SSE +
	// control/metrics POST 的唯一 client，避免再建一个令牌互异的 client
	// 导致 403（见 docs/STATUS_CARD_BACKEND_METRICS_FIX.md）。
	runErr := backendrpc.RunWithClient(ctx, rpc, connOpts,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				logger.Error("handle event", "event_type", ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
	stop() // cancel ctx so the refresh goroutine exits before rpc.Close()
	return runErr
}
