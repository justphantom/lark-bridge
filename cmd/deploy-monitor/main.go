// Command lark-deploy-monitor runs the deploy-monitor backend.
//
// It registers as a backend (backendType "deploy-monitor") over SSE and, when
// a bound chat sends "/deploy", runs `make <target>` in the project root. The
// deploy runs asynchronously (single-flight) and the result is reported back
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
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/config"
	"github.com/hu/lark-bridge/internal/deploymonitor"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
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

	if cfg.IPCSecret == "" {
		return fmt.Errorf("ipc_secret is required (frontend IPC rejects connections without it)")
	}
	if cfg.BackendID == "" {
		return fmt.Errorf("backend_id is required")
	}
	if cfg.FrontendURL == "" {
		return fmt.Errorf("frontend_url is required")
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
	return backendrpc.Run(ctx, cfg.BackendID, "deploy-monitor", cfg.FrontendURL, cfg.IPCSecret,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				logger.Error("handle event", "event_type", ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
}

// execCommander is the production commander: runs `make <target>` in dir,
// capturing combined stdout+stderr. It is the only implementation outside tests.
type execCommander struct{}

func (execCommander) Run(ctx context.Context, dir, target string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "make", "-C", dir, target)
	return cmd.CombinedOutput()
}

func buildLogger(cfg *config.Config) (*log.Logger, error) {
	return log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "deploy-monitor")
}
