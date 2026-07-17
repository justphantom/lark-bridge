// Command miniagent-cli runs a single miniagent turn from stdin and emits
// stream-json events to stdout. It is the standalone, feishu-free entry point
// for the miniagent: usable in pipes, scripts, CI, and as the subprocess that
// minibridge (cmd/miniagent-back) will fork in P3.
//
// Usage:
//
//	echo "你好" | miniagent-cli --model kimi-for-coding --api-key sk-xxx --base-url http://...
//	miniagent-cli --model kimi --workdir /proj --verbose < prompt.txt
//	miniagent-cli --model kimi --state-dir /tmp/ma --chat-id test --workdir /proj
//
// Each event is one JSON object per line (NDJSON). The terminal event is
// either {"type":"result",...} (exit 0) or {"type":"error",...} (exit 1).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/miniagent"
)

var version = "dev"

func main() {
	var (
		model      = flag.String("model", "", "LLM model id (required)")
		apiKey     = flag.String("api-key", os.Getenv("MINIAGENT_API_KEY"), "LLM API key (or $MINIAGENT_API_KEY)")
		baseURL    = flag.String("base-url", os.Getenv("MINIAGENT_BASE_URL"), "LLM endpoint root, no /v1 suffix (or $MINIAGENT_BASE_URL)")
		system     = flag.String("system", "你是一个简洁的助手，回答通常不超过 500 字。", "system prompt")
		maxTokens  = flag.Int("max-tokens", 4096, "max output tokens")
		workdir    = flag.String("workdir", "", "working directory (tool bounds + shell cwd)")
		stateDir   = flag.String("state-dir", "", "state directory for session/memory (empty = stateless)")
		chatID     = flag.String("chat-id", "", "chat id for per-chat session isolation (empty = no history)")
		permission = flag.String("permission", "default", "permission mode: plan (read-only), default (bounded), free (unrestricted)")
		verbose    = flag.Bool("verbose", false, "emit tool_use and tool_result events (default: tool_use only)")
		showVer    = flag.Bool("version", false, "show version")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("miniagent-cli %s\n", version)
		os.Exit(0)
	}
	if *model == "" {
		fmt.Fprintln(os.Stderr, "miniagent-cli: --model is required")
		os.Exit(1)
	}
	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent-cli: --api-key is required (or set $MINIAGENT_API_KEY)")
		os.Exit(1)
	}

	// Read prompt from stdin.
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent-cli: read stdin: %v\n", err)
		os.Exit(1)
	}
	if len(prompt) == 0 {
		fmt.Fprintln(os.Stderr, "miniagent-cli: stdin is empty (send prompt via pipe or redirect)")
		os.Exit(1)
	}

	// Logger goes to stderr so stdout stays pure NDJSON.
	var lvl log.LevelVar
	lvl.Set(log.LevelDebug)
	logger := log.NewJSON(&lvl, os.Stderr, "miniagent-cli")

	// LLM client.
	llm := &miniagent.HTTPClient{
		APIKey:  *apiKey,
		BaseURL: *baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Logger:  logger,
	}

	// Tools: permission mode controls tool set and restrictions.
	// plan = read-only (no write/shell); default = bounded; free = unrestricted.
	unrestricted := *permission == "free"
	var tools []miniagent.Tool
	switch *permission {
	case "plan":
		// Read-only: only read_file + webfetch. No write/shell registered.
		if *workdir != "" {
			tools = append(tools, miniagent.ReadFile{WorkspaceRoot: *workdir})
		}
		tools = append(tools, miniagent.WebFetch{})
	default:
		// default or free: full tool set.
		if *workdir != "" || unrestricted {
			tools = append(tools,
				miniagent.ReadFile{WorkspaceRoot: *workdir, Unrestricted: unrestricted},
				miniagent.WriteFile{WorkspaceRoot: *workdir, Unrestricted: unrestricted},
				miniagent.Shell{WorkspaceRoot: *workdir, Unrestricted: unrestricted},
			)
		}
		tools = append(tools, miniagent.WebFetch{})
	}

	// History (optional).
	var history *miniagent.History
	if *stateDir != "" && *chatID != "" {
		history = miniagent.NewHistory(*stateDir, logger)
	}

	// Load prior conversation (nil-safe when history is nil).
	hist := history.Load(*chatID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	emit := miniagent.StreamEmitFunc(os.Stdout, *verbose)

	result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
		Model:     *model,
		System:    *system,
		MaxTokens: *maxTokens,
		Tools:     tools,
	}, "cli", string(prompt), hist, emit, logger)

	if err != nil {
		miniagent.EmitError(os.Stdout, err.Error())
		os.Exit(1)
	}

	// Persist new messages (no-op when history is nil).
	history.Append(*chatID, result.History)

	miniagent.EmitResult(os.Stdout, result, *model)
}
