package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxShellOutputChars is the output cap for shell combined stdout+stderr.
const maxShellOutputChars = 20000

// shellTimeout bounds one shell command; a hung command is killed and the
// partial output returned with an error marker.
const shellTimeout = 60 * time.Second

// shellBlockedPatterns are command substrings the shell tool refuses to run.
// Matching is case-insensitive on the raw command string and is a coarse
// guard, NOT a security boundary: a determined prompt can bypass it via
// base64 decoding, variable expansion, symlinks, etc. The real boundary is
// that the tool runs as an unprivileged user under workspace_root; treat
// this list as a tripwire on the most destructive shapes, not a sandbox.
var shellBlockedPatterns = []string{
	"rm -rf",
	"rm -fr",
	"mkfs",
	"dd if=",
	"shutdown",
	"poweroff",
	"reboot",
	"halt",
	":(){:|:&};:", // fork bomb
	"> /dev/sd",
	"chmod -R 000",
	"chown -R",
}

// shellArgs is the LLM-supplied argument object for shell.
type shellArgs struct {
	Command string `json:"command"`
}

// Shell runs one shell command under WorkspaceRoot via `sh -c`. cwd is
// pinned to WorkspaceRoot (so relative paths land there), but a command can
// still escape via absolute paths or cd, so shellBlockedPatterns refuses the
// most destructive shapes as a coarse tripwire (NOT a security boundary).
// Output is stdout+stderr combined, truncated to maxShellOutputChars. A
// command that exceeds shellTimeout is killed.
type Shell struct {
	WorkspaceRoot string
	Unrestricted  bool
}

func (Shell) Spec() ToolSpec {
	return ToolSpec{
		Name:        "shell",
		Description: "在 workspace_root 下执行一条 shell 命令（sh -c）。返回 stdout+stderr 合并输出。破坏性命令会被拒绝；命令最长运行 " + shellTimeout.String() + "。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "要执行的 shell 命令，相对路径基于 workspace_root",
				},
			},
			"required": []string{"command"},
		},
	}
}

// Call runs the command. In default mode: empty WorkspaceRoot → error, a
// blocked pattern → error, cwd pinned to root. In free mode (Unrestricted):
// skip blocklist + cwd lock + root check, but env isolation is ALWAYS on.
func (s Shell) Call(ctx context.Context, args string) ToolResult {
	var a shellArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if strings.TrimSpace(a.Command) == "" {
		return ToolResult{IsError: true, Output: "参数缺失：command"}
	}

	if !s.Unrestricted {
		if strings.TrimSpace(s.WorkspaceRoot) == "" {
			return ToolResult{IsError: true, Output: "shell 未配置：workspace_root 为空"}
		}
		if msg := blockedShellReason(a.Command); msg != "" {
			return ToolResult{IsError: true, Output: msg}
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()
	// #nosec G204 -- the agent's whole purpose is to run LLM-chosen shell.
	cmd := exec.CommandContext(runCtx, "sh", "-c", a.Command)
	if !s.Unrestricted {
		root, err := filepath.Abs(s.WorkspaceRoot)
		if err != nil {
			return ToolResult{IsError: true, Output: fmt.Sprintf("解析 workspace_root 失败：%v", err)}
		}
		if _, err := os.Stat(root); err != nil {
			return ToolResult{IsError: true, Output: fmt.Sprintf("workspace_root 不可访问：%v", err)}
		}
		cmd.Dir = root
	}
	// Env isolation is ALWAYS enforced regardless of Unrestricted — API key
	// leakage is an absolute security concern, not a convenience trade-off.
	cmd.Env = envWithoutSecrets()
	out, err := cmd.CombinedOutput()
	body := truncate(string(out), maxShellOutputChars, "…")
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return ToolResult{IsError: true, Output: body + fmt.Sprintf("\n⏱ 命令超时（>%s），已终止。", shellTimeout)}
		}
		return ToolResult{IsError: true, Output: body + fmt.Sprintf("\n退出码错误：%v", err)}
	}
	return ToolResult{Output: body}
}

// blockedShellReason returns a non-empty human reason when command matches a
// blocked pattern, "" otherwise. Case-insensitive on a folded copy.
func blockedShellReason(command string) string {
	folded := strings.ToLower(command)
	for _, p := range shellBlockedPatterns {
		if strings.Contains(folded, strings.ToLower(p)) {
			return fmt.Sprintf("拒绝执行：命令匹配黑名单模式 %q（破坏性命令已被拦截）。", p)
		}
	}
	return ""
}

// secretEnvPrefixes lists env-var name prefixes that must never reach an
// LLM-spawned shell: the miniagent/feishu/ipc credentials plus the generic
// *_SECRET/*_KEY/*_TOKEN/*_PASSWORD shapes. The shell tool passes through
// everything else (PATH, HOME, LANG, …) so commands find their tools, but
// these are stripped so `env` or a leaked error log cannot exfiltrate keys.
var secretEnvPrefixes = []string{
	"MINIAGENT_",
	"FEISHU_",
	"IPC_",
}

// envWithoutSecrets returns os.Environ() with secret-bearing entries removed.
// A var is dropped when its name (the part before '=') starts with any
// secretEnvPrefix OR exactly matches the generic suffixes *_SECRET / *_KEY /
// *_TOKEN / *_PASSWORD (case-insensitive), which covers most provider
// conventions without enumerating each one.
func envWithoutSecrets() []string {
	out := make([]string, 0, 64)
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if isSecretEnv(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isSecretEnv reports whether name looks like a credential var. Prefix match
// is exact-case (env names are conventionally UPPER); suffix match is
// case-insensitive since conventions vary.
func isSecretEnv(name string) bool {
	for _, p := range secretEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	upper := strings.ToUpper(name)
	switch {
	case strings.HasSuffix(upper, "_SECRET"),
		strings.HasSuffix(upper, "_KEY"),
		strings.HasSuffix(upper, "_TOKEN"),
		strings.HasSuffix(upper, "_PASSWORD"),
		strings.HasSuffix(upper, "_API_KEY"):
		return true
	}
	return false
}
