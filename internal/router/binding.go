package router

import (
	"github.com/justphantom/lark-bridge/internal/log"
)

// Bind forcibly maps chatID to the given sessionID, directory, title,
// modelSpec and agent, overwriting any prior binding for chatID. Used by
// claude ensureBinding (with agent="") and by opencode /session-use /
// /session-new. The modelSpec / agent fields are written verbatim; pass "" to
// clear.
func (r *Router) Bind(chatID, sessionID, directory, title, modelSpec, agent string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings[chatID] = Binding{
		SessionID: sessionID,
		Directory: directory,
		Title:     title,
		ModelSpec: modelSpec,
		Agent:     agent,
	}
	r.saveAsync()
	r.logger.Info("binding stored",
		log.FieldChatID, chatID,
		log.FieldSessionID, sessionID,
		log.FieldDirectory, directory,
		"model", modelSpec,
		"agent", agent)
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
