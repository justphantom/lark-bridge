package router

// Lookup returns the binding currently bound to chatID. The ok result is
// false when no binding exists.
func (r *Router) Lookup(chatID string) (Binding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	binding, ok := r.bindings[chatID]
	return binding, ok
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

// SetDirectory replaces the working directory on the binding for chatID and
// persists the change. No-op when the binding does not exist.
func (r *Router) SetDirectory(chatID, directory string) {
	r.mutate(chatID, func(b *Binding) { b.Directory = directory })
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
