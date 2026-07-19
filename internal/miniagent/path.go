package miniagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveUnderRoot cleans p and ensures the result stays under root, both on
// the string level and on the filesystem level (EvalSymlinks on the parent
// dir and on the final path if it exists). Used by the /cd command to keep
// the per-chat working-directory pin inside WORKSPACE_ROOT.
func resolveUnderRoot(root, p string) (string, error) {
	clean := filepath.Clean(p)
	var full string
	if filepath.IsAbs(clean) {
		full = clean
	} else {
		full = filepath.Join(root, clean)
	}
	if err := checkUnderRoot(root, full, p); err != nil {
		return "", err
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("路径 %q 父目录解析失败：%w", p, err)
		}
		return full, nil
	}
	resolved := filepath.Join(realParent, filepath.Base(full))
	if err := checkUnderRoot(root, resolved, p); err != nil {
		return "", err
	}
	return resolved, nil
}

func checkUnderRoot(root, full, original string) error {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return fmt.Errorf("路径 %q 不在 workspace_root 内", original)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %q 越出 workspace_root", original)
	}
	return nil
}

// sanitizeChatID collapses any character unsafe for a filename into '_' so a
// chatID with an unexpected character cannot escape the state directory.
// Mirrors the CLI's sanitizeChatID so bridge-side test seeds land where the
// CLI will read them.
func sanitizeChatID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
