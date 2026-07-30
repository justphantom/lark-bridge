// Command omp-back runs the Oh My Pi (omp) backend of the lark-bridge split.
//
// It connects to the frontend IPC server over SSE (reading protocol.Event),
// drives `omp -p --mode json ...` subprocesses per turn, and emits
// protocol.Control messages back over POST. Configuration is read from
// -config.
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
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/ompbridge"
	"github.com/justphantom/lark-bridge/internal/router"
)

var version = "dev"

// getComponentLevel maps a logical component name to its per-component log
// level override (or "" when no override is set). Shared by the BuildHandler /
// ReadyCheck callbacks below.
func getComponentLevel(cfg *config.Config, component string) string {
	switch component {
	case "router":
		return cfg.ComponentLogLevels.Router
	case "omp":
		return cfg.ComponentLogLevels.Omp
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
		cfgPath = flag.String("config", "./omp-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-omp-back %s\n", version)
		os.Exit(0)
	}

	runner := buildOmpRunner()
	if err := runner.Run(*cfgPath, version); err != nil {
		fmt.Fprintf(os.Stderr, "lark-omp-back: %v\n", err)
		os.Exit(1)
	}
}

// buildOmpRunner assembles the CLIRunner used by main. Factored out so
// tests can drive it without parsing flags.
func buildOmpRunner() *backendhost.CLIRunner[*ompbridge.Handler] {
	return &backendhost.CLIRunner[*ompbridge.Handler]{
		BackendType:         "omp",
		UnitName:            "lark-omp-back.service",
		UsageFile:           "usage-omp.json",
		ProgramPrefix:       "lark-omp-back",
		LoggerComponent:     "omp-back",
		MetricsInterval:     func(cfg *config.Config) time.Duration { return time.Duration(cfg.StatusMonitor.Interval) },
		EventHandlerFactory: func(h *ompbridge.Handler) backendhost.EventHandler { return h.HandleEvent },
		CloserFactory:       func(h *ompbridge.Handler) func() { return h.Close },
		ReadyCheck: func(ctx context.Context, cfg *config.Config, base *log.BaseLogger) error {
			client := omp.New(omp.Options{
				CLIPath:            cfg.OMP.CLIPath,
				AppendSystemPrompt: cfg.OMP.AppendSystemPrompt,
				MaxConcurrent:      cfg.OMP.MaxConcurrent,
				ListTimeout:        time.Duration(cfg.OMP.ModelListTimeout),
				ListCacheTTL:       time.Duration(cfg.OMP.ListCacheTTL) * time.Second,
				Logger:             base.ComponentLogger("omp", getComponentLevel(cfg, "omp")),
			})
			return client.IsReady(ctx)
		},
		BuildHandler: func(r *router.Router, rpc *backendrpc.Client, cfg *config.Config, base *log.BaseLogger) (*ompbridge.Handler, error) {
			client := omp.New(omp.Options{
				CLIPath:            cfg.OMP.CLIPath,
				AppendSystemPrompt: cfg.OMP.AppendSystemPrompt,
				MaxConcurrent:      cfg.OMP.MaxConcurrent,
				ListTimeout:        time.Duration(cfg.OMP.ModelListTimeout),
				ListCacheTTL:       time.Duration(cfg.OMP.ListCacheTTL) * time.Second,
				Logger:             base.ComponentLogger("omp", getComponentLevel(cfg, "omp")),
			})
			return ompbridge.NewWithLogger(r, client, rpc, ompbridge.HandlerConfig{
				CoreConfig: bridgebase.CoreConfig{
					DefaultDirectory:    cfg.OMP.DefaultDirectory,
					PermissionDefault:   cfg.OMP.ApprovalMode,
					StateDir:            cfg.StateDir,
					StreamHistory:       cfg.OMP.StreamHistory,
					StreamArchiveRedact: cfg.StreamArchiveRedact,
					PromptTimeout:       time.Duration(cfg.Timeouts.PromptTimeout),
					IdleTimeout:         time.Duration(cfg.Timeouts.IdleTimeout),
					DebugRedact:         cfg.LogDebugRedact,
					WorkspaceRoot:       os.Getenv("WORKSPACE_ROOT"),
				},
				ThinkingDefault: cfg.OMP.ThinkingLevel,
				MaxAutoRetries:  cfg.OMP.MaxAutoRetries,
				ModelOptions:    cfg.OMP.ModelOptions,
				ApprovalOptions: cfg.OMP.ApprovalOptions,
				ThinkingOptions: cfg.OMP.ThinkingOptions,
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
