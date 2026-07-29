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
	"os"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendhost"
	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencode"
	"github.com/justphantom/lark-bridge/internal/opencodebridge"
	"github.com/justphantom/lark-bridge/internal/router"
)

var version = "dev"

// getComponentLevel maps a logical component name to its per-component log
// level override (or "" when no override is set).
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

	runner := buildOpencodeRunner()
	if err := runner.Run(*cfgPath, version); err != nil {
		fmt.Fprintf(os.Stderr, "lark-opencode-back: %v\n", err)
		os.Exit(1)
	}
}

// buildOpencodeRunner assembles the CLIRunner used by main. Factored out so
// tests can drive it without parsing flags.
func buildOpencodeRunner() *backendhost.CLIRunner[*opencodebridge.Handler] {
	return &backendhost.CLIRunner[*opencodebridge.Handler]{
		BackendType:         "opencode",
		UnitName:            "lark-opencode-back.service",
		UsageFile:           "usage-opencode.json",
		ProgramPrefix:       "lark-opencode-back",
		LoggerComponent:     "opencode-back",
		MetricsInterval:     func(cfg *config.Config) time.Duration { return time.Duration(cfg.StatusMonitor.Interval) },
		EventHandlerFactory: func(h *opencodebridge.Handler) backendhost.EventHandler { return h.HandleEvent },
		CloserFactory:       func(h *opencodebridge.Handler) func() { return h.Close },
		ReadyCheck: func(ctx context.Context, cfg *config.Config, base *log.BaseLogger) error {
			client := opencode.New(opencode.Config{
				CLIPath:          cfg.Opencode.CLIPath,
				DefaultDirectory: cfg.Opencode.DefaultDirectory,
				MaxConcurrent:    cfg.Opencode.MaxConcurrent,
				ListCacheTTL:     cfg.Opencode.ListCacheTTL,
			}, base.ComponentLogger("opencode", getComponentLevel(cfg, "opencode")))
			return client.IsReady(ctx)
		},
		BuildHandler: func(r *router.Router, rpc *backendrpc.Client, cfg *config.Config, base *log.BaseLogger) (*opencodebridge.Handler, error) {
			client := opencode.New(opencode.Config{
				CLIPath:          cfg.Opencode.CLIPath,
				DefaultDirectory: cfg.Opencode.DefaultDirectory,
				MaxConcurrent:    cfg.Opencode.MaxConcurrent,
				ListCacheTTL:     cfg.Opencode.ListCacheTTL,
			}, base.ComponentLogger("opencode", getComponentLevel(cfg, "opencode")))
			return opencodebridge.NewWithLogger(r, client, rpc, opencodebridge.HandlerConfig{
				CoreConfig: bridgebase.CoreConfig{
					DefaultDirectory: cfg.Opencode.DefaultDirectory,
					StateDir:         cfg.StateDir,
					StreamHistory:    cfg.Opencode.StreamHistory,
					PromptTimeout:    time.Duration(cfg.Timeouts.PromptTimeout),
					IdleTimeout:      time.Duration(cfg.Timeouts.IdleTimeout),
					DebugRedact:      cfg.LogDebugRedact,
					WorkspaceRoot:    os.Getenv("WORKSPACE_ROOT"),
				},
			}, base.ComponentLogger("bridge", getComponentLevel(cfg, "bridge"))), nil
		},
		LoggerForRouter: func(cfg *config.Config, base *log.BaseLogger) *log.Logger {
			return base.ComponentLogger("router", getComponentLevel(cfg, "router"))
		},
		LoggerForRPC: func(cfg *config.Config, base *log.BaseLogger) *log.Logger {
			return base.ComponentLogger("rpc", "")
		},
	}
}
