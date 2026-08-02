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
	"time"

	"github.com/justphantom/lark-bridge/internal/backendhost"
	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/claudebridge"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
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

	runner := buildClaudeRunner()
	if err := runner.Run(*cfgPath, version); err != nil {
		fmt.Fprintf(os.Stderr, "lark-claude-back: %v\n", err)
		os.Exit(1)
	}
}

// buildClaudeRunner assembles the CLIRunner used by main. Factored out so
// tests can drive it without parsing flags.
func buildClaudeRunner() *backendhost.CLIRunner[*claudebridge.Handler] {
	return &backendhost.CLIRunner[*claudebridge.Handler]{
		BackendType:         "claude",
		UnitName:            "lark-claude-back.service",
		UsageFile:           "usage-claude.json",
		RouterFile:          "router-claude.v5.json",
		ProgramPrefix:       "lark-claude-back",
		LoggerComponent:     "claude-back",
		MetricsInterval:     func(cfg *config.Config) time.Duration { return time.Duration(cfg.StatusMonitor.Interval) },
		EventHandlerFactory: func(h *claudebridge.Handler) backendhost.EventHandler { return h.HandleEvent },
		CloserFactory:       func(h *claudebridge.Handler) func() { return h.Close },
		ReadyCheck: func(ctx context.Context, cfg *config.Config, base *log.BaseLogger) error {
			api := claude.New(claude.Options{
				CLIPath:            cfg.Claude.CLIPath,
				PermissionMode:     cfg.Claude.PermissionMode,
				AppendSystemPrompt: cfg.Claude.AppendSystemPrompt,
				MaxConcurrent:      cfg.Claude.MaxConcurrent,
				SettingsDir:        cfg.Claude.SettingsDir,
				SettingsCacheTTL:   time.Duration(cfg.Claude.SettingsCacheTTL) * time.Second,
				Logger:             base.Logger,
			})
			return api.IsReady(ctx)
		},
		BuildHandler: func(r *router.Router, rpc *backendrpc.Client, cfg *config.Config, base *log.BaseLogger) (*claudebridge.Handler, error) {
			api := claude.New(claude.Options{
				CLIPath:            cfg.Claude.CLIPath,
				PermissionMode:     cfg.Claude.PermissionMode,
				AppendSystemPrompt: cfg.Claude.AppendSystemPrompt,
				MaxConcurrent:      cfg.Claude.MaxConcurrent,
				SettingsDir:        cfg.Claude.SettingsDir,
				SettingsCacheTTL:   time.Duration(cfg.Claude.SettingsCacheTTL) * time.Second,
				Logger:             base.Logger,
			})
			return claudebridge.NewWithLogger(r, api, rpc, claudebridge.HandlerConfig{
				CoreConfig: bridgebase.CoreConfig{
					DefaultDirectory:    cfg.Claude.DefaultDirectory,
					PermissionDefault:   cfg.Claude.PermissionMode,
					StateDir:            cfg.StateDir,
					StreamHistory:       cfg.Claude.StreamHistory,
					StreamArchiveRedact: cfg.RedactStreams(),
					PromptTimeout:       time.Duration(cfg.Timeouts.PromptTimeout),
					DebugRedact:         cfg.LogDebugRedact,
					WorkspaceRoot:       os.Getenv("WORKSPACE_ROOT"),
				},
				ModelOptions:      cfg.Claude.ModelOptions,
				PermissionOptions: cfg.Claude.PermissionOptions,
				EffortOptions:     cfg.Claude.EffortOptions,
			}, base.ComponentLogger("bridge", "")), nil
		},
	}
}
