// Command lark-agnes-back runs the Agnes AI media-generation backend.
//
// It registers as a backend (backendType "agnes") over SSE and exposes four
// slash commands that wrap Agnes AI's image/video generation APIs:
//
//   - /image-prompt <描述>: expand a terse description into a full image prompt
//     via the chat model (agnes-2.5-flash).
//   - /image <提示词>: generate an image via agnes-image-2.1-flash; the image
//     lands inline in the chat as a TypeFile control.
//   - /video-prompt <描述>: expand a description into a full video prompt.
//   - /video <提示词>: generate a video via agnes-video-v2.0 (async task +
//     polling); the final video URL is delivered as a Notice.
//
// Unlike the CLI backends (claude/miniagent), agnes-back does NOT fork a
// subprocess: it calls the Agnes HTTP API directly via net/http (the project is
// zero-external-dependency). Configuration is read from -config. The agnes.api_key
// field should use ${AGNES_API_KEY} so the key is pulled from the environment,
// not committed in the config file.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/agnesback"
	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "./agnes-back-config.json", "path to JSON config file")
	showVer := flag.Bool("version", false, "show version information")
	flag.Parse()

	if *showVer {
		fmt.Printf("lark-agnes-back %s\n", version)
		os.Exit(0)
	}
	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "lark-agnes-back: %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, cfgWarns, err := config.LoadWithWarnings(cfgPath)
	if err != nil {
		return err
	}
	logger, err := log.NewFromConfig(cfg.LogLevel, cfg.LogOutput, cfg.LogFormat, "agnes")
	if err != nil {
		return err
	}
	for _, w := range cfgWarns {
		logger.Warn("config warning", "warning", w)
	}

	if err := backendrpc.ValidateBackendConfig(cfg.IPCSecret, cfg.BackendID, cfg.FrontendURL); err != nil {
		return err
	}
	// The API key is required. It is supplied one of two ways: inline via
	// api_key (recommended ${AGNES_API_KEY}) — no key_file variant like
	// miniagent, since agnes-back is HTTP-only and has no subprocess to inject
	// an env var into.
	if cfg.AgnesBack.APIKey == "" {
		return fmt.Errorf("agnes.api_key is required (use ${AGNES_API_KEY})")
	}

	connOpts := backendrpc.ConnectOptions{
		BackendID:   cfg.BackendID,
		BackendType: "agnes",
		FrontendURL: cfg.FrontendURL,
		Secret:      cfg.IPCSecret,
		Version:     version,
		// 进程级钉扎一个后端令牌：本进程所有 client（含重连派生的）与所有
		// POST 携带同一令牌，否则 SSE 握手注册的令牌会与 POST 令牌互异，
		// 被前端 validateBackendToken 以 403 拒绝（见 policies#4）。
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

	// Build the config (defaults already applied by config.Load) + the real
	// API client, then wire the handler. FromConfig resolves the AgnesBack
	// config block into the ClientConfig the client/handler share.
	cc, client := agnesback.FromConfig(cfg.AgnesBack)
	h := agnesback.NewHandler(cc, client, rpc, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("agnes-back ready",
		"backend_id", cfg.BackendID,
		"frontend_url", cfg.FrontendURL,
		"base_url", cfg.AgnesBack.BaseURL,
		"chat_model", cfg.AgnesBack.ChatModel,
		"image_model", cfg.AgnesBack.ImageModel,
		"video_model", cfg.AgnesBack.VideoModel)

	// Host/process metrics push for the status-monitor overview card.
	go backendrpc.StartMetricsLoop(ctx, rpc, backendrpc.MetricsOptions{
		Interval: time.Duration(cfg.StatusMonitor.Interval),
		StateDir: cfg.StateDir,
		UnitName: "lark-agnes-back.service",
		Version:  version,
		Logger:   logger,
	})

	eventErr := func(err error) { logger.Warn("ipc", log.FieldError, err) }
	// RunWithClient（而非 Run）：复用上方已 Connect 出的 rpc 作为 SSE +
	// control/metrics POST 的唯一 client，避免再建一个令牌互异的 client
	// 导致 403（见 policies#4）。
	runErr := backendrpc.RunWithClient(ctx, rpc, connOpts,
		func(ctx context.Context, ev *protocol.Event) error {
			if err := h.HandleEvent(ctx, ev); err != nil {
				logger.Error("handle event", "event_type", ev.Type, log.FieldError, err)
			}
			return nil
		}, eventErr)
	return runErr
}
