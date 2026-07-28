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
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	logger, err := log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "status-monitor")
	if err != nil {
		return err
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
	runErr := backendrpc.Run(ctx, connOpts,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				logger.Error("handle event", "event_type", ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
	stop() // cancel ctx so the refresh goroutine exits before rpc.Close()
	return runErr
}
