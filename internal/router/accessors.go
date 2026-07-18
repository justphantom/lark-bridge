package router

import (
	"github.com/justphantom/lark-bridge/internal/log"
)

// AllBindings returns a snapshot of every chat→Binding mapping the router
// knows about. The returned map is owned by the caller.
func (r *Router) AllBindings() map[string]Binding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Binding, len(r.bindings))
	for k, v := range r.bindings {
		out[k] = v
	}
	return out
}

// Lookup returns the binding currently bound to chatID. The ok result is
// false when no binding exists.
func (r *Router) Lookup(chatID string) (Binding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	binding, ok := r.bindings[chatID]
	return binding, ok
}

// TitleOf returns the title bound to chatID, or "".
func (r *Router) TitleOf(chatID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if b, ok := r.bindings[chatID]; ok {
		return b.Title
	}
	return ""
}

// mutate loads the binding for chatID, lets fn patch it in place, and
// persists the result when the binding exists and fn actually changed it. It
// is the shared lock/read/assign/saveAsync backbone for the Set* accessors so
// each one only carries its own field assignment. Returns whether the binding
// was changed (and thus persisted).
func (r *Router) mutate(chatID string, fn func(*Binding)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.bindings[chatID]
	if !ok {
		return false
	}
	fn(&binding)
	if binding == r.bindings[chatID] {
		return false
	}
	r.bindings[chatID] = binding
	r.saveAsync()
	return true
}

// SetModelSpec replaces the pinned model on the binding for chatID and
// persists the change. No-op when the binding does not exist. Pass "" to
// clear.
func (r *Router) SetModelSpec(chatID, modelSpec string) {
	r.mutate(chatID, func(b *Binding) { b.ModelSpec = modelSpec })
}

// SetAgent replaces the pinned agent on the binding for chatID and persists
// the change. No-op when the binding does not exist. Pass "" to clear.
func (r *Router) SetAgent(chatID, agent string) {
	r.mutate(chatID, func(b *Binding) { b.Agent = agent })
}

// SetSessionID replaces the session id on the binding for chatID and persists
// the change. The Claude backend learns its session id lazily from the first
// run's system/init event; this method lets the stream loop back-fill it once
// observed. No-op when no binding exists or the id is unchanged.
func (r *Router) SetSessionID(chatID, sessionID string) {
	if r.mutate(chatID, func(b *Binding) { b.SessionID = sessionID }) {
		r.logger.Info("binding session id updated",
			log.FieldChatID, chatID,
			log.FieldSessionID, sessionID)
	}
}

// SetDirectory replaces the working directory on the binding for chatID and
// persists the change. No-op when the binding does not exist.
func (r *Router) SetDirectory(chatID, directory string) {
	r.mutate(chatID, func(b *Binding) { b.Directory = directory })
}

// SetPermissionMode replaces the pinned Claude permission mode on the binding
// for chatID and persists the change. No-op when the binding does not exist.
// Pass "" to clear.
func (r *Router) SetPermissionMode(chatID, permissionMode string) {
	r.mutate(chatID, func(b *Binding) { b.PermissionMode = permissionMode })
}

// SetEffortLevel replaces the pinned Claude effort level on the binding for
// chatID and persists the change. No-op when the binding does not exist. Pass
// "" to clear.
func (r *Router) SetEffortLevel(chatID, effortLevel string) {
	r.mutate(chatID, func(b *Binding) { b.EffortLevel = effortLevel })
}

// SetSettingsFile replaces the pinned Claude --settings file path on the
// binding for chatID and persists the change. No-op when the binding does not
// exist. Pass "" to clear.
func (r *Router) SetSettingsFile(chatID, settingsFile string) {
	r.mutate(chatID, func(b *Binding) { b.SettingsFile = settingsFile })
}
