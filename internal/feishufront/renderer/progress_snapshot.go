package renderer

// Clone returns a deep copy of the state safe to render outside the progress
// lock. The tools and todos slices are copied (not shared) so concurrent
// AddToolUse / AddToolResult / AddTodo mutations cannot race with a Render
// running on the clone.
//
// The dispatcher renders every progress delta; Render+Marshal is the expensive
// part. Cloning under the lock and rendering on the clone afterwards keeps the
// global progressMu held only for the cheap state mutation + copy, so one
// turn's render no longer serialises another turn's update.
func (s *ProgressState) Clone() *ProgressState {
	cp := &ProgressState{
		stepCount: s.stepCount,
		tools:     make([]toolRow, len(s.tools)),
		subagents: make([]subagentRow, len(s.subagents)),
		todos:     make([]TodoItem, len(s.todos)),
		loading:   s.loading,
		gate:      s.gate,
		thinking:  s.thinking,
	}
	copy(cp.tools, s.tools)
	copy(cp.subagents, s.subagents)
	copy(cp.todos, s.todos)
	return cp
}
