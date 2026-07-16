package goosebridge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/router"
)

// dirPerm is the owner-only permission for per-chat session working
// directories. The CLI runs git/subprocesses inside these dirs; 0o700 keeps
// other local users out of session state.
const dirPerm = 0o700

// ensureBinding returns the binding for chatID, creating one on first use,
// and applies any per-prompt overrides carried by the Event (sessionID,
// directory, modelSpec). goose sessions are lazy — the binding starts with
// an empty session anchor (filled from the first successful run by
// streamRun, which stores the --name) and a per-chat working directory
// under the configured default directory.
//
// When a binding already exists, the non-empty overrides are applied via the
// matching Set* accessor so the next run resumes the updated session / dir /
// model.
func (h *Handler) ensureBinding(chatID, sessionID, directory, modelSpec, title string) (binding router.Binding, err error) {
	// An Event may carry a directory override. Validate its shape before any
	// MkdirAll so an untrusted source cannot make the subprocess CWD escape the
	// intended tree (mirrors /cd's validateAbsDir, but without the existence
	// check — the dir is created on demand below).
	if directory != "" {
		if err := validateSessionDirPath(directory); err != nil {
			return router.Binding{}, err
		}
	}
	if b, ok := h.router.Lookup(chatID); ok {
		if sessionID != "" {
			h.router.SetSessionID(chatID, sessionID)
			b.SessionID = sessionID
		}
		if directory != "" {
			if err := os.MkdirAll(directory, dirPerm); err != nil {
				return router.Binding{}, fmt.Errorf("create session dir: %w", err)
			}
			h.router.SetDirectory(chatID, directory)
			b.Directory = directory
		}
		if modelSpec != "" {
			h.router.SetModelSpec(chatID, modelSpec)
			b.ModelSpec = modelSpec
		}
		return b, nil
	}
	// Create the binding without a directory: the user must /cd into a
	// project before the first prompt runs. sessionDirectory is only
	// computed on demand (in runPrompt via /cd), so no dir is created here.
	if directory != "" {
		if err := os.MkdirAll(directory, dirPerm); err != nil {
			return router.Binding{}, fmt.Errorf("create session dir: %w", err)
		}
	}
	// Empty session id → streamRun back-fills it after the first run.
	h.router.Bind(chatID, sessionID, directory, title, modelSpec, "")
	b, _ := h.router.Lookup(chatID)
	h.logger.Info("binding created",
		log.FieldChatID, chatID,
		log.FieldDirectory, directory)
	return b, nil
}

// sessionDirectory returns the per-chat working directory under the
// configured default directory. The chatID is sanitised so an unusual chat
// id cannot escape the base directory.
func (h *Handler) sessionDirectory(chatID string) string {
	base := h.defaultDirectory
	if base == "" {
		base = h.stateDir
	}
	if base == "" {
		base = "."
	}
	return filepath.Join(base, sanitizeForPath(chatID))
}

// sanitizeForPath reduces s to a single safe path component.
func sanitizeForPath(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "session"
	}
	return out
}
