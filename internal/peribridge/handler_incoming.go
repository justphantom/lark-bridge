package peribridge

import (
	"fmt"
	"os"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/router"
)

// dirPerm is the owner-only permission for per-chat working directories.
const dirPerm = 0o700

// ensureBinding returns the binding for chatID, creating one on first use, and
// applies any per-prompt overrides carried by the Event (directory, modelSpec).
//
// peri is stateless: there is no session id to capture or resume, so this
// never touches SessionID. The binding still records directory (so /cd
// persists) and modelSpec (so model pinning persists).
func (h *Handler) ensureBinding(chatID, directory, modelSpec string) (router.Binding, error) {
	if directory != "" {
		if err := validateSessionDirPath(directory); err != nil {
			return router.Binding{}, err
		}
	}
	if b, ok := h.router.Lookup(chatID); ok {
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
	if directory != "" {
		if err := os.MkdirAll(directory, dirPerm); err != nil {
			return router.Binding{}, fmt.Errorf("create session dir: %w", err)
		}
	}
	// SessionID is always "" for peri (stateless). The router.Bind signature
	// still takes it for cross-backend uniformity.
	h.router.Bind(chatID, "", directory, "", modelSpec, "")
	b, _ := h.router.Lookup(chatID)
	h.logger.Info("binding created",
		log.FieldChatID, chatID,
		log.FieldDirectory, directory)
	return b, nil
}
