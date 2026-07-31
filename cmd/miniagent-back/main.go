// Command lark-miniagent-back runs the miniagent backend. Like the claude
// and opencode backends, it shells out to an external agent CLI (the
// miniagent binary at github.com/justphantom/miniagent): each turn forks
// one subprocess that owns the ReAct loop, tool execution, and the LLM
// call. The bridge itself does IPC + slash-command dispatch + event
// forwarding.
//
// miniagent is stateless (post fe85c16): no sessions, no memory, no
// per-chat jsonl. The only persistent per-chat state is the router binding
// (Directory + ModelSpec), stored under {state_dir}/router-miniagent.v5.json.
//
// Configuration is read from -config. The miniagent.api_key field should
// use ${MINIAGENT_API_KEY} so the key is pulled from the environment, not
// committed in the config file.
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

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniagent"
	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", "./miniagent-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-miniagent-back %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-miniagent-back: %v\n", err)
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

	if err := backendrpc.ValidateBackendConfig(cfg.IPCSecret, cfg.BackendID, cfg.FrontendURL); err != nil {
		return err
	}
	if cfg.MiniAgent.APIKey == "" {
		return fmt.Errorf("miniagent.api_key is required (use ${MINIAGENT_API_KEY} in the config)")
	}
	// fail-fast: an empty model makes the miniagent CLI refuse to start
	// (its main.go requires -model non-empty) and exit code 1 surfaces as
	// a confusing "启动 miniagent 失败" on the first prompt.
	if cfg.MiniAgent.Model == "" {
		return fmt.Errorf("miniagent.model is required (use ${MINIAGENT_DEFAULT_MODEL} in the config)")
	}
	// Per-backend router file (R2): without persistence every redeploy resets
	// all per-chat model/directory pins, and sharing one file with the other
	// backends lost-updates it. miniagent now owns
	// {state_dir}/router-miniagent.v5.json; MigrateLegacyBindings carries an
	// existing deployment's bindings forward on the first run after the split.
	// The router_path == "" guard below is a state_dir sanity check
	// (router_path defaults from state_dir in applyDefaults).
	if cfg.RouterPath == "" {
		return fmt.Errorf("router_path is required (set router_path or state_dir in the config)")
	}
	routerPath := filepath.Join(cfg.StateDir, "router-miniagent.v5.json")
	router.MigrateLegacyBindings(routerPath, logger)
	r, err := router.New(routerPath, logger)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer r.Close()

	connOpts := backendrpc.ConnectOptions{
		BackendID:   cfg.BackendID,
		BackendType: "miniagent",
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

	// CLI subprocess mode: miniagent-back forks miniagent per turn.
	// The CLI binary (github.com/justphantom/miniagent) lives alongside
	// this binary in the deploy dir.
	cliPath := filepath.Join(filepath.Dir(os.Args[0]), "miniagent")
	if _, err := os.Stat(cliPath); err != nil { //nolint:gosec // G703: cliPath derives from our own argv[0], not user input
		// Fallback: check /usr/local/bin (make deploy from miniagent repo).
		cliPath = "/usr/local/bin/miniagent"
	}
	client := miniclient.New(miniclient.Config{
		CLIPath:      cliPath,
		APIKey:       cfg.MiniAgent.APIKey,
		BaseURL:      cfg.MiniAgent.BaseURL,
		SystemPrompt: cfg.MiniAgent.SystemPrompt,
		MaxTokens:    cfg.MiniAgent.MaxTokens,
	}, logger)

	h := miniagent.New(rpc, logger, r, cfg.MiniAgent.WorkspaceRoot, cfg.MiniAgent.Model, client, cfg.MiniAgent.StreamHistory, cfg.StateDir, cfg.StreamArchiveRedact)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer h.Close()

	// Host/process metrics push for the status-monitor overview card. The
	// cgroup row is the parent service's memory; fork-on-prompt children are
	// not sampled separately (idle → "—" only when the file is unreadable).
	bridgebase.GoSafe(logger, "metrics-loop", func() {
		backendrpc.StartMetricsLoop(ctx, rpc, backendrpc.MetricsOptions{
			Interval: time.Duration(cfg.StatusMonitor.Interval),
			StateDir: cfg.StateDir,
			UnitName: "lark-miniagent-back.service",
			Version:  version,
			Logger:   logger,
		})
	})

	logger.Info("miniagent ready (CLI mode, stateless)",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"cli_path", cliPath,
		"router_path", cfg.RouterPath,
		"workspace_root", cfg.MiniAgent.WorkspaceRoot)

	eventErr := func(err error) {
		logger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, connOpts,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				logger.Error("handle event", "event_type", ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
}

func buildLogger(cfg *config.Config) (*log.Logger, error) {
	return log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "miniagent")
}
