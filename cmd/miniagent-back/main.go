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
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/config"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/miniagent"
	"github.com/hu/lark-bridge/internal/protocol"
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

	if cfg.IPCSecret == "" {
		return fmt.Errorf("ipc_secret is required (frontend IPC rejects connections without it)")
	}
	if cfg.BackendID == "" {
		return fmt.Errorf("backend_id is required")
	}
	if cfg.FrontendURL == "" {
		return fmt.Errorf("frontend_url is required")
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

	llm := &miniagent.HTTPClient{
		APIKey:  cfg.MiniAgent.APIKey,
		BaseURL: cfg.MiniAgent.BaseURL,
		Logger:  logger,
	}
	// Tools register only when workspace_root is set (empty = disabled; the
	// LLM never sees them). All three are bounded to workspace_root:
	//   - read_file  reads text (path escape refused)
	//   - write_file writes/overwrites text + creates parent dirs (0644)
	//   - shell      `sh -c` with cwd pinned + destructive-pattern tripwire
	// webfetch is URL-driven (not path-driven) so it registers unconditionally.
	var tools []miniagent.Tool
	if cfg.MiniAgent.WorkspaceRoot != "" {
		tools = append(tools,
			miniagent.ReadFile{WorkspaceRoot: cfg.MiniAgent.WorkspaceRoot},
			miniagent.WriteFile{WorkspaceRoot: cfg.MiniAgent.WorkspaceRoot},
			miniagent.Shell{WorkspaceRoot: cfg.MiniAgent.WorkspaceRoot},
		)
	}
	tools = append(tools, miniagent.WebFetch{})
	h := miniagent.New(llm, miniagent.LoopConfig{
		Model:     cfg.MiniAgent.Model,
		System:    cfg.MiniAgent.SystemPrompt,
		MaxTokens: cfg.MiniAgent.MaxTokens,
		Tools:     tools,
	}, rpc, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("miniagent ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"base_url", cfg.MiniAgent.BaseURL,
		"model", cfg.MiniAgent.Model,
		"tools", len(tools),
		"workspace_root", cfg.MiniAgent.WorkspaceRoot)

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
	lvl, err := log.FromString(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	var w io.Writer = os.Stderr
	if cfg.LogOutput == "stdout" {
		w = os.Stdout
	}
	if cfg.LogFormat == "json" {
		return log.NewJSON(lvl, w, "miniagent"), nil
	}
	return log.New(lvl, w, "miniagent"), nil
}
