package miniagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Model returns the per-chat pinned model id, or "" when none is set (the
// caller falls back to the global default). Read from the .model file.
// Nil receiver returns "" so activeModel can short-circuit when memory is off.
func (h *History) Model(chatID string) string {
	if h == nil {
		return ""
	}
	return readPin(h, h.modelPath(chatID))
}

// SetModel pins model for chatID (atomic temp+rename, same pattern as
// writeCur). An empty model clears the pin (removes the .model file).
// Nil receiver yields the same error as every other mutation on a
// disabled History.
func (h *History) SetModel(chatID, model string) error {
	if h == nil {
		return errors.New("miniagent: memory disabled")
	}
	return writePin(h, h.modelPath(chatID), model)
}

func (h *History) modelPath(chatID string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+".model")
}

// Directory returns the per-chat pinned working directory, or "" when none
// is set (the caller falls back to the global workspace_root). The directory
// bounds read_file/write_file/shell to a sub-tree under WORKSPACE_ROOT so
// different chats can work on different projects. Nil receiver → "".
func (h *History) Directory(chatID string) string {
	if h == nil {
		return ""
	}
	return readPin(h, h.dirPath(chatID))
}

// SetDir pins a working directory for chatID (atomic temp+rename). An empty
// dir clears the pin (removes the .dir file). The caller MUST validate that
// dir is under WORKSPACE_ROOT before calling — this method stores verbatim.
func (h *History) SetDir(chatID, dir string) error {
	if h == nil {
		return errors.New("miniagent: memory disabled")
	}
	return writePin(h, h.dirPath(chatID), dir)
}

func (h *History) dirPath(chatID string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+".dir")
}

// Permission returns the per-chat pinned permission mode, or "" when none
// is set (the caller falls back to the global default). Nil receiver → "".
func (h *History) Permission(chatID string) string {
	if h == nil {
		return ""
	}
	return readPin(h, h.permPath(chatID))
}

// SetPermission pins a permission mode for chatID (atomic temp+rename).
// An empty mode clears the pin.
func (h *History) SetPermission(chatID, mode string) error {
	if h == nil {
		return errors.New("miniagent: memory disabled")
	}
	return writePin(h, h.permPath(chatID), mode)
}

func (h *History) permPath(chatID string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+".perm")
}

// readPin returns the trimmed content of path, or "" on any read error.
// Callers guard the nil receiver before computing path.
func readPin(h *History, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writePin writes value to path atomically (temp+rename under h.dir). An
// empty value removes the pin file. Callers guard the nil receiver first;
// h is non-nil here by contract. Folding this helper removes ~90 lines of
// near-identical boilerplate across the three pin setters.
func writePin(h *History, path, value string) error {
	if value == "" {
		_ = os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(h.dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(value); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
