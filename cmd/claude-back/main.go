// Command claude-back runs the Claude backend of the lark-bridge split.
//
// It connects to the frontend's IPC server over SSE (reading protocol.Event),
// drives one Claude Code CLI turn per prompt, and emits protocol.Control
// messages back over POST. Configuration is read from -config.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/claude"
	"github.com/hu/lark-bridge/internal/claudebridge"
	"github.com/hu/lark-bridge/internal/config"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
	"github.com/hu/lark-bridge/internal/usage"
)

var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", "./claude-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-claude-back %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-claude-back: %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	logger, err := buildLogger(cfg)
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

	// Claude-back uses a nil SessionCreator: sessions are bound lazily and
	// only Bind/Lookup/Set* are exercised (never GetOrCreate).
	r, err := router.New(cfg.RouterPath, logger)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer r.Close()

	// Per-session usage store: accumulates token/cost totals keyed by
	// claude session_id. Own file (usage-claude.json) so the opencode backend
	// sharing this state_dir never contends on the write.
	usageStore, err := usage.New(filepath.Join(cfg.StateDir, "usage-claude.json"), logger)
	if err != nil {
		return fmt.Errorf("usage store: %w", err)
	}
	defer usageStore.Close()

	api := claude.New(cfg.Claude, logger)

	rpc, err := backendrpc.Connect(cfg.BackendID, "claude", cfg.FrontendURL, cfg.IPCSecret)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(logger)
	defer rpc.Close()

	h := claudebridge.NewWithLogger(r, api, rpc, claudebridge.HandlerConfig{
		DefaultDirectory:  cfg.Claude.DefaultDirectory,
		PermissionDefault: cfg.Claude.PermissionMode,
		StateDir:          cfg.StateDir,
		StreamHistory:     cfg.Claude.StreamHistory,
		PromptTimeout:     time.Duration(cfg.Timeouts.PromptTimeout),
		ModelOptions:      cfg.Claude.ModelOptions,
		PermissionOptions: cfg.Claude.PermissionOptions,
		EffortOptions:     cfg.Claude.EffortOptions,
		DebugRedact:       cfg.LogDebugRedact,
		WorkspaceRoot:     os.Getenv("WORKSPACE_ROOT"),
	}, logger)
	h.SetUsage(usageStore)
	defer h.Close()

	// Health gate: fail fast if the Claude CLI is not installed.
	readyCtx, readyCancel := context.WithCancel(context.Background())
	defer readyCancel()
	if err := api.IsReady(readyCtx); err != nil {
		return fmt.Errorf("claude health: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// On signal, cancel ctx so backendrpc.Run's reconnect loop exits instead
	// of hanging on a closed RecvEvent.
	logger.Info("claude-back ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL)

	eventErr := func(err error) {
		logger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, cfg.BackendID, "claude", cfg.FrontendURL, cfg.IPCSecret,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				logger.Error("handle event", log.FieldEventType, ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
}

// buildLogger builds the component logger from cfg.
func buildLogger(cfg *config.Config) (*log.Logger, error) {
	return log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "claude-back")
}
