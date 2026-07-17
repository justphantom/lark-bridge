package miniagent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/streamarchive"
)

// maxHistoryTokens bounds how much prior conversation the LLM sees. A rough
// char/4 estimate (English-leaning; 中文 is undercounted but safe-side at the
// cost of slightly more truncation). 6000 leaves headroom for the system
// prompt, the current user turn, and the model's reply under typical 8K+
// context windows.
const maxHistoryTokens = 6000

// maxToolResultInHistory clamps a tool result before it enters the stored
// history (and the messages fed back to the LLM). The full result is still
// emitted to the frontend; this only caps what lingers across turns so one
// big read_file cannot crowd out everything else.
const maxToolResultInHistory = 500

// History persists per-chat conversation as jsonl under
// {stateDir}/miniagent/history/{sanitizedChatID}.jsonl. Each line is one
// Message. Load reads the whole file; Append adds lines. The loop feeds
// Load's result back into the LLM so the agent remembers prior turns.
//
// A nil *History (MemoryEnabled=false) is valid: Load returns nil and Append
// is a no-op, so the handler/runTurn code does not need a separate "memory
// off" branch.
type History struct {
	dir    string
	logger *log.Logger
}

// NewHistory builds a History rooted at {stateDir}/miniagent/history. The
// directory is created lazily on the first Append. logger may be nil.
func NewHistory(stateDir string, logger *log.Logger) *History {
	if logger == nil {
		logger = log.Nop()
	}
	return &History{dir: filepath.Join(stateDir, "miniagent", "history"), logger: logger}
}

// Load returns the stored conversation for chatID (trimmed to the token
// budget), or nil if none. The file itself is append-only (old turns are
// kept on disk for the audit trail); trim drops whole old turns from what
// the LLM sees so context stays bounded. A corrupt line is skipped with a
// debug log rather than failing the whole turn.
func (h *History) Load(chatID string) []Message {
	if h == nil {
		return nil
	}
	f, err := os.Open(h.pathFor(chatID))
	if err != nil {
		return nil // missing file is the common case on first turn
	}
	defer f.Close()
	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // allow large tool_result lines
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			h.logger.Debug("miniagent history: skip malformed line", log.FieldError, err)
			continue
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		// A read failure (e.g. a line overrunning the scanner buffer) is not
		// normal EOF; surface it so the operator notices silent history loss.
		h.logger.Warn("miniagent history: read error", log.FieldError, err)
	}
	return h.trim(msgs)
}

// Append writes msgs as additional jsonl lines for chatID. Best-effort: a
// write error is logged but not returned, since failing the turn over a
// history save would lose an otherwise-good reply.
func (h *History) Append(chatID string, msgs []Message) {
	if h == nil || len(msgs) == 0 {
		return
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		h.logger.Warn("miniagent history: mkdir failed", log.FieldError, err)
		return
	}
	f, err := os.OpenFile(h.pathFor(chatID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		h.logger.Warn("miniagent history: open failed", log.FieldError, err)
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		_, _ = w.Write(b)
		_ = w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		h.logger.Warn("miniagent history: flush failed", log.FieldError, err)
	}
}

// trim drops whole conversation turns from the head of msgs until the
// estimated token count is at or under maxHistoryTokens. A "turn" is a user
// message plus everything after it up to (but not including) the next user
// message — this keeps assistant tool_calls grouped with their tool-role
// replies so OpenAI's pairing requirement is never broken. msgs already
// under the cap is returned unchanged.
func (h *History) trim(msgs []Message) []Message {
	if h == nil || len(msgs) == 0 {
		return msgs
	}
	for estimateTokens(msgs) > maxHistoryTokens && hasMultipleTurns(msgs) {
		msgs = dropFirstTurn(msgs)
	}
	return msgs
}

// pathFor returns the jsonl path for chatID. SanitizeName reuses the
// streamarchive cleaner so a chatID with unexpected characters cannot
// escape the history directory.
func (h *History) pathFor(chatID string) string {
	return filepath.Join(h.dir, streamarchive.SanitizeName(chatID)+".jsonl")
}

// estimateTokens is a rough char/4 budget for the message list. It counts
// content + tool_call args + tool_call_id; good enough to gate trimming
// without pulling in a real tokenizer.
func estimateTokens(msgs []Message) int {
	total := 0
	for i := range msgs {
		total += len(msgs[i].Content) / 4
		for _, tc := range msgs[i].ToolCalls {
			total += len(tc.Args) / 4
		}
		total += len(msgs[i].ToolCallID) / 4
	}
	return total
}

// hasMultipleTurns reports whether msgs contains more than one user message.
// Used to stop trim from deleting the only remaining turn (leaving the LLM
// with no context for the current user message would be worse than exceeding
// the soft cap).
func hasMultipleTurns(msgs []Message) bool {
	users := 0
	for _, m := range msgs {
		if m.Role == "user" {
			users++
		}
	}
	return users > 1
}

// dropFirstTurn removes the leading user message and every message up to
// (but not including) the next user message. This drops the user turn, the
// assistant reply, any tool_calls, and their tool-role results as one unit.
func dropFirstTurn(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}
	// Skip the first user message and any non-user messages that follow.
	i := 1
	for i < len(msgs) && msgs[i].Role != "user" {
		i++
	}
	return msgs[i:]
}

// truncateToolResult clamps a tool result for storage/replay. The full
// output still reaches the frontend via emit; only the value fed back to
// the LLM and persisted to history is trimmed.
func truncateToolResult(s string) string {
	return truncate(s, maxToolResultInHistory, "…[tool_result 已截断]")
}
