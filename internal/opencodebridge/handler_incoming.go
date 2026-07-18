package opencodebridge

import (
	"fmt"
	"os"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// dirPerm is the owner-only permission for per-chat session working
// directories. The CLI runs git/subprocesses inside these dirs; 0o700 keeps
// other local users out of session state.
const dirPerm = 0o700

// ensureBinding returns the binding for chatID, creating one on first use, and
// applies any per-prompt overrides carried by the Event (sessionID, directory,
// modelSpec, agent). opencode sessions are lazy: the binding starts with an
// empty session id (filled from the first run's session event by streamRun)
// and a per-chat working directory under the configured default directory.
//
// When a binding already exists, the non-empty overrides are applied via the
// matching Set* accessor so the next run resumes the updated session / dir /
// model / agent.
func (h *Handler) ensureBinding(chatID, sessionID, directory, modelSpec, agent string) (binding router.Binding, err error) {
	// An Event may carry a directory override. Validate its shape before any
	// MkdirAll so an untrusted source cannot make the subprocess CWD escape the
	// intended tree (mirrors /cd's validateAbsDir, but without the existence
	// check — the dir is created on demand below).
	if directory != "" {
		if err := validateSessionDirPath(directory); err != nil {
			return router.Binding{}, err
		}
	}
	if b, ok := h.Router.Lookup(chatID); ok {
		if sessionID != "" {
			h.Router.SetSessionID(chatID, sessionID)
			b.SessionID = sessionID
		}
		if directory != "" {
			if err := os.MkdirAll(directory, dirPerm); err != nil {
				return router.Binding{}, fmt.Errorf("create session dir: %w", err)
			}
			h.Router.SetDirectory(chatID, directory)
			b.Directory = directory
		}
		if modelSpec != "" {
			h.Router.SetModelSpec(chatID, modelSpec)
			b.ModelSpec = modelSpec
		}
		if agent != "" {
			h.Router.SetAgent(chatID, agent)
			b.Agent = agent
		}
		return b, nil
	}
	// Create the binding without a directory: the user must /cd into a
	// project before the first prompt runs. sessionDirectory is only
	// computed on demand, so no dir is created here.
	if directory != "" {
		if err := os.MkdirAll(directory, dirPerm); err != nil {
			return router.Binding{}, fmt.Errorf("create session dir: %w", err)
		}
	}
	// Empty session id -> streamRun back-fills it after the first run.
	h.Router.Bind(chatID, sessionID, directory, "", modelSpec, agent)
	b, _ := h.Router.Lookup(chatID)
	h.Logger.Info("binding created",
		log.FieldChatID, chatID,
		log.FieldDirectory, directory)
	return b, nil
}
