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
// Metadata queries (no stdin needed, exit after output):
//
//	miniagent-cli --api-key sk --list-models
//	miniagent-cli --state-dir /tmp/ma --chat-id test --list-sessions
//	miniagent-cli --state-dir /tmp/ma --chat-id test --show-current
//	miniagent-cli --state-dir /tmp/ma --chat-id test --use-session 20260718-120000
//	miniagent-cli --state-dir /tmp/ma --chat-id test --del-session 20260718-120000
//
// Each event is one JSON object per line (NDJSON). The terminal event is
// either {"type":"result",...} (exit 0) or {"type":"error",...} (exit 1).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniagent"
)

var version = "dev"

func main() {
	var (
		model      = flag.String("model", "", "LLM model id (required for conversation)")
		apiKey     = flag.String("api-key", os.Getenv("MINIAGENT_API_KEY"), "LLM API key (or $MINIAGENT_API_KEY)")
		baseURL    = flag.String("base-url", os.Getenv("MINIAGENT_BASE_URL"), "LLM endpoint root, no /v1 suffix (or $MINIAGENT_BASE_URL)")
		system     = flag.String("system", "你是一个简洁的助手，回答通常不超过 500 字。", "system prompt")
		maxTokens  = flag.Int("max-tokens", 4096, "max output tokens")
		workdir    = flag.String("workdir", "", "working directory (tool bounds + shell cwd)")
		stateDir   = flag.String("state-dir", "", "state directory for session/memory (empty = stateless)")
		chatID     = flag.String("chat-id", "", "chat id for per-chat session isolation (empty = no history)")
		permission = flag.String("permission", "default", "permission mode: plan (read-only), default (bounded), free (unrestricted)")
		verbose    = flag.Bool("verbose", false, "emit tool_use and tool_result events (default: tool_use only)")
		blockedPat = flag.String("blocked-patterns", "", "JSON array of blocked shell patterns (overrides built-in defaults)")
		showVer    = flag.Bool("version", false, "show version")

		// Metadata query flags (exit after output, no stdin needed).
		listModels   = flag.Bool("list-models", false, "list available models from the endpoint, then exit")
		listSessions = flag.Bool("list-sessions", false, "list sessions for --chat-id, then exit")
		showCurrent  = flag.Bool("show-current", false, "show current session/model/dir/permission for --chat-id, then exit")
		useSession   = flag.String("use-session", "", "switch to session <id> for --chat-id, then exit")
		delSession   = flag.String("del-session", "", "delete session <id> for --chat-id, then exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("miniagent-cli %s\n", version)
		os.Exit(0)
	}

	// Logger goes to stderr so stdout stays pure NDJSON/JSON.
	var lvl log.LevelVar
	lvl.Set(log.LevelDebug)
	logger := log.NewJSON(&lvl, os.Stderr, "miniagent-cli")

	// ── Metadata query flags (exit after output) ───────

	if *listModels {
		runListModels(*apiKey, *baseURL)
		return
	}

	if *listSessions {
		runListSessions(*stateDir, *chatID)
		return
	}

	if *showCurrent {
		runShowCurrent(*stateDir, *chatID, *model)
		return
	}

	if *useSession != "" {
		runUseSession(*stateDir, *chatID, *useSession)
		return
	}

	if *delSession != "" {
		runDelSession(*stateDir, *chatID, *delSession)
		return
	}

	// ── Conversation mode ───────────────────────────────

	if *model == "" {
		fmt.Fprintln(os.Stderr, "miniagent-cli: --model is required (or use a metadata flag like --list-models)")
		os.Exit(1)
	}
	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent-cli: --api-key is required (or set $MINIAGENT_API_KEY)")
		os.Exit(1)
	}

	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent-cli: read stdin: %v\n", err)
		os.Exit(1)
	}
	if len(prompt) == 0 {
		fmt.Fprintln(os.Stderr, "miniagent-cli: stdin is empty (send prompt via pipe or redirect)")
		os.Exit(1)
	}

	llm := &miniagent.HTTPClient{
		APIKey:  *apiKey,
		BaseURL: *baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Logger:  logger,
	}

	unrestricted := *permission == "free"
	var blockedPats []string
	if *blockedPat != "" {
		if err := json.Unmarshal([]byte(*blockedPat), &blockedPats); err != nil {
			fmt.Fprintf(os.Stderr, "miniagent-cli: --blocked-patterns parse error: %v\n", err)
			os.Exit(1)
		}
	}
	var tools []miniagent.Tool
	switch *permission {
	case "plan":
		if *workdir != "" {
			tools = append(tools, miniagent.ReadFile{WorkspaceRoot: *workdir})
		} else {
			fmt.Fprintln(os.Stderr, "miniagent-cli: --workdir is empty, read_file not registered (plan mode needs a workspace)")
		}
		tools = append(tools, miniagent.WebFetch{})
	default:
		if *workdir != "" || unrestricted {
			tools = append(tools,
				miniagent.ReadFile{WorkspaceRoot: *workdir, Unrestricted: unrestricted},
				miniagent.WriteFile{WorkspaceRoot: *workdir, Unrestricted: unrestricted},
				miniagent.EditFile{WorkspaceRoot: *workdir, Unrestricted: unrestricted},
				miniagent.Shell{WorkspaceRoot: *workdir, Unrestricted: unrestricted, BlockedPatterns: blockedPats},
			)
		} else {
			fmt.Fprintln(os.Stderr, "miniagent-cli: --workdir is empty AND permission is not free; read_file/write_file/shell/edit_file not registered")
		}
		tools = append(tools, miniagent.WebFetch{})
	}

	var history *miniagent.History
	var facts miniagent.FactStore
	if *stateDir != "" && *chatID != "" {
		history = miniagent.NewHistory(*stateDir, logger)
		facts = miniagent.NewFactStore(*stateDir, logger)
		tools = append(tools, miniagent.NewMemoryTools(facts, *chatID)...)
	}
	hist := history.Load(*chatID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	emit := miniagent.StreamEmitFunc(os.Stdout, *verbose)

	memoryContext := ""
	if facts != nil {
		// Inject chat-scoped facts into the CLI system prompt.
		if chatFacts, _ := facts.List(miniagent.ScopeChat, *chatID, ""); len(chatFacts) > 0 {
			memoryContext = formatFactsForCLI(chatFacts)
		}
	}

	result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
		Model:         *model,
		System:        *system,
		MemoryContext: memoryContext,
		MaxTokens:     *maxTokens,
		Tools:         tools,
	}, "cli", string(prompt), hist, emit, logger)

	if err != nil {
		miniagent.EmitError(os.Stdout, err.Error())
		os.Exit(1)
	}

	history.Append(*chatID, result.History)
	miniagent.EmitResult(os.Stdout, result, *model)
}

// formatFactsForCLI renders facts for appending to the CLI system prompt.
// Duplicated lightly from miniagent.formatFacts to keep the CLI main package
// self-contained.
func formatFactsForCLI(facts []miniagent.Fact) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n以下是与当前对话相关的已知事实（由用户或之前的对话沉淀）：\n")
	for _, f := range facts {
		fmt.Fprintf(&sb, "- %s: %s\n", f.Key, f.Value)
	}
	return sb.String()
}

// ── Metadata query implementations ────────────────────

func runListModels(apiKey, baseURL string) {
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent-cli: --api-key is required for --list-models")
		os.Exit(1)
	}
	c := &miniagent.HTTPClient{APIKey: apiKey, BaseURL: baseURL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := c.ListModels(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent-cli: list models: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(models)
	fmt.Println(string(out))
}

func mustHistory(stateDir, chatID, action string) *miniagent.History {
	if stateDir == "" {
		fmt.Fprintf(os.Stderr, "miniagent-cli: --state-dir is required for --%s\n", action)
		os.Exit(1)
	}
	if chatID == "" {
		fmt.Fprintf(os.Stderr, "miniagent-cli: --chat-id is required for --%s\n", action)
		os.Exit(1)
	}
	return miniagent.NewHistory(stateDir, log.Nop())
}

func runListSessions(stateDir, chatID string) {
	h := mustHistory(stateDir, chatID, "list-sessions")
	sessions, err := h.ListSessions(chatID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent-cli: list sessions: %v\n", err)
		os.Exit(1)
	}
	type sessionOut struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
		Bytes   int64  `json:"bytes"`
		ModTime string `json:"mod_time"`
	}
	out := make([]sessionOut, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, sessionOut{
			ID:      s.ID,
			Current: s.Current,
			Bytes:   s.Bytes,
			ModTime: s.ModTime.Format("2006-01-02 15:04:05"),
		})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func runShowCurrent(stateDir, chatID, defaultModel string) {
	h := mustHistory(stateDir, chatID, "show-current")
	sid := h.Current(chatID)
	model := h.Model(chatID)
	dir := h.Directory(chatID)
	perm := h.Permission(chatID)
	info := struct {
		ChatID     string `json:"chat_id"`
		SessionID  string `json:"session_id"`
		Model      string `json:"model"`
		Directory  string `json:"directory"`
		Permission string `json:"permission"`
	}{
		ChatID:     chatID,
		SessionID:  sid,
		Model:      model,
		Directory:  dir,
		Permission: perm,
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(b))
}

func runUseSession(stateDir, chatID, sid string) {
	h := mustHistory(stateDir, chatID, "use-session")
	if err := h.UseSession(chatID, sid); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent-cli: use session: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("switched to session %s\n", sid)
}

func runDelSession(stateDir, chatID, sid string) {
	h := mustHistory(stateDir, chatID, "del-session")
	if err := h.DeleteSession(chatID, sid); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent-cli: delete session: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("deleted session %s\n", sid)
}
