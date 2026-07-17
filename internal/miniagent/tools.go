package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// maxReadFileChars bounds one read_file result so a huge file cannot blow
// the LLM context window. Truncated with a marker.
const maxReadFileChars = 20000

// maxShellOutputChars is the same cap for shell combined stdout+stderr.
const maxShellOutputChars = 20000

// shellTimeout bounds one shell command; a hung command is killed and the
// partial output returned with an error marker.
const shellTimeout = 60 * time.Second

// maxWebFetchChars is the same cap for webfetch text output.
const maxWebFetchChars = 20000

// webfetchTimeout bounds one HTTP GET.
const webfetchTimeout = 30 * time.Second

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
	"/dev/null > /dev/sd",
	"chmod -R 000",
	"chown -R",
}

// Tool is one agent tool the LLM may call. Spec returns the OpenAI
// function schema (name/description/parameters); Call executes it with the
// raw JSON arguments string the LLM produced.
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, args string) ToolResult
}

// ToolResult is the outcome of one tool call. Output is fed back to the
// LLM verbatim as the tool message content; IsError tells the LLM the call
// failed (it should not parse Output as success data).
type ToolResult struct {
	Output  string
	IsError bool
}

// readfileArgs is the LLM-supplied argument object for read_file.
type readfileArgs struct {
	Path string `json:"path"`
}

// ReadFile reads a text file under WorkspaceRoot. It is the P1a tool;
// write_file / shell / webfetch land later. Call enforces WorkspaceRoot:
// a path that escapes it (after Clean) returns an error result instead of
// touching the filesystem.
type ReadFile struct {
	WorkspaceRoot string // absolute or relative; cleaned in Call
}

func (ReadFile) Spec() ToolSpec {
	return ToolSpec{
		Name:        "read_file",
		Description: "读取 workspace_root 内的文本文件内容。path 可以是绝对路径或相对 workspace_root 的路径。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要读取的文件路径，相对 workspace_root 或绝对路径",
				},
			},
			"required": []string{"path"},
		},
	}
}

// Call resolves path under WorkspaceRoot, reads the file, and truncates.
// Any failure (root unset, escape attempt, missing file, not a regular
// file, read error) yields IsError=true with a human-readable Output.
func (r ReadFile) Call(_ context.Context, args string) ToolResult {
	if strings.TrimSpace(r.WorkspaceRoot) == "" {
		return ToolResult{IsError: true, Output: "read_file 未配置：workspace_root 为空"}
	}
	var a readfileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Path == "" {
		return ToolResult{IsError: true, Output: "参数缺失：path"}
	}

	root, err := filepath.Abs(r.WorkspaceRoot)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("解析 workspace_root 失败：%v", err)}
	}
	full, err := resolveUnderRoot(root, a.Path)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}

	info, err := os.Stat(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("无法访问 %q：%v", a.Path, err)}
	}
	if info.IsDir() {
		return ToolResult{IsError: true, Output: fmt.Sprintf("%q 是目录，不是文件", a.Path)}
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: truncate(string(data), maxReadFileChars, "…")}
}

// resolveUnderRoot cleans p and ensures the result stays under root. It
// accepts an absolute path inside root or a path relative to root. A path
// that escapes root via ".." or is absolute but outside root returns an
// error naming the offending path.
func resolveUnderRoot(root, p string) (string, error) {
	clean := filepath.Clean(p)
	var full string
	if filepath.IsAbs(clean) {
		full = clean
	} else {
		full = filepath.Join(root, clean)
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", fmt.Errorf("路径 %q 不在 workspace_root 内", p)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径 %q 越出 workspace_root", p)
	}
	return full, nil
}

// truncate clamps s to n runes and appends marker when it truncated. rune-
// based so multibyte content (中文) is never split mid-character. n<=0 means
// no limit. marker is "" for a silent cut, or a visible suffix like "…".
func truncate(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + marker
}

// shellArgs is the LLM-supplied argument object for shell.
type shellArgs struct {
	Command string `json:"command"`
}

// Shell runs one shell command under WorkspaceRoot via `sh -c`. It is the
// destructive-capable counterpart to ReadFile: cwd is pinned to
// WorkspaceRoot (so relative paths land there), but a command can still
// escape via absolute paths or cd, so shellBlockedPatterns refuses the
// most destructive shapes as a coarse tripwire (NOT a security boundary —
// see its doc). Output is stdout+stderr combined, truncated to
// maxShellOutputChars. A command that exceeds shellTimeout is killed.
type Shell struct {
	WorkspaceRoot string
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

// Call runs the command. Empty WorkspaceRoot → error. A blocked pattern →
// error. Otherwise CombinedOutput under a timeout, truncated on return.
func (s Shell) Call(ctx context.Context, args string) ToolResult {
	if strings.TrimSpace(s.WorkspaceRoot) == "" {
		return ToolResult{IsError: true, Output: "shell 未配置：workspace_root 为空"}
	}
	var a shellArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if strings.TrimSpace(a.Command) == "" {
		return ToolResult{IsError: true, Output: "参数缺失：command"}
	}
	if msg := blockedShellReason(a.Command); msg != "" {
		return ToolResult{IsError: true, Output: msg}
	}

	root, err := filepath.Abs(s.WorkspaceRoot)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("解析 workspace_root 失败：%v", err)}
	}
	if _, err := os.Stat(root); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("workspace_root 不可访问：%v", err)}
	}

	runCtx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()
	// #nosec G204 -- the agent's whole purpose is to run LLM-chosen shell;
	// the workspace_root pin + blocked-pattern tripwire + unprivileged user
	// are the accepted boundaries, not command allow-listing.
	cmd := exec.CommandContext(runCtx, "sh", "-c", a.Command)
	cmd.Dir = root
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

// writefileArgs is the LLM-supplied argument object for write_file.
type writefileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteFile writes text content to a path under WorkspaceRoot, creating the
// file if absent and truncating if present (overwrite semantics). Missing
// parent directories are created. Path safety is the same as ReadFile: a
// path that escapes WorkspaceRoot (after Clean) is refused.
type WriteFile struct {
	WorkspaceRoot string
}

func (WriteFile) Spec() ToolSpec {
	return ToolSpec{
		Name:        "write_file",
		Description: "把 content 写入 workspace_root 内的文件（覆盖已有内容；自动创建父目录）。path 可相对 workspace_root 或绝对。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要写入的文件路径，相对 workspace_root 或绝对路径",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "要写入的完整文件内容",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

// Call resolves path under WorkspaceRoot, creates parent dirs, and writes.
// Any failure (root unset, escape, mkdir failure, write failure) yields
// IsError=true. Returns the bytes written on success.
func (w WriteFile) Call(_ context.Context, args string) ToolResult {
	if strings.TrimSpace(w.WorkspaceRoot) == "" {
		return ToolResult{IsError: true, Output: "write_file 未配置：workspace_root 为空"}
	}
	var a writefileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Path == "" {
		return ToolResult{IsError: true, Output: "参数缺失：path"}
	}

	root, err := filepath.Abs(w.WorkspaceRoot)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("解析 workspace_root 失败：%v", err)}
	}
	full, err := resolveUnderRoot(root, a.Path)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}

	// MkdirAll so the LLM can write src/new.go without a separate mkdir
	// tool. The parent dir is already bounded to workspace_root by
	// resolveUnderRoot, so this cannot create dirs outside it.
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("创建父目录失败：%v", err)}
	}
	// 0644: standard project-file mode (owner rw, group/other r). Matches
	// what `cat > file` or an editor produces, so the file interops with
	// shell/git without surprising permission diffs.
	if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("写入 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: fmt.Sprintf("已写入 %d 字节到 %s", len(a.Content), a.Path)}
}

// webfetchArgs is the LLM-supplied argument object for webfetch.
type webfetchArgs struct {
	URL string `json:"url"`
}

// WebFetch GETs one http(s) URL and returns its text content. The HTML is
// parsed via golang.org/x/net/html; script/style/title nodes are dropped
// and the remaining text is concatenated, collapsing whitespace, then
// truncated to maxWebFetchChars.
//
// KNOWN SSRF RISK (accepted): scheme is checked to be http/https, but the
// host is NOT screened for private/loopback/link-local addresses. The LLM
// can therefore direct the agent at internal endpoints (e.g. cloud
// metadata 169.254.169.254, localhost services). Treat the miniagent user
// as the boundary, not URL filtering. Add host screening if this backend
// ever runs somewhere where internal reachability is sensitive.
type WebFetch struct {
	HTTP *http.Client // nil → default with webfetchTimeout
}

func (WebFetch) Spec() ToolSpec {
	return ToolSpec{
		Name:        "webfetch",
		Description: "抓取一个 http(s) 网页并返回其纯文本内容（已去掉 script/style/HTML 标签，最长 " + fmt.Sprintf("%d", maxWebFetchChars) + " 字符）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "要抓取的完整 http(s) URL",
				},
			},
			"required": []string{"url"},
		},
	}
}

// Call GETs the URL, checks for a 2xx, and extracts text. Any failure
// (bad scheme, non-2xx, transport error, parse error) yields IsError=true.
func (w WebFetch) Call(ctx context.Context, args string) ToolResult {
	var a webfetchArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if strings.TrimSpace(a.URL) == "" {
		return ToolResult{IsError: true, Output: "参数缺失：url"}
	}
	if !isHTTPURL(a.URL) {
		return ToolResult{IsError: true, Output: fmt.Sprintf("仅支持 http/https URL，收到 %q", a.URL)}
	}

	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: webfetchTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("构造请求失败：%v", err)}
	}
	req.Header.Set("User-Agent", "lark-miniagent-back/webfetch")
	resp, err := client.Do(req)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("抓取失败：%v", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB raw cap
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取响应失败：%v", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ToolResult{IsError: true, Output: fmt.Sprintf("%s 返回 %d：%s", a.URL, resp.StatusCode, truncate(string(body), 200, "…"))}
	}

	ctype := resp.Header.Get("Content-Type")
	if strings.Contains(ctype, "text/html") {
		text, perr := htmlToText(body)
		if perr != nil {
			// Fall back to raw body rather than failing the whole call:
			// the LLM may still glean something from partial markup.
			return ToolResult{Output: truncate(string(body), maxWebFetchChars, "…")}
		}
		return ToolResult{Output: truncate(text, maxWebFetchChars, "…")}
	}
	// Non-HTML (plain text, json, etc.) returned as-is.
	return ToolResult{Output: truncate(string(body), maxWebFetchChars, "…")}
}

// isHTTPURL reports whether u starts with an http:// or https:// scheme
// (case-insensitive). It does NOT validate the rest of the URL — the http
// client will surface malformed URLs as transport errors.
func isHTTPURL(u string) bool {
	low := strings.ToLower(u)
	return strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://")
}

// blockTags are HTML elements that force a line break in the extracted
// text. Inline tags (b/i/a/span/…) are NOT here so their text concatenates
// naturally with surrounding text (e.g. "hello <b>world</b>" → "hello world",
// not "hello\nworld"). Used by htmlToText.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "dd": true, "div": true, "dl": true, "dt": true,
	"fieldset": true, "figcaption": true, "figure": true, "footer": true,
	"form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "header": true, "hr": true, "li": true, "main": true,
	"nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"table": true, "tbody": true, "td": true, "tfoot": true, "th": true,
	"thead": true, "tr": true, "ul": true,
}

// htmlToText parses an HTML body and returns its visible text: script,
// style, title, and noscript subtrees are skipped; block-level elements
// force a line break (so <p>hello <b>world</b></p> renders as "hello world"
// on one line); inline text concatenates with surrounding text. Used by
// webfetch so the LLM gets readable content instead of raw markup.
func htmlToText(body []byte) (string, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "title", "noscript":
				return // skip entire subtree
			}
		}
		if n.Type == html.ElementNode && blockTags[n.Data] && b.Len() > 0 {
			b.WriteByte('\n')
		}
		if n.Type == html.TextNode {
			s := strings.TrimSpace(n.Data)
			if s != "" {
				b.WriteString(s)
				if !strings.HasSuffix(s, " ") {
					b.WriteByte(' ')
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	// Collapse the trailing space each text node appended; TrimSpace handles
	// the edges and a single trailing space, but a double space can survive
	// when two inline nodes meet at a block boundary.
	return strings.TrimSpace(b.String()), nil
}
