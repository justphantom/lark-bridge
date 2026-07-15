package router

import (
	"context"
	"errors"

	"github.com/hu/lark-bridge/internal/log"
)

// GetOrCreate returns the binding for chatID, creating a new session on first
// call (opencode path). directory is forwarded to CreateSessionInDirectory;
// an empty directory lets the server pick its default. Subsequent calls
// return the existing binding unchanged. title is the Feishu chat name
// snapshot used for display. modelSpec and agent are the initial pinned model
// and agent (written the first time it is created; use SetModelSpec /
// SetAgent to update them later).
//
// The slow path (HTTP create) runs WITHOUT holding r.mu, so a long
// CreateSession does not block Lookup / Bind / SetModelSpec / UpdateTitle for
// other chatIDs. Concurrent first-time calls for the same chatID may both
// issue a CreateSession; the loser detects the winner's binding on re-acquire
// and returns it.
//
// Panics if the router was constructed with a nil SessionCreator
// (claude-back never calls this method).
func (r *Router) GetOrCreate(ctx context.Context, chatID, directory, title, modelSpec, agent string) (Binding, error) {
	// Fast path: existing binding under RLock; refresh title only when changed.
	r.mu.RLock()
	binding, ok := r.bindings[chatID]
	r.mu.RUnlock()
	if ok {
		if title != "" && binding.Title != title {
			r.mu.Lock()
			b, ok2 := r.bindings[chatID]
			if ok2 && b.Title != title {
				b.Title = title
				r.bindings[chatID] = b
				r.saveAsync()
			}
			r.mu.Unlock()
			binding.Title = title
		}
		return binding, nil
	}

	// Slow path: create the session out of lock so other chatIDs' lookups do
	// not queue behind this HTTP call.
	id, realDir, err := r.create.CreateSessionInDirectory(ctx, "feishu:"+chatID, directory)
	if err != nil {
		return Binding{}, err
	}
	if id == "" {
		return Binding{}, errors.New("router: creator returned empty session id")
	}
	effectiveDir := realDir
	if effectiveDir == "" {
		effectiveDir = directory
	}
	newBinding := Binding{
		SessionID: id,
		Directory: effectiveDir,
		Title:     title,
		ModelSpec: modelSpec,
		Agent:     agent,
	}

	// Re-acquire write lock and reconcile with any concurrent winner.
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.bindings[chatID]; ok {
		return existing, nil
	}
	r.bindings[chatID] = newBinding
	r.saveAsync()
	r.logger.Info("binding created",
		log.FieldChatID, chatID,
		log.FieldSessionID, id,
		log.FieldDirectory, effectiveDir,
		"model", modelSpec,
		"agent", agent)
	return newBinding, nil
}

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

// UpdateTitle refreshes the persisted title for chatID. No-op when the chatID
// has no binding or title is empty. Used by the opencode handler to keep the
// title in sync with the latest Feishu chat name.
func (r *Router) UpdateTitle(chatID, title string) {
	if title == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.bindings[chatID]
	if !ok {
		return
	}
	if binding.Title == title {
		return
	}
	binding.Title = title
	r.bindings[chatID] = binding
	r.saveAsync()
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
