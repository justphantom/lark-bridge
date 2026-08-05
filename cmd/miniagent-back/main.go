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
	cfg, cfgWarns, err := config.LoadWithWarnings(cfgPath)
	if err != nil {
		return err
	}
	logger, err := buildLogger(cfg)
	if err != nil {
		return err
	}
	for _, w := range cfgWarns {
		logger.Warn("config warning", "warning", w)
	}

	if err := backendrpc.ValidateBackendConfig(cfg.IPCSecret, cfg.BackendID, cfg.FrontendURL); err != nil {
		return err
	}
	// The API key is required, supplied one of two ways: inline via api_key,
	// or via key_file (the bridge reads the file and injects the value as
	// $MINIAGENT_API_KEY on the subprocess — miniagent removed -key-file
	// post-3.4.0). miniclient.New takes both; only one needs to be set.
	if cfg.MiniAgent.APIKey == "" && cfg.MiniAgent.KeyFile == "" {
		return fmt.Errorf("miniagent.api_key is required (use ${MINIAGENT_API_KEY}, or set key_file to load it from a file)")
	}
	// fail-fast: an empty model makes the miniagent CLI refuse to start
	// (its main.go requires -model non-empty) and exit code 1 surfaces as
	// a confusing "启动 miniagent 失败" on the first prompt.
	if cfg.MiniAgent.Model == "" {
		return fmt.Errorf("miniagent.model is required (use ${MINIAGENT_DEFAULT_MODEL} in the config)")
	}
	// Binary-specific required fields (validate() only checks cross-binary enums;
	// per its header comment, one-binary requireds live here).
	if cfg.MiniAgent.WorkspaceRoot == "" {
		return fmt.Errorf("miniagent.workspace_root is required (v3 -mode default needs a workdir; /cd picker disabled without it)")
	}
	// Resolve config_path: if empty, scan ConfigDir (default ~/.miniagent) for
	// miniagent.json or *-miniagent.json. The CLI no longer accepts bare mode,
	// so a resolvable config path is effectively required.
	resolvedPath := config.ResolveConfigPath(cfg)
	if resolvedPath == "" {
		return fmt.Errorf("miniagent.config_path is required (set config_dir/config_path, or place miniagent.json/*-miniagent.json under the default ~/.miniagent)")
	}
	// Guard against relative paths in explicit config: resolveConfigPath already
	// anchors relative paths at ConfigDir, but an explicit ConfigPath that was
	// set by an operator who meant an absolute path must stay absolute.
	if !filepath.IsAbs(resolvedPath) {
		return fmt.Errorf("miniagent.config_path resolved to a relative path %q — check config_dir", resolvedPath)
	}
	cfg.MiniAgent.ConfigPath = resolvedPath
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

	// CLI subprocess mode: miniagent-back forks miniagent per turn.
	// The CLI binary (github.com/justphantom/miniagent) lives alongside
	// this binary in the deploy dir. os.Executable() resolves the real binary
	// path even when argv[0] has been rewritten by a wrapper/supervisor
	// (systemd, shell aliases); fall back to argv[0]'s dir only if it fails.
	exeDir := filepath.Dir(os.Args[0])
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exeDir = filepath.Dir(resolved)
		} else {
			exeDir = filepath.Dir(exe)
		}
	}
	cliPath := filepath.Join(exeDir, "miniagent")
	if _, err := os.Stat(cliPath); err != nil { //nolint:gosec // G703: cliPath derives from our own binary path, not user input
		// Fallback: check /usr/local/bin (make deploy from miniagent repo).
		cliPath = "/usr/local/bin/miniagent"
	}
	client := miniclient.New(miniclient.Config{
		CLIPath:       cliPath,
		APIKey:        cfg.MiniAgent.APIKey,
		SystemPrompt:  cfg.MiniAgent.SystemPrompt,
		MaxTokens:     cfg.MiniAgent.MaxTokens,
		Stream:        cfg.MiniAgent.Stream,
		MaxIterations: cfg.MiniAgent.MaxIterations,
		Mode:          cfg.MiniAgent.Mode,
		Thinking:      cfg.MiniAgent.Thinking,
		KeyFile:       cfg.MiniAgent.KeyFile,
		ConfigPath:    cfg.MiniAgent.ConfigPath,
	}, logger)

	// P0: startup health gate. Run BEFORE connecting to the frontend so a
	// missing or too-old CLI fails fast here, instead of registering with the
	// frontend and tearing down on the first turn.
	healthCtx, healthCancel := context.WithCancel(context.Background())
	defer healthCancel()
	if err := client.IsReady(healthCtx); err != nil {
		return fmt.Errorf("miniagent health: %w", err)
	}

	connOpts := backendrpc.ConnectOptions{
		BackendID:   cfg.BackendID,
		BackendType: "miniagent",
		FrontendURL: cfg.FrontendURL,
		Secret:      cfg.IPCSecret,
		Version:     version,
		// 进程级钉扎一个后端令牌：本进程所有 client（含重连派生的）与所有
		// POST 携带同一令牌，否则 SSE 握手注册的令牌会与 POST 令牌互异，
		// 被前端 validateBackendToken 以 403 拒绝（见 docs/STATUS_CARD_BACKEND_METRICS_FIX.md）。
		BackendToken: backendrpc.NewBackendToken(),
		// M10-1: TLS client config for https frontend_url (CA pinning +
		// optional mTLS client certificate).
		TLSCAFile:         cfg.IPCTLSCAFile,
		TLSClientCertFile: cfg.IPCTLSClientCertFile,
		TLSClientKeyFile:  cfg.IPCTLSClientKeyFile,
	}
	rpc, err := backendrpc.Connect(connOpts)
	if err != nil {
		return fmt.Errorf("connect frontend: %w", err)
	}
	rpc.SetLogger(logger)
	defer rpc.Close()

	// configDir bounds the /config picker: the directory scanned for
	// miniagent.json / *-miniagent.json. Same resolution the startup config-path
	// scan used (ResolveConfigPath), so the picker offers exactly what startup
	// picked from.
	configDir := config.ResolveConfigDir(cfg)
	h := miniagent.New(rpc, logger, r, cfg.MiniAgent.WorkspaceRoot, cfg.MiniAgent.Model, cfg.MiniAgent.Provider, client, configDir, cfg.MiniAgent.StreamHistory, cfg.StateDir, cfg.RedactStreams())

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
	// RunWithClient（而非 Run）：复用上方已 Connect 出的 rpc 作为 SSE +
	// control/metrics POST 的唯一 client，避免再建一个令牌互异的 client
	// 导致 403（见 docs/STATUS_CARD_BACKEND_METRICS_FIX.md）。
	return backendrpc.RunWithClient(ctx, rpc, connOpts,
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
