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
		{"multi_edit", "edit"}, // v2.0.0 tool; normalises to Multi_edit → edit
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
	}
	for _, c := range cases {
		name := normalizeToolName(c.raw)
		got := toolCategory(toolRow{name: name})
		if got != c.want {
			t.Errorf("raw=%q normalised=%q: category=%q want %q", c.raw, name, got, c.want)
		}
	}
}

// TestToolCategory_SubagentShellPrecedence verifies a subagent row named "Shell"
// (claude renders some subagents as "Shell") counts as "sub", NOT "exec": the
// isSubagent check at the top of toolCategory wins over the Shell→exec case.
func TestToolCategory_SubagentShellPrecedence(t *testing.T) {
	got := toolCategory(toolRow{name: "Shell", isSubagent: true})
	if got != "sub" {
		t.Errorf("subagent Shell category = %q, want \"sub\"", got)
	}
}
