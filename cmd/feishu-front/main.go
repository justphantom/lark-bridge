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
	"runtime/debug"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/fileconvert"
	"github.com/justphantom/lark-bridge/internal/hostmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
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
	// mistaken for dead during idle periods. Increased to 10m to tolerate
	// transient network issues that can occur during long-running tasks like
	// video generation (typically 1-2 minutes).
	wsWatchdogInterval = 30 * time.Second
	wsFatalAfter       = 10 * time.Minute
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
	cfg, cfgWarns, err := config.LoadFeishuFrontWithWarnings(cfgPath)
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
	for _, w := range cfgWarns {
		logger.Warn("config warning", "warning", w)
	}

	// The IPC server is reachable by backends over HTTP; require a shared
	// secret so a backendID cannot be impersonated (see H3).
	if cfg.IPCSecret == "" {
		return fmt.Errorf("ipc_secret is required (frontend IPC has no auth without it)")
	}

	// Feishu bot.
	bot, err := feishu.NewBotWithLogger(cfg.FeishuAppID, cfg.FeishuAppSecret, logger,
		feishu.WithDomain(cfg.FeishuDomain))
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
	// M10-1: TLS (or mTLS) for the IPC listener; mandatory when ipc_addr is
	// non-loopback (config validate + Listen both enforce).
	ipc.SetTLS(cfg.IPCTLSCertFile, cfg.IPCTLSKeyFile, cfg.IPCTLSClientCAFile)

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
	// Progress card "思考中" zone: cap the live reasoning shown. Default 50
	// runes; overridable via renderer.max_thinking_runes.
	dispatcher.SetMaxThinkingRunes(cfg.Renderer.MaxThinkingRunes)

	// File-message pipeline: opt-in via file_convert.enabled. When the
	// operator has not enabled it, file-type messages keep the legacy
	// "不支持的消息类型" rejection, so existing deployments are unaffected.
	if cfg.FileConvert.Enabled {
		if err := wireFilePipeline(cfg, bot, dispatcher, logger); err != nil {
			return err
		}
	}

	// /send file delivery: a backend's TypeFile control asks the frontend to
	// upload+send a file into the chat (send-file-design.md). Wired
	// unconditionally — it is independent of the inbound file_convert upload
	// pipeline — so any bound backend can /send files regardless of whether
	// inbound uploads are enabled. *feishu.Bot implements FileSender.
	dispatcher.SetFileSender(bot)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Card debouncer: coalesce UpdateCard calls to avoid API rate limits.
	dispatcher.InitDebouncer(ctx, cardDebounceInterval)
	// Periodic TTL sweep for the three dedup sets: Add is O(1) on the hot
	// path, so this ticker is what keeps the TTL-only sets bounded.
	dispatcher.StartDedupPrune(ctx)
	// Periodic TTL sweep for the routing table: a chat that neither sends a
	// message nor switches backend for 14d is treated as abandoned and pruned
	// (persisted), so long-running deployments do not accumulate dead groups.
	router.StartPrune(ctx, 14*24*time.Hour)
	// Periodic inbox sweep: PruneInbox otherwise runs only once at startup, so
	// a multi-month deployment would accumulate one inbox dir per chatID/msgID.
	// Immediate-then-daily; only when the file pipeline is wired.
	if cfg.FileConvert.Enabled {
		dispatcher.StartInboxPrune(ctx, time.Duration(cfg.FileConvert.Retention))
	}

	ipc.SetOnOffline(dispatcher.OnBackendOffline)
	ipc.SetOnOnline(dispatcher.OnBackendOnline)
	// /v1/status reports in-flight turn count so deploy.sh can avoid cutting
	// off a live conversation when restarting the frontend.
	ipc.SetInFlightTurns(turns.InFlight)
	ipc.SetInFlightDetail(turns.InFlightTurns)

	// feishu-front self-reports into /v1/status: it does not POST
	// /v1/metrics to itself (a self-loop HTTP hop buys nothing), the status
	// handler reads the local host directly instead. The IP is probed once —
	// display-only, so DHCP churn between restarts is harmless.
	selfIP := hostmetrics.PrimaryIPv4()
	ipc.SetSelfMetrics(func() (protocol.HostStats, protocol.ServiceStat) {
		host, _ := hostmetrics.CollectHost(cfg.StateDir, time.Now())
		host.IP = selfIP
		cg, ok, _ := hostmetrics.SelfCgroupMem("lark-feishu-front.service")
		if !ok {
			cg = 0
		}
		return host, protocol.ServiceStat{
			BackendID:      "feishu-front",
			IP:             selfIP,
			Version:        version,
			CgroupMemBytes: cg,
			ReportedAt:     host.ReportedAt,
		}
	})

	bot.OnIncoming(dispatcher.DispatchIncoming)
	bot.OnCardAction(dispatcher.DispatchCardAction)

	// Health checker evicts silent backends. Recover so a panic in the tick
	// (EachConn / onOffline) cannot kill the loop and let dead backends rot.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in health check",
					log.FieldPanic, r, log.FieldStack, string(debug.Stack()))
			}
		}()
		ipc.StartHealthCheck(ctx, healthInterval, time.Duration(cfg.Timeouts.BackendHealth))
	}()

	// Control pump: drain registry.Controls() and dispatch each.
	// Recover per-message so a panic in DispatchControl (nil deref, slice
	// bounds, map concurrent write in updateProgress/sendResult/notice/…)
	// cannot kill the pump goroutine — that would fill ctrlCh and 503 every
	// later /v1/control POST, freezing every backend's card at its last frame.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case rc := <-registry.Controls():
				func() {
					defer func() {
						if r := recover(); r != nil {
							logger.Error("panic in control dispatch",
								"control_type", rc.Control.Type,
								log.FieldPanic, r,
								log.FieldStack, string(debug.Stack()))
						}
					}()
					if err := dispatcher.DispatchControl(ctx, rc); err != nil {
						logger.Error("dispatch control", "control_type", rc.Control.Type, log.FieldError, err)
					}
				}()
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
	// The lark client reconnects itself on transient failure, so this is the
	// catastrophic-case fallback (reconnect loop wedged or process lost the
	// ability to send/receive entirely) — exit lets systemd pull up a fresh
	// copy. The SDK-era soft-restart machinery is gone: the new client owns
	// its own reconnection and does not leak goroutines.
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
					logger.Error("bot unhealthy past fatal window, exit for supervisor recovery",
						"last_healthy", bot.LastHealthy(),
						"fatal_after", wsFatalAfter)
					os.Exit(1)
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
func buildLogger(cfg *config.FeishuFrontConfig) (*log.Logger, error) {
	return log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "feishu-front")
}

// wireFilePipeline configures the dispatcher with the file-message pipeline:
//   - resolves the inbox directory (defaulting to {state_dir}/inbox) and
//     pre-creates it with 0700 perms;
//   - constructs a fileconvert.Converter carrying the operator's
//     convert_timeout (docx/pptx conversion is pure Go; no external binary
//     preflight is needed);
//   - hands both to the dispatcher alongside the bot's DownloadFile method;
//   - runs a one-shot retention sweep so a long-lived deployment does not
//     accumulate stale uploads.
//
// Returns an error only on unrecoverable setup (inbox dir not creatable).
func wireFilePipeline(cfg *config.FeishuFrontConfig, bot *feishu.Bot, dispatcher *feishufront.Dispatcher, logger *log.Logger) error {
	inbox := cfg.FileConvert.InboxDir
	if inbox == "" {
		inbox = filepath.Join(cfg.StateDir, "inbox")
	}
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return fmt.Errorf("file_convert: create inbox %s: %w", inbox, err)
	}

	converter := fileconvert.New(fileconvert.Options{
		Timeout:         time.Duration(cfg.FileConvert.ConvertTimeout),
		Logger:          logger,
		PptxMaxSlides:   cfg.FileConvert.PptxMaxSlides,
		XlsxMaxSheets:   cfg.FileConvert.XlsxMaxSheets,
		XlsxFormulaMode: cfg.FileConvert.XlsxFormulaMode,
	})

	// Parse the operator-supplied prompt template once at startup. config.
	// validate already syntax-checked it, so a parse failure here is a
	// process-level invariant violation (e.g. the FuncMap was changed
	// without re-validating) rather than an operator typo; surface it as
	// a fatal so the process never starts with a broken template wired in.
	tmpl, err := template.New("file_convert.prompt_template").Funcs(config.PromptTemplateFuncs()).Parse(cfg.FileConvert.PromptTemplate)
	if err != nil {
		return fmt.Errorf("file_convert: parse prompt_template: %w", err)
	}

	// PostPromptTemplate is optional: empty → post messages degrade to
	// text-only Markdown (no image download). When set, parse it the same
	// way so a typo fails fast at startup.
	var postTmpl *template.Template
	if strings.TrimSpace(cfg.FileConvert.PostPromptTemplate) != "" {
		postTmpl, err = template.New("file_convert.post_prompt_template").Funcs(config.PromptTemplateFuncs()).Parse(cfg.FileConvert.PostPromptTemplate)
		if err != nil {
			return fmt.Errorf("file_convert: parse post_prompt_template: %w", err)
		}
	}

	// XlsxPromptTemplate is optional: empty → xlsx uploads fall back to the
	// generic template (path only, no per-sheet schema). When set, parse it
	// so the C-paradigm prompt (path + column names + row counts) is wired.
	var xlsxTmpl *template.Template
	if strings.TrimSpace(cfg.FileConvert.XlsxPromptTemplate) != "" {
		xlsxTmpl, err = template.New("file_convert.xlsx_prompt_template").Funcs(config.PromptTemplateFuncs()).Parse(cfg.FileConvert.XlsxPromptTemplate)
		if err != nil {
			return fmt.Errorf("file_convert: parse xlsx_prompt_template: %w", err)
		}
	}

	dispatcher.SetFilePipeline(bot, converter, inbox, cfg.FileConvert.MaxFileSize, tmpl, postTmpl)
	dispatcher.SetXlsxPromptTemplate(xlsxTmpl)
	logger.Info("file pipeline enabled",
		"inbox", inbox,
		"max_file_size", cfg.FileConvert.MaxFileSize,
		"convert_timeout", time.Duration(cfg.FileConvert.ConvertTimeout),
		"retention", time.Duration(cfg.FileConvert.Retention),
		"post_pipeline", postTmpl != nil,
		"xlsx_pipeline", xlsxTmpl != nil)
	return nil
}
