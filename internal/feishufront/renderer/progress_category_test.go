package renderer

import "testing"

// TestToolCategory_NormalisedNames covers the full leaf-tool path: the raw
// tool name an agent emits is normalised (first letter upper-cased) on entry to
// a toolRow, then toolCategory classifies it. miniagent's lowercase
// read/write/edit/shell must land in read/write/edit/exec respectively — the
// Shell case is the non-trivial one (toolCategory historically matched only
// "Bash").
func TestToolCategory_NormalisedNames(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		// miniagent (lowercase) — normalised then classified.
		{"read", "read"},
		{"write", "write"},
		{"edit", "edit"},
		// miniagent v3.2.0 merged multi_edit into edit's `edits` array; the
		// standalone tool is gone, so a leftover Multi_edit row now falls into
		// the unclassified bucket (omitted from the grouped summary). This
		// pins the v3.3 follow-up: the renderer no longer special-cases it.
		{"multi_edit", ""},
		{"shell", "exec"},
		// claude/opencode/omp (already PascalCase).
		{"Read", "read"},
		{"Grep", "read"},
		{"Glob", "read"},
		{"Bash", "exec"},
		{"Edit", "edit"},
		{"Write", "write"},
		// Unclassified → "" (omitted from the grouped summary).
		{"WebSearch", ""},
		{"Cron", ""},
		// claude's MultiEdit (distinct PascalCase name) was never classified.
		{"MultiEdit", ""},
	}
	for _, c := range cases {
		name := normalizeToolName(c.raw)
		got := toolCategory(toolRow{name: name})
		if got != c.want {
			t.Errorf("raw=%q normalised=%q: category=%q want %q", c.raw, name, got, c.want)
		}
	}
}
