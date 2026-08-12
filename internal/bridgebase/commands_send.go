package bridgebase

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// MaxSendFileSize is the per-file ceiling for /send, matching the Feishu IM
// file-message API limit. Enforced before base64 (which would inflate 33%)
// so a too-large file is rejected with a friendly notice rather than a
// mid-upload failure or an oversized IPC payload.
const MaxSendFileSize = 30 << 20 // 30 MiB

// sendDirOptionLimit caps one picker card's option count at the button-render
// ceiling (cardkit.MaxCardElements − header/markdown/footer = 45). The
// directory browser renders as immediate-click buttons (no select_static/
// form_submit) so a pick commits in
// one click without opening Feishu's long form/select callback window — the
// window that silently reverted the picker PATCH (回弹) and forced the "已发送"
// outcome onto a duplicate card. A huge directory truncates; the user reaches
// cut entries via /send <path>.
const sendDirOptionLimit = 45

// SafeJoin constrains rel under root, resolving symlinks so a link that points
// outside root cannot escape (send-file-design.md §5.1). root and the joined
// target are both Abs/Clean-ed first; EvalSymlinks then resolves the real
// path, and a Rel check confirms it did not land above root. Returns the
// resolved absolute path or an error a backend turns into a user notice.
// Errors deliberately carry no absolute server paths — the message lands in
// the chat verbatim, and leaking /home/... layouts aids an attacker.
func SafeJoin(root, rel string) (string, error) {
	absRoot, err := ResolveRoot(root)
	if err != nil {
		return "", fmt.Errorf("解析根目录失败")
	}
	rel = filepath.Clean(rel)
	target := filepath.Join(absRoot, rel)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("路径不存在或无法访问：%s", rel)
	}
	if !withinRoot(absRoot, resolved) {
		return "", fmt.Errorf("路径越界：%s", rel)
	}
	return resolved, nil
}

// ResolveRoot returns root's absolute, symlink-resolved form. The resolution
// matters when the bound directory is itself a symlink (~/proj → /data/proj):
// EvalSymlinks on the target resolves every component including root's, so
// comparing against the unresolved root would false-positive every file as
// "越界". A root that cannot be resolved falls back to the Abs/Clean form.
func ResolveRoot(root string) (string, error) {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if r, rerr := filepath.EvalSymlinks(absRoot); rerr == nil {
		absRoot = r
	}
	return absRoot, nil
}

// withinRoot reports whether resolved lies at or under absRoot. The check is
// rel == ".." or a "../" prefix — a plain ".." prefix would also reject a
// legitimate sibling named "..foo".
func withinRoot(absRoot, resolved string) bool {
	rel, err := filepath.Rel(absRoot, resolved)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// BuildSendOptions renders one directory's entries as the picker option list:
// "⬆️ .." when not at root, then sorted subdirectories ("📁 name/"), then
// sorted files ("📄 name"). Dotfiles are hidden. The list is capped at
// sendDirOptionLimit (dirs before files) so a vast directory never produces a
// card Feishu rejects; a user reaches truncated entries via /send <path>.
func BuildSendOptions(currDir, rootDir string, entries []os.DirEntry) []string {
	absCurr, _ := filepath.Abs(filepath.Clean(currDir))
	absRoot, _ := filepath.Abs(filepath.Clean(rootDir))
	var dirs, files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, "📁 "+name+"/")
		} else {
			files = append(files, "📄 "+name)
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	options := make([]string, 0, 1+len(dirs)+len(files))
	if absCurr != absRoot {
		options = append(options, "⬆️ ..")
	}
	options = append(options, dirs...)
	options = append(options, files...)
	if len(options) > sendDirOptionLimit {
		options = options[:sendDirOptionLimit]
	}
	return options
}

// ParseSendOption decodes one BuildSendOptions entry back into a kind and a
// raw name: "up" (..), "dir" (name without trailing slash), or "file". An
// unrecognised string yields ("", "") so the caller can bail out safely.
func ParseSendOption(choice string) (kind, name string) {
	switch {
	case choice == "⬆️ ..":
		return "up", ""
	case strings.HasPrefix(choice, "📁 "):
		return "dir", strings.TrimSuffix(strings.TrimPrefix(choice, "📁 "), "/")
	case strings.HasPrefix(choice, "📄 "):
		return "file", strings.TrimPrefix(choice, "📄 ")
	}
	return "", ""
}

// ReadFilePayload reads one file, enforces the size cap, and wraps it as a
// TypeFile payload (base64 content). Shared by the bridges so the
// size/encoding/escape policy lives in one place. updateMessageID is threaded
// through so the frontend can PATCH the picker card the user just clicked
// instead of sending a standalone result notice.
//
// root is the chat's bound directory: the path is re-validated here (symlink
// resolution + containment) at read time rather than only at selection time,
// closing the swap-a-symlink TOCTOU window between the two. Non-regular files
// (FIFO/device) are rejected — os.Stat reports size 0 for a FIFO, which would
// otherwise pass the cap and then block the read forever. The read itself is
// capped at MaxSendFileSize+1 so a file growing after Stat cannot slip past.
// Errors carry no absolute server paths (they surface in the chat verbatim).
func ReadFilePayload(chatID, fileName, root, path, updateMessageID string) (*protocol.FilePayload, error) {
	absRoot, err := ResolveRoot(root)
	if err != nil {
		return nil, fmt.Errorf("工作目录无效")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("路径不存在或无法访问：%s", fileName)
	}
	if !withinRoot(absRoot, resolved) {
		return nil, fmt.Errorf("路径越界：%s", fileName)
	}
	// Stat before Open: opening a FIFO for read blocks until a writer shows
	// up, so the regular-file gate must happen on Stat (which never blocks).
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("文件不存在或无权访问：%s", fileName)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是常规文件，无法发送", fileName)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("文件 %s 为空，不支持发送空文件", fileName)
	}
	if info.Size() > MaxSendFileSize {
		return nil, fmt.Errorf("文件 %s 为 %s，超过 %s 上限", fileName, sendFileSize(info.Size()), sendFileSize(MaxSendFileSize))
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("文件不存在或无权访问：%s", fileName)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSendFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("读取文件失败：%s", fileName)
	}
	if int64(len(data)) > MaxSendFileSize {
		return nil, fmt.Errorf("文件 %s 超过 %s 上限", fileName, sendFileSize(MaxSendFileSize))
	}
	return &protocol.FilePayload{
		ChatID:          chatID,
		FileName:        fileName,
		Content:         base64.StdEncoding.EncodeToString(data),
		UpdateMessageID: updateMessageID,
	}, nil
}

// sendFileSize renders a byte count compactly for the over-limit notice.
func sendFileSize(n int64) string {
	const mi = int64(1) << 20
	if n < mi {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/float64(mi))
}
