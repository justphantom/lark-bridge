// Command lark-miniagent-back runs the miniagent backend.
//
// miniagent is a self-contained ReAct agent: unlike claude/opencode (which
// shell out to an external agent CLI), it calls an OpenAI-compatible chat
// completions endpoint directly, drives a tool loop, and emits Controls
// back to the frontend. P0 implements single-turn Q&A; tools, memory, and
// permissions land in later phases.
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

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniagent"
	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/protocol"
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

	rpc, err := backendrpc.Connect(cfg.BackendID, "miniagent", cfg.FrontendURL, cfg.IPCSecret)
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
		CLIPath:              cliPath,
		APIKey:               cfg.MiniAgent.APIKey,
		BaseURL:              cfg.MiniAgent.BaseURL,
		SystemPrompt:         cfg.MiniAgent.SystemPrompt,
		MaxTokens:            cfg.MiniAgent.MaxTokens,
		Permission:           cfg.MiniAgent.Permission,
		ShellBlockedPatterns: cfg.MiniAgent.ShellBlockedPatterns,
	}, logger)

	// CLIState: every state read/write (sessions, pins, memory, list-models)
	// forks the miniagent binary. nil when state-dir is empty (stateless mode).
	var cli *miniagent.CLIState
	memoryEnabled := cfg.MiniAgent.MemoryEnabled == nil || *cfg.MiniAgent.MemoryEnabled
	if memoryEnabled && cfg.StateDir != "" {
		cli = miniagent.NewCLIState(cliPath, cfg.StateDir)
	}
	h := miniagent.New(rpc, logger, cli, cfg.MiniAgent.WorkspaceRoot, cfg.StateDir, client, cfg.MiniAgent.Model, cfg.MiniAgent.Permission)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer h.Close()

	logger.Info("miniagent ready (CLI mode)",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"cli_path", cliPath,
		"memory_enabled", memoryEnabled,
		"workspace_root", cfg.MiniAgent.WorkspaceRoot,
		"permission", cfg.MiniAgent.Permission)

	eventErr := func(err error) {
		logger.Warn("ipc", log.FieldError, err)
	}
	return backendrpc.Run(ctx, cfg.BackendID, "miniagent", cfg.FrontendURL, cfg.IPCSecret,
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
