// Command peri-back runs the peri backend of the lark-bridge split.
//
// It connects to the frontend IPC server over SSE (reading protocol.Event),
// drives `peri -p --output-format stream-json` subprocesses per turn, and
// emits protocol.Control messages back over POST. Configuration is read from
// -config.
//
// peri print mode is stateless: each turn is independent with no session
// continuity. This is an accepted design constraint.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/config"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/peri"
	"github.com/hu/lark-bridge/internal/peribridge"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", "./peri-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-peri-back %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-peri-back: %v\n", err)
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

	if cfg.IPCSecret == "" {
		return fmt.Errorf("ipc_secret is required (frontend IPC rejects connections without it)")
	}
	if cfg.BackendID == "" {
		return fmt.Errorf("backend_id is required")
	}
	if cfg.FrontendURL == "" {
		return fmt.Errorf("frontend_url is required")
	}

	client := peri.New(peri.Config{
		CLIPath:          cfg.Peri.CLIPath,
		DefaultDirectory: cfg.Peri.DefaultDirectory,
		MaxConcurrent:    cfg.Peri.MaxConcurrent,
		MaxTurns:         cfg.Peri.MaxTurns,
	}, componentLogger(cfg, baseLevel, output, "peri"))

	// peri is stateless: sessions are never created up front (no GetOrCreate),
	// so the router's SessionCreator is nil — mirroring opencode-back.
	r, err := router.New(nil, cfg.RouterPath,
		componentLogger(cfg, baseLevel, output, "router"))
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer r.Close()

	if err := client.IsReady(context.Background()); err != nil {
		return fmt.Errorf("peri CLI health check: %w", err)
	}

	rpc, err := backendrpc.Connect(cfg.BackendID, "peri", cfg.FrontendURL, cfg.IPCSecret)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(componentLogger(cfg, baseLevel, output, "rpc"))
	defer rpc.Close()

	bridgeLogger := componentLogger(cfg, baseLevel, output, "bridge")
	h := peribridge.NewWithLogger(r, client, rpc, peribridge.HandlerConfig{
		DefaultDirectory:  cfg.Peri.DefaultDirectory,
		StateDir:          cfg.StateDir,
		StreamHistory:     cfg.Peri.StreamHistory,
		PromptTimeout:     time.Duration(cfg.Timeouts.PromptTimeout),
		PermissionOptions: cfg.Peri.PermissionOptions,
		EffortOptions:     cfg.Peri.EffortOptions,
		SettingsDir:       cfg.Peri.SettingsDir,
		DebugRedact:       cfg.LogDebugRedact,
		WorkspaceRoot:     os.Getenv("WORKSPACE_ROOT"),
	}, bridgeLogger)
	defer h.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	baseLogger.Info("peri-back ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"cli_path", cfg.Peri.CLIPath)

	eventErr := func(err error) {
		baseLogger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, cfg.BackendID, "peri", cfg.FrontendURL, cfg.IPCSecret,
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
		return log.NewJSON(lvl, output, "peri-back"), lvl, output, nil
	}
	return log.New(lvl, output, "peri-back"), lvl, output, nil
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
	case "peri":
		return cfg.ComponentLogLevels.Peri
	case "bridge":
		return cfg.ComponentLogLevels.Bridge
	case "dedup":
		return cfg.ComponentLogLevels.Dedup
	default:
		return ""
	}
}
