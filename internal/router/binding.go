package router

import (
	"github.com/justphantom/lark-bridge/internal/log"
)

// Bind forcibly maps chatID to the given sessionID, directory, title and
// modelSpec, overwriting any prior binding for chatID. Used by /new and /use.
// The modelSpec field is written verbatim; pass "" to clear.
func (r *Router) Bind(chatID, sessionID, directory, title, modelSpec string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings[chatID] = Binding{
		SessionID: sessionID,
		Directory: directory,
		Title:     title,
		ModelSpec: modelSpec,
	}
	r.saveAsync()
	r.logger.Info("binding stored",
		log.FieldChatID, chatID,
		log.FieldSessionID, sessionID,
		log.FieldDirectory, directory,
		"model", modelSpec)
}

// Unbind removes the binding for chatID. Used by /session-del.
func (r *Router) Unbind(chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bindings, chatID)
	r.saveAsync()
	r.logger.Info("binding deleted",
		log.FieldChatID, chatID)
}
