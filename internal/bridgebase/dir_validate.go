package bridgebase

import (
	"fmt"
	"os"
	"path/filepath"
)

// DirPerm is the owner-only permission for per-chat session working
// directories. The CLI runs git/subprocesses inside these dirs; 0o700 keeps
// other local users out of session state. Shared across all CLI bridges.
const DirPerm = 0o700

// ValidateAbsDir checks that dir is an absolute path, an existing directory,
// and writable by the current uid — the same uid the CLI subprocess will run
// as, so the probe result is authoritative. The writability check is what
// makes a systemd ReadWritePaths exclusion surface here (with a clear message)
// rather than mid-turn inside the agent's edit flow.
func ValidateAbsDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("目录不可访问：%w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("路径不是目录：%s", dir)
	}

	probe, err := os.MkdirTemp(dir, ".cdprobe-*")
	if err != nil {
		return fmt.Errorf("目录不可写（可能被 systemd ReadWritePaths 排除或 Unix 权限不足）：%w", err)
	}
	_ = os.Remove(probe)
	return nil
}

// ValidateSessionDirPath checks the shape of a session directory the bridge is
// about to create from an Event-carried override: it must be an absolute path.
// Event.Directory is empty in production (the frontend never sets it), so this
// is defence in depth — the workspace boundary is enforced by /cd.
//
// IsAbs only, by design: a relative path (including "..") does not begin with
// "/", so IsAbs already rejects it; a ".." segment inside an absolute path
// (e.g. "/a/../b") is resolved by the filesystem to a concrete path at
// MkdirAll/CWD time and is not a traversal escape. The workspace root boundary
// is enforced separately — /cd's ValidateAbsDir and bridgebase's filepath.Rel
// check both Clean before comparing. Existence is not required (unlike /cd's
// ValidateAbsDir) — ensureBinding creates the dir via CreateSessionDir on
// demand.
func ValidateSessionDirPath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	return nil
}

// CreateSessionDir creates dir (and parents) with the owner-only DirPerm. Used
// by every bridge's ensureBinding to materialise a per-chat working directory
// before binding the chat to it.
func CreateSessionDir(dir string) error {
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	return nil
}
