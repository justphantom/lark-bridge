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

// SetProvider replaces the pinned miniagent -provider on the binding for chatID
// and persists the change. No-op when the binding does not exist. Pass "" to
// clear. Paired with ModelSpec: miniagent post-v4.0.1 (02f8f81) requires
// -provider/-model as a matched pair, so the model picker sets both together.
func (r *Router) SetProvider(chatID, provider string) {
	r.mutate(chatID, func(b *Binding) { b.Provider = provider })
}

// SetAgent replaces the pinned agent on the binding for chatID and persists
// the change. No-op when the binding does not exist. Pass "" to clear.
func (r *Router) SetAgent(chatID, agent string) {
	r.mutate(chatID, func(b *Binding) { b.Agent = agent })
}

// SetSessionIDIfGeneration replaces the session id on the binding for chatID,
// but only if the binding exists and its Generation matches the one observed by
// the caller. This prevents a turn that started on one binding from clobbering
// a binding that was deleted and recreated (/session-del followed by a new
// prompt) while the turn was in flight. Returns whether the session id was
// written.
func (r *Router) SetSessionIDIfGeneration(chatID, sessionID string, generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.bindings[chatID]
	if !ok || binding.Generation != generation {
		return false
	}
	if binding.SessionID == sessionID {
		return false
	}
	binding.SessionID = sessionID
	r.bindings[chatID] = binding
	r.saveAsync()
	r.logger.Info("binding session id updated",
		log.FieldChatID, chatID,
		log.FieldSessionID, sessionID)
	return true
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

// SetMode replaces the pinned miniagent -mode on the binding for chatID and
// persists the change. No-op when the binding does not exist. Pass "" to clear.
func (r *Router) SetMode(chatID, mode string) {
	r.mutate(chatID, func(b *Binding) { b.Mode = mode })
}

// SetThinking replaces the pinned miniagent -thinking on the binding for chatID
// and persists the change. No-op when the binding does not exist. Pass "" to
// clear.
func (r *Router) SetThinking(chatID, thinking string) {
	r.mutate(chatID, func(b *Binding) { b.Thinking = thinking })
}

// SetMaxIterations replaces the pinned miniagent -max-iterations on the binding
// for chatID and persists the change. No-op when the binding does not exist.
// Pass 0 to clear: 0 is the zero value and means "do not pass the flag", so the
// upstream CLI picks its own default (20).
func (r *Router) SetMaxIterations(chatID string, n int) {
	r.mutate(chatID, func(b *Binding) { b.MaxIterations = n })
}

// SetConfigFile replaces the pinned miniagent -config path on the binding for
// chatID and persists the change. No-op when the binding does not exist. Pass ""
// to clear (the client's startup config path is used on the next turn).
func (r *Router) SetConfigFile(chatID, configPath string) {
	r.mutate(chatID, func(b *Binding) { b.ConfigFile = configPath })
}
