// Command omp-back runs the Oh My Pi (omp) backend of the lark-bridge split.
//
// It connects to the frontend IPC server over SSE (reading protocol.Event),
// drives `omp -p --mode json ...` subprocesses per turn, and emits
// protocol.Control messages back over POST. Configuration is read from
// -config.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/ompbridge"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/usage"
)

var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", "./omp-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-omp-back %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-omp-back: %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	baseLogger, baseLevel, output, err := buildBaseLogger(cfg)
	if err != nil {
		return err
	}

	// The frontend validates a shared bearer token on every SSE/POST; a
	// backend without the matching secret cannot register or emit controls.
	if err := backendrpc.ValidateBackendConfig(cfg.IPCSecret, cfg.BackendID, cfg.FrontendURL); err != nil {
		return err
	}

	client := omp.New(omp.Options{
		CLIPath:            cfg.OMP.CLIPath,
		AppendSystemPrompt: cfg.OMP.AppendSystemPrompt,
		MaxConcurrent:      cfg.OMP.MaxConcurrent,
		Logger:             componentLogger(cfg, baseLevel, output, "omp"),
	})

	// CLI mode never calls GetOrCreate (sessions are bound lazily from the
	// first run's `session` header), so the router's SessionCreator is nil --
	// mirroring claude-back / opencode-back.
	r, err := router.New(cfg.RouterPath,
		componentLogger(cfg, baseLevel, output, "router"))
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer r.Close()

	// Per-session usage store: accumulates token/cost totals keyed by omp
	// session id. Own file (usage-omp.json) so the claude/opencode backends
	// sharing this state_dir never contend on the write.
	usageStore, err := usage.New(filepath.Join(cfg.StateDir, "usage-omp.json"), componentLogger(cfg, baseLevel, output, "usage"), time.Duration(cfg.Timeouts.UsageSessionTTL))
	if err != nil {
		return fmt.Errorf("usage store: %w", err)
	}
	defer usageStore.Close()

	// Startup health gate: fail fast if the omp CLI is not installed.
	if err := client.IsReady(context.Background()); err != nil {
		return fmt.Errorf("omp CLI health check: %w", err)
	}

	connOpts := backendrpc.ConnectOptions{
		BackendID:   cfg.BackendID,
		BackendType: "omp",
		FrontendURL: cfg.FrontendURL,
		Secret:      cfg.IPCSecret,
		Version:     version,
	}
	rpc, err := backendrpc.Connect(connOpts)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(componentLogger(cfg, baseLevel, output, "rpc"))
	defer rpc.Close()

	bridgeLogger := componentLogger(cfg, baseLevel, output, "bridge")
	h := ompbridge.NewWithLogger(r, client, rpc, ompbridge.HandlerConfig{
		DefaultDirectory: cfg.OMP.DefaultDirectory,
		StateDir:         cfg.StateDir,
		StreamHistory:    cfg.OMP.StreamHistory,
		PromptTimeout:    time.Duration(cfg.Timeouts.PromptTimeout),
		IdleTimeout:      time.Duration(cfg.Timeouts.IdleTimeout),
		DebugRedact:      cfg.LogDebugRedact,
		WorkspaceRoot:    os.Getenv("WORKSPACE_ROOT"),
		ApprovalDefault:  cfg.OMP.ApprovalMode,
		ThinkingDefault:  cfg.OMP.ThinkingLevel,
		ModelOptions:     cfg.OMP.ModelOptions,
		ApprovalOptions:  cfg.OMP.ApprovalOptions,
		ThinkingOptions:  cfg.OMP.ThinkingOptions,
	}, bridgeLogger)
	h.SetUsage(usageStore)
	defer h.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Host/process metrics push for the status-monitor overview card.
	go backendrpc.StartMetricsLoop(ctx, rpc, backendrpc.MetricsOptions{
		Interval: time.Duration(cfg.StatusMonitor.Interval),
		StateDir: cfg.StateDir,
		UnitName: "lark-omp-back.service",
		Version:  version,
		Logger:   baseLogger,
	})

	baseLogger.Info("omp-back ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"cli_path", cfg.OMP.CLIPath)

	eventErr := func(err error) {
		baseLogger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, connOpts,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				baseLogger.Error("handle event", "event_type", ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
}

// buildBaseLogger builds the base logger and level var shared by component
// loggers.
func buildBaseLogger(cfg *config.Config) (*log.Logger, *log.LevelVar, io.Writer, error) {
	lvl, err := log.FromString(cfg.LogLevel)
	if err != nil {
		return nil, nil, nil, err
	}
	var output io.Writer = os.Stderr
	if cfg.LogOutput == "stdout" {
		output = os.Stdout
	}
	if cfg.LogFormat == "json" {
		return log.NewJSON(lvl, output, "omp-back"), lvl, output, nil
	}
	return log.New(lvl, output, "omp-back"), lvl, output, nil
}

// componentLogger builds a component-tagged logger, applying any per-component
// level override from cfg; falls back to baseLevel on an invalid override.
func componentLogger(cfg *config.Config, baseLevel *log.LevelVar, output io.Writer, component string) *log.Logger {
	level := cfg.LogLevel
	if override := getComponentLevel(cfg, component); override != "" {
		level = override
	}
	levelVar, err := log.FromString(level)
	if err != nil {
		levelVar = baseLevel
	}
	if cfg.LogFormat == "json" {
		return log.NewJSON(levelVar, output, component)
	}
	return log.New(levelVar, output, component)
}

func getComponentLevel(cfg *config.Config, component string) string {
	switch component {
	case "router":
		return cfg.ComponentLogLevels.Router
	case "omp":
		return cfg.ComponentLogLevels.Omp
	case "feishu":
		return cfg.ComponentLogLevels.Feishu
	case "bridge":
		return cfg.ComponentLogLevels.Bridge
	case "dedup":
		return cfg.ComponentLogLevels.Dedup
	default:
		return ""
	}
}
