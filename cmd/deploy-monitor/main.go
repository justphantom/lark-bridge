// Command lark-deploy-monitor runs the deploy-monitor backend.
//
// It registers as a backend (backendType "deploy-monitor") over SSE and, when
// a bound chat sends "/deploy", runs `make <target>` in the project root; "/pull"
// and "/push" run git pull --ff-only / git push in the same root. The job runs
// asynchronously (single-flight, shared across /deploy, /pull, /push) and the result is reported back
// as a Notice Control. Configuration is read from -config.
//
// deploy.sh is expected to NOT stop/restart this service mid-deploy (it only
// updates the binary and restarts the monitor last), so the terminal notice
// can be emitted. See deploy/deploy.sh for the service-group split.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/deploymonitor"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", "./deploy-monitor-config.json", "path to JSON config file")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-deploy-monitor %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-deploy-monitor: %v\n", err)
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

	rpc, err := backendrpc.Connect(cfg.BackendID, "deploy-monitor", cfg.FrontendURL, cfg.IPCSecret)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(logger)
	defer rpc.Close()

	h := deploymonitor.New(
		deploymonitor.Config{
			ProjectRoot:  cfg.DeployMonitor.ProjectRoot,
			DeployTarget: cfg.DeployMonitor.DeployTarget,
		},
		rpc,
		rpc,
		execCommander{},
		logger,
		0,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("deploy-monitor ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"project_root", cfg.DeployMonitor.ProjectRoot,
		"deploy_target", cfg.DeployMonitor.DeployTarget)

	eventErr := func(err error) {
		logger.Warn("ipc", log.FieldError, err)
	}
	runErr := backendrpc.Run(ctx, cfg.BackendID, "deploy-monitor", cfg.FrontendURL, cfg.IPCSecret,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				logger.Error("handle event", "event_type", ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)

	// Graceful drain: a SIGTERM makes backendrpc.Run return, but a make deploy
	// /pull /push may still be mid-flight in its own goroutine. Wait up to the
	// job timeout (+buffer) for it to finish so the process exit does not
	// SIGKILL a half-built docker image or half-pushed git state. The job's
	// own ctx timeout is the hard bound; the +30s absorbs the final notice
	// POST back to the frontend.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Minute+30*time.Second)
	defer drainCancel()
	if err := h.Close(drainCtx); err != nil {
		logger.Warn("deploy job drain timed out, process exiting", log.FieldError, err)
	}
	return runErr
}

// execCommander is the production commander: runs `name args...` inside dir,
// capturing combined stdout+stderr. Tree-wide SIGKILL on ctx cancel so
// `make deploy`'s recursive subprocesses (make, docker, npm …) cannot
// pin the single-flight slot after the parent is killed; output is capped
// at cmdutil.MaxCombinedOutput so a pathological deploy log (100MB+)
// cannot exhaust memory.
type execCommander struct{}

func (execCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return cmdutil.RunCombinedBounded(ctx, dir, name, args...)
}

func buildLogger(cfg *config.Config) (*log.Logger, error) {
	return log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "deploy-monitor")
}
