// Command opencode-back runs the opencode backend of the lark-bridge split.
//
// It connects to the frontend IPC server over SSE (reading protocol.Event),
// drives `opencode run` subprocesses per turn, and emits protocol.Control
// messages back over POST. Configuration is read from -config.
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

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/config"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/opencode"
	"github.com/hu/lark-bridge/internal/opencodebridge"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
	"github.com/hu/lark-bridge/internal/usage"
)

var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", "./opencode-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-opencode-back %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-opencode-back: %v\n", err)
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
	if cfg.IPCSecret == "" {
		return fmt.Errorf("ipc_secret is required (frontend IPC rejects connections without it)")
	}
	// Fail fast on missing identity/connection: an empty BackendID fails
	// registration with an opaque error, and an empty FrontendURL dials nothing.
	if cfg.BackendID == "" {
		return fmt.Errorf("backend_id is required")
	}
	if cfg.FrontendURL == "" {
		return fmt.Errorf("frontend_url is required")
	}

	client := opencode.New(opencode.Config{
		CLIPath:          cfg.Opencode.CLIPath,
		DefaultDirectory: cfg.Opencode.DefaultDirectory,
		MaxConcurrent:    cfg.Opencode.MaxConcurrent,
		ListCacheTTL:     cfg.Opencode.ListCacheTTL,
	}, componentLogger(cfg, baseLevel, output, "opencode"))

	// CLI mode never calls GetOrCreate (sessions are bound lazily from the
	// first run's session event), so the router's SessionCreator is nil --
	// mirroring claude-back.
	r, err := router.New(nil, cfg.RouterPath,
		componentLogger(cfg, baseLevel, output, "router"))
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer r.Close()

	// Per-session usage store: accumulates token/cost totals keyed by
	// opencode session id. Own file (usage-opencode.json) so the claude
	// backend sharing this state_dir never contends on the write.
	usageStore, err := usage.New(filepath.Join(cfg.StateDir, "usage-opencode.json"), componentLogger(cfg, baseLevel, output, "usage"))
	if err != nil {
		return fmt.Errorf("usage store: %w", err)
	}
	defer usageStore.Close()

	// Startup health gate: fail fast if the opencode CLI is not installed.
	if err := client.IsReady(context.Background()); err != nil {
		return fmt.Errorf("opencode CLI health check: %w", err)
	}

	rpc, err := backendrpc.Connect(cfg.BackendID, "opencode", cfg.FrontendURL, cfg.IPCSecret)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(componentLogger(cfg, baseLevel, output, "rpc"))
	defer rpc.Close()

	bridgeLogger := componentLogger(cfg, baseLevel, output, "bridge")
	h := opencodebridge.NewWithLogger(r, client, rpc, opencodebridge.HandlerConfig{
		DefaultDirectory: cfg.Opencode.DefaultDirectory,
		StateDir:         cfg.StateDir,
		StreamHistory:    cfg.Opencode.StreamHistory,
		PromptTimeout:    time.Duration(cfg.Timeouts.PromptTimeout),
		DebugRedact:      cfg.LogDebugRedact,
		WorkspaceRoot:    os.Getenv("WORKSPACE_ROOT"),
	}, bridgeLogger)
	h.SetUsage(usageStore)
	defer h.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	baseLogger.Info("opencode-back ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"cli_path", cfg.Opencode.CLIPath)

	eventErr := func(err error) {
		baseLogger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, cfg.BackendID, "opencode", cfg.FrontendURL, cfg.IPCSecret,
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
		return log.NewJSON(lvl, output, "opencode-back"), lvl, output, nil
	}
	return log.New(lvl, output, "opencode-back"), lvl, output, nil
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
	case "opencode":
		return cfg.ComponentLogLevels.Opencode
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
