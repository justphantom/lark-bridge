package goosebridge

import (
	"fmt"
	"os"
	"path/filepath"
)

// validateAbsDir validates a workspace directory the /cd picker is about to
// pin: absolute, exists, is a directory, and is writable by the process. The
// writability probe is what makes a systemd ReadWritePaths exclusion surface
// here (with a clear message) rather than mid-turn inside a goose tool call.
func validateAbsDir(dir string) error {
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

// validateSessionDirPath checks the shape of a session directory the bridge is
// about to create from an Event-carried override: it must be an absolute path.
// Event.Directory is empty in production (the frontend never sets it), so this
// is defence in depth. A relative path is rejected so an untrusted Event
// cannot make the subprocess CWD relative to the process working directory.
// Existence is not required — ensureBinding creates the dir via MkdirAll.
func validateSessionDirPath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	return nil
}
