// Command opencode-serve-back runs the opencode-serve backend of the
// lark-bridge split.
//
// Unlike opencode-back (CLI subprocess mode), this backend talks to a
// running `opencode serve` HTTP server over JSON+SSE. It connects to the
// frontend IPC server over SSE (reading protocol.Event), POSTs each prompt
// to the serve server as an async session message, and emits
// protocol.Control messages back over POST. Configuration is read from
// -config; the serve server itself is operator-managed and its base URL is
// supplied via opencode_serve.base_url.
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
	"github.com/justphantom/lark-bridge/internal/opencodeserve"
	"github.com/justphantom/lark-bridge/internal/opencodeservebridge"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/usage"
)

var version = "dev"

const backendType = "opencode-serve"

func main() {
	var (
		cfgPath = flag.String("config", "./opencode-serve-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-opencode-serve-back %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-opencode-serve-back: %v\n", err)
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

	client, err := opencodeserve.New(opencodeserve.Config{
		BaseURL:       cfg.OpencodeServe.BaseURL,
		MaxConcurrent: cfg.OpencodeServe.MaxConcurrent,
		ListCacheTTL:  cfg.OpencodeServe.ListCacheTTL,
	}, componentLogger(cfg, baseLevel, output, "opencode"))
	if err != nil {
		return fmt.Errorf("opencode serve client: %w", err)
	}
	client.SetLogger(componentLogger(cfg, baseLevel, output, "opencode"))
	defer client.Close()

	r, err := router.New(cfg.RouterPath,
		componentLogger(cfg, baseLevel, output, "router"))
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer r.Close()

	// Per-session usage store: accumulates token/cost totals keyed by
	// opencode session id. Own file (usage-opencode-serve.json) so it never
	// contends with the CLI-mode backend's write.
	usageStore, err := usage.New(filepath.Join(cfg.StateDir, "usage-opencode-serve.json"), componentLogger(cfg, baseLevel, output, "usage"))
	if err != nil {
		return fmt.Errorf("usage store: %w", err)
	}
	defer usageStore.Close()

	// Startup health gate: fail fast if the serve server is unreachable.
	if err := client.IsReady(context.Background()); err != nil {
		return fmt.Errorf("opencode serve health check: %w", err)
	}

	rpc, err := backendrpc.Connect(cfg.BackendID, backendType, cfg.FrontendURL, cfg.IPCSecret)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(componentLogger(cfg, baseLevel, output, "rpc"))
	defer rpc.Close()

	bridgeLogger := componentLogger(cfg, baseLevel, output, "bridge")
	h := opencodeservebridge.NewWithLogger(r, client, rpc, opencodeservebridge.HandlerConfig{
		StateDir:      cfg.StateDir,
		StreamHistory: cfg.OpencodeServe.StreamHistory,
		PromptTimeout: time.Duration(cfg.Timeouts.PromptTimeout),
		DebugRedact:   cfg.LogDebugRedact,
		WorkspaceRoot: os.Getenv("WORKSPACE_ROOT"),
	}, bridgeLogger)
	h.SetUsage(usageStore)
	defer h.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	baseLogger.Info("opencode-serve-back ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"base_url", cfg.OpencodeServe.BaseURL)

	eventErr := func(err error) {
		baseLogger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, cfg.BackendID, backendType, cfg.FrontendURL, cfg.IPCSecret,
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
		return log.NewJSON(lvl, output, "opencode-serve-back"), lvl, output, nil
	}
	return log.New(lvl, output, "opencode-serve-back"), lvl, output, nil
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
