// Command goose-back runs the goose backend of the lark-bridge split.
//
// It connects to the frontend's IPC server over SSE (reading protocol.Event),
// drives one goose CLI turn per prompt (goose run -i - --output-format
// stream-json), and emits protocol.Control messages back over POST.
// Configuration is read from -config.
//
// goose sessions persist in a global SQLite DB and are resumed by --name: the
// first turn for a chat creates a named session, subsequent turns resume it.
// See internal/goosebridge/deps.go for the session strategy.
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
	"github.com/hu/lark-bridge/internal/goose"
	"github.com/hu/lark-bridge/internal/goosebridge"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
	"github.com/hu/lark-bridge/internal/usage"
)

var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", "./goose-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-goose-back %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-goose-back: %v\n", err)
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

	if cfg.IPCSecret == "" {
		return fmt.Errorf("ipc_secret is required (frontend IPC rejects connections without it)")
	}
	if cfg.BackendID == "" {
		return fmt.Errorf("backend_id is required")
	}
	if cfg.FrontendURL == "" {
		return fmt.Errorf("frontend_url is required")
	}

	// goose-back uses a nil SessionCreator: sessions are bound lazily (the
	// --name anchor is back-filled after the first successful turn).
	r, err := router.New(nil, cfg.RouterPath, logger)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer r.Close()

	// Per-session usage store: accumulates token totals keyed by the goose
	// --name anchor. Own file so backends sharing this state_dir never contend.
	usageStore, err := usage.New(filepath.Join(cfg.StateDir, "usage-goose.json"), logger)
	if err != nil {
		return fmt.Errorf("usage store: %w", err)
	}
	defer usageStore.Close()

	api := goose.New(goose.Config{
		CLIPath:          cfg.Goose.CLIPath,
		DefaultDirectory: cfg.Goose.DefaultDirectory,
		MaxConcurrent:    cfg.Goose.MaxConcurrent,
		MaxTurns:         cfg.Goose.MaxTurns,
	}, logger)

	rpc, err := backendrpc.Connect(cfg.BackendID, "goose", cfg.FrontendURL, cfg.IPCSecret)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(logger)
	defer rpc.Close()

	h := goosebridge.NewWithLogger(r, api, rpc, goosebridge.HandlerConfig{
		DefaultDirectory:  cfg.Goose.DefaultDirectory,
		StateDir:          cfg.StateDir,
		StreamHistory:     cfg.Goose.StreamHistory,
		PromptTimeout:     time.Duration(cfg.Timeouts.PromptTimeout),
		ModelOptions:      cfg.Goose.ModelOptions,
		PermissionOptions: cfg.Goose.PermissionOptions,
		EffortOptions:     cfg.Goose.EffortOptions,
		DebugRedact:       cfg.LogDebugRedact,
		WorkspaceRoot:     os.Getenv("WORKSPACE_ROOT"),
	}, logger)
	h.SetUsage(usageStore)
	defer h.Close()

	// Health gate: fail fast if the goose CLI is not installed.
	readyCtx, readyCancel := context.WithCancel(context.Background())
	defer readyCancel()
	if err := api.IsReady(readyCtx); err != nil {
		return fmt.Errorf("goose health: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("goose-back ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL)

	eventErr := func(err error) {
		logger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, cfg.BackendID, "goose", cfg.FrontendURL, cfg.IPCSecret,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				logger.Error("handle event", log.FieldEventType, ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
}

// buildLogger builds the component logger from cfg.
func buildLogger(cfg *config.Config) (*log.Logger, error) {
	lvl, err := log.FromString(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	var w io.Writer = os.Stderr
	if cfg.LogOutput == "stdout" {
		w = os.Stdout
	}
	if cfg.LogFormat == "json" {
		return log.NewJSON(lvl, w, "goose-back"), nil
	}
	return log.New(lvl, w, "goose-back"), nil
}
