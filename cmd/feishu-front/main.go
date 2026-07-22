// Command feishu-front runs the frontend of the lark-bridge split.
//
// It owns the Feishu WebSocket bot, the IPC server (SSE + Control POST), the
// Layer-1 chat→backend router, and the dispatcher that turns inbound Feishu
// messages into Prompt Events and backend Controls into cards. Configuration
// is read from -config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/log"
)

var version = "dev"

const (
	dirPerm            = 0o700
	healthInterval     = 30 * time.Second
	shutdownIPCTimeout = 5 * time.Second
	shutdownBotTimeout = 5 * time.Second
	// cardDebounceInterval coalesces rapid UpdateCard calls (progress
	// streaming) so the Feishu API is not hammered past its rate limit.
	// 500ms balances liveness (a tool row flips to done within half a
	// second) against Feishu's per-message update QPS. A long multi-round
	// task (实测 claude: 209s, 102 tool calls, 82 distinct rows) still fits
	// comfortably; if substantially longer tasks surface rate-limit errors,
	// raise this rather than adding adaptive logic.
	cardDebounceInterval = 500 * time.Millisecond
	// wsWatchdogInterval / wsFatalAfter bound the bot-health watchdog. The
	// Lark SDK's Start blocks on select{} and never returns, so a permanently
	// dead link leaves the process up but silently dropping every message.
	// The watchdog fatals (→ systemd Restart=on-failure) when no OnReady /
	// OnReconnected signal has arrived within wsFatalAfter, but only after the
	// bot has been healthy at least once (an initial-connect failure is already
	// surfaced by Start itself).
	//
	// wsFatalAfter must exceed the SDK's default ping interval (2 min) so a
	// stable connection that only refreshes health via inbound traffic is not
	// mistaken for dead during idle periods.
	wsWatchdogInterval = 30 * time.Second
	wsFatalAfter       = 5 * time.Minute
)

func main() {
	var (
		cfgPath = flag.String("config", "./feishu-config.json", "path to JSON config file")
		addr    = flag.String("addr", "", "IPC listen address (overrides ipc_addr in config)")
		showVer = flag.Bool("version", false, "show version information")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-feishu-front %s\n", version)
		os.Exit(0)
	}

	if err := run(*cfgPath, *addr); err != nil {
		fmt.Fprintf(os.Stderr, "lark-feishu-front: %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath, addr string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// -addr flag overrides ipc_addr in config; both default to localhost:6060.
	listenAddr := cfg.IPCAddr
	if addr != "" {
		listenAddr = addr
	}

	logger, err := buildLogger(cfg)
	if err != nil {
		return err
	}

	// The IPC server is reachable by backends over HTTP; require a shared
	// secret so a backendID cannot be impersonated (see H3).
	if cfg.IPCSecret == "" {
		return fmt.Errorf("ipc_secret is required (frontend IPC has no auth without it)")
	}

	// Feishu bot.
	bot, err := feishu.NewBotWithLogger(cfg.FeishuAppID, cfg.FeishuAppSecret, logger,
		feishu.WithDomain(cfg.FeishuDomain), feishu.WithLogLevel(cfg.FeishuLogLevel))
	if err != nil {
		return fmt.Errorf("feishu bot: %w", err)
	}
	bot.SetDebugRedact(cfg.LogDebugRedact)

	// Layer-1 router: persists routing.json under state_dir.
	routingPath := filepath.Join(cfg.StateDir, "routing.json")
	if err := os.MkdirAll(cfg.StateDir, dirPerm); err != nil {
		logger.Warn("state_dir unavailable, routing will not persist",
			log.FieldPath, cfg.StateDir, log.FieldError, err)
		routingPath = ""
	}
	router, err := feishufront.NewLayer1Router(routingPath)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}

	// IPC server + registry.
	registry := feishufront.NewBackendRegistry()
	ipc := feishufront.NewIPCServer(registry, cfg.IPCSecret)
	ipc.SetLogger(logger)

	// Dispatcher wires the bot, registry, turn manager and router.
	turns := feishufront.NewTurnManager()
	dispatcher := feishufront.NewDispatcher(bot, registry, turns, router)
	dispatcher.SetLogger(logger)
	// Replay guard: zero-value config fields keep the dispatcher's built-in
	// defaults (300s stale window, 5m event TTL, 1000 entry cap).
	dispatcher.SetDedupConfig(
		time.Duration(cfg.Dedup.StaleWindow),
		time.Duration(cfg.Dedup.EventTTL),
		cfg.Dedup.EventMaxEntries,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Card debouncer: coalesce UpdateCard calls to avoid API rate limits.
	dispatcher.InitDebouncer(ctx, cardDebounceInterval)

	ipc.SetOnOffline(dispatcher.OnBackendOffline)
	ipc.SetOnOnline(dispatcher.OnBackendOnline)
	// /v1/status reports in-flight turn count so deploy.sh can avoid cutting
	// off a live conversation when restarting the frontend. Wire the type
	// resolver so InFlight can exclude deploy-monitor's own /deploy turn
	// (which otherwise self-blocks: make deploy → /v1/status → inflight>0).
	turns.SetTypeResolver(registry.BackendType)
	ipc.SetInFlightTurns(turns.InFlight)
	ipc.SetInFlightDetail(turns.InFlightTurns)

	bot.OnIncoming(dispatcher.DispatchIncoming)
	bot.OnCardAction(dispatcher.DispatchCardAction)

	// Health checker evicts silent backends.
	go ipc.StartHealthCheck(ctx, healthInterval, time.Duration(cfg.Timeouts.BackendHealth))

	// Control pump: drain registry.Controls() and dispatch each.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case rc := <-registry.Controls():
				if err := dispatcher.DispatchControl(ctx, rc); err != nil {
					logger.Error("dispatch control", "control_type", rc.Control.Type, log.FieldError, err)
				}
			}
		}
	}()

	// IPC server on its own goroutine; main blocks on the bot.
	ipcErrCh := make(chan error, 1)
	go func() {
		ipcErrCh <- ipc.Listen(listenAddr)
	}()

	logger.Info("feishu-front ready",
		"addr", listenAddr,
		"routing_path", routingPath)

	botErrCh := make(chan error, 1)
	go func() {
		botErrCh <- bot.Start(ctx)
	}()

	// WS-health watchdog: see wsFatalAfter. Runs alongside the main select;
	// on a fatal diagnosis it logs and exits so the supervisor restarts us.
	startedAt := time.Now()
	go func() {
		ticker := time.NewTicker(wsWatchdogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if feishu.ShouldExitUnhealthy(time.Now(), bot.LastHealthy(), startedAt, wsFatalAfter) {
					// 软重启优先:Stop 旧 WS + 新建一条,业务状态/IPC/turn 全保留。
					// Restart 失败仍 os.Exit(1) 兜底,等价现行 fail-safe;新连接若再失败,
					// 下一轮 watchdog tick 会再次触发 Restart 或最终兜底退出。
					logger.Warn("bot unhealthy, soft-restarting",
						"last_healthy", bot.LastHealthy(),
						"fatal_after", wsFatalAfter)
					if err := bot.Restart(ctx); err != nil {
						logger.Error("soft restart failed, fall back to exit", log.FieldError, err)
						os.Exit(1)
					}
					startedAt = time.Now() // 重置宽限窗口,给新连接同样 5min
				}
			}
		}
	}()

	// Wait for shutdown signal or a fatal component error.
	var firstErr error
	select {
	case <-ctx.Done():
	case err := <-ipcErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			firstErr = err
		}
	case err := <-botErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			firstErr = err
		}
	}

	// Graceful shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownBotTimeout+shutdownIPCTimeout)
	defer cancel()
	if err := bot.Stop(shutdownCtx); err != nil {
		logger.Warn("bot stop", log.FieldError, err)
	}
	ipcShutdownCtx, ipcCancel := context.WithTimeout(context.Background(), shutdownIPCTimeout)
	defer ipcCancel()
	if err := ipc.Shutdown(ipcShutdownCtx); err != nil {
		logger.Warn("ipc shutdown", log.FieldError, err)
	}
	return firstErr
}

// buildLogger builds the component logger from cfg.
func buildLogger(cfg *config.Config) (*log.Logger, error) {
	return log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "feishu-front")
}
