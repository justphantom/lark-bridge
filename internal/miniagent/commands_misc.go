package miniagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
)

// memoryRecord matches one line in .miniagent/memory.jsonl. The upstream
// format is {type, topic, content} with an optional id; the bridge re-parses
// because upstream's readMemoryRecords is an internal-only helper in a
// separate Go module and cannot be imported.
type memoryRecord struct {
	Type    string `json:"type"`
	Topic   string `json:"topic,omitempty"`
	Content string `json:"content"`
}

// readMemoryRecords reads the project-level memory file and returns parsed
// records. As of miniagent v3.3.0 (1ac831e) the upstream does dual-layer
// discovery: <workdir>/.miniagent/memory.jsonl takes precedence, falling back
// to ~/.miniagent/memory.jsonl when the workdir copy is absent (workdir >
// home > empty, per-file override — NOT a merge). The bridge mirrors that so
// /memory shows the same records miniagent itself injects into the system
// prompt; before, a workdir without a memory file but with a home file would
// wrongly report "暂无记忆" while the agent actually had memory injected.
//
// Returns nil (not an error) when neither file exists — the caller surfaces a
// friendly "no memory" notice. A read/parse error from the chosen file is
// returned (the home fallback is only tried when the workdir file is ABSENT,
// not when it is malformed: a malformed workdir file is a real signal).
func readMemoryRecords(workdir string) ([]memoryRecord, error) {
	if workdir != "" {
		p := filepath.Join(workdir, ".miniagent", "memory.jsonl")
		if _, err := os.Stat(p); err == nil {
			return parseMemoryFile(p)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	// Fallback: ~/.miniagent/memory.jsonl. os.UserHomeDir failure (no HOME)
	// means no home layer — treat as "no memory".
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		p := filepath.Join(home, ".miniagent", "memory.jsonl")
		if _, err := os.Stat(p); err == nil {
			return parseMemoryFile(p)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, nil
}

// parseMemoryFile reads and parses one memory.jsonl file. Shared by the
// workdir-first and home-fallback paths of readMemoryRecords.
func parseMemoryFile(p string) ([]memoryRecord, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []memoryRecord
	// NDJSON: one JSON object per line. Parse each line independently so a
	// single malformed record (truncated write, partial flush) is skipped
	// without corrupting stream position — json.Decoder does not resync to a
	// line boundary after a Decode error, so a streaming decoder would drop
	// or misparse records after a bad line. Allow up to 1 MiB per line;
	// memory content is unbounded.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r memoryRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// cmdHelp lists the available commands. The persistent per-chat state is the
// router binding (model + directory + mode + thinking) plus the per-chat
// session jsonl (/new deletes it so the next prompt starts fresh).
func (h *Handler) cmdHelp(_ context.Context, _ string, _ string) (level, title, body string) {
	var sb strings.Builder
	sb.WriteString("可用命令：\n\n")
	sb.WriteString("/current        显示当前模型/工作目录/权限/思考\n")
	sb.WriteString("/model          切换模型（弹出选择卡）\n")
	sb.WriteString("/model <id>     直接指定模型\n")
	sb.WriteString("/model clear    恢复默认模型\n")
	sb.WriteString("/models         列出可用模型\n")
	sb.WriteString("/cd             切换工作目录（弹出选择卡）\n")
	sb.WriteString("/cd <path>     直接指定目录\n")
	sb.WriteString("/cd clear       恢复默认目录\n")
	sb.WriteString("/config         切换配置文件（弹出选择卡）\n")
	sb.WriteString("/config clear   恢复默认配置文件\n")
	sb.WriteString("/mode           显示当前权限模式\n")
	sb.WriteString("/mode <m>       设置权限模式（default | auto）\n")
	sb.WriteString("/mode clear     恢复默认权限模式\n")
	sb.WriteString("/thinking       显示当前思考级别\n")
	sb.WriteString("/thinking <l>   设置思考级别（off|minimal|low|medium|high|xhigh|max）\n")
	sb.WriteString("/thinking clear 恢复默认思考级别\n")
	sb.WriteString("/maxiter        显示当前迭代上限\n")
	sb.WriteString("/maxiter <N>    设置每轮 LLM 调用上限（≥1）\n")
	sb.WriteString("/maxiter clear  恢复默认迭代上限\n")
	sb.WriteString("/new            清空当前会话历史（下次提问开始新会话）\n")
	sb.WriteString("/pull           在当前工作目录执行 git pull --ff-only\n")
	sb.WriteString("/push           在当前工作目录执行 git push\n")
	sb.WriteString("/session-abort  中止当前任务\n")
	sb.WriteString("/running        显示运行中的会话\n")
	sb.WriteString("/memory         查看项目级记忆（.miniagent/memory.jsonl）\n")
	sb.WriteString("/help           显示本帮助\n")
	sb.WriteString("\n直接发送消息即可与 AI 对话。")
	return "info", "帮助", sb.String()
}

// cmdRunning lists currently active turns for this chat.
func (h *Handler) cmdRunning(_ context.Context, chatID, _ string) (level, title, body string) {
	sessions := h.RunningSessions()
	var filtered []RunningSession
	for _, s := range sessions {
		if s.ChatID == chatID {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		return "info", "运行中会话", "当前没有运行中的会话。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🔄 **运行中会话** (%d)\n\n", len(filtered))
	for _, s := range filtered {
		fmt.Fprintf(&sb, "- 群ID：`%s`（运行 %s）\n", s.ChatID, bridgebase.FormatDuration(s.Duration))
	}
	sb.WriteString("\n💡 如需中止，请发送 `/session-abort`")
	return "info", "运行中会话", sb.String()
}

// cmdMemory shows the project-level memory (.miniagent/memory.jsonl) the agent
// itself reads. As of miniagent v3.3.0 the upstream does dual-layer discovery:
// <workdir>/.miniagent/memory.jsonl overrides ~/.miniagent/memory.jsonl (the
// bridge mirrors this in readMemoryRecords), so this command surfaces exactly
// what the agent injects into its system prompt. The write path goes through
// miniagent's `write` tool (path=memory) which the user triggers via normal
// chat, not via a slash command.
func (h *Handler) cmdMemory(_ context.Context, chatID, _ string) (level, title, body string) {
	workdir := h.activeDir(chatID) // already falls back to workspaceRoot
	if workdir == "" {
		return "warning", "记忆", "请先用 /cd 设置工作目录。"
	}
	records, err := readMemoryRecords(workdir)
	if err != nil {
		return "error", "记忆", "读取失败：" + err.Error()
	}
	if len(records) == 0 {
		return "info", "项目记忆", "暂无记忆（在 miniagent 中用 write tool path=memory 追加）。"
	}
	var sb strings.Builder
	for _, r := range records {
		fmt.Fprintf(&sb, "- [%s]", r.Type)
		if r.Topic != "" {
			fmt.Fprintf(&sb, " %s:", r.Topic)
		}
		fmt.Fprintf(&sb, " %s\n", r.Content)
	}
	return "info", "项目记忆", sb.String()
}
