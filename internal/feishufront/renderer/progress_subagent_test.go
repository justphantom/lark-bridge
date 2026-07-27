package renderer

import (
	"strings"
	"testing"
)

// TestAddSubagentUse_RunningThenProgress verifies claude's task_started →
// task_progress lifecycle folds into one row keyed by ChildSession: identity
// (type/title) set at started stays stable when progress carries only the
// live description + cumulative usage.
func TestAddSubagentUse_RunningThenProgress(t *testing.T) {
	s := NewProgressState()
	s.AddSubagentUse(SubagentInfo{
		Status: "running", TaskType: "local_agent", Type: "general-purpose",
		Title: "调查飞书接口调用", ChildSession: "a31",
	})
	s.AddSubagentUse(SubagentInfo{
		Status: "running", TaskType: "local_agent", Type: "general-purpose",
		Description: "Reading internal/lark/ws/frame.go", ChildSession: "a31",
		DurationMs: 33203, ToolUses: 14, LastToolName: "Read",
	})
	if len(s.subagents) != 1 {
		t.Fatalf("want 1 row (folded), got %d", len(s.subagents))
	}
	row := s.subagents[0]
	if row.title != "调查飞书接口调用" {
		t.Errorf("title = %q, want stable 调查飞书接口调用", row.title)
	}
	if row.desc != "Reading internal/lark/ws/frame.go" {
		t.Errorf("desc = %q, want the live progress action", row.desc)
	}
	if row.toolUses != 14 || row.durationMs != 33203 {
		t.Errorf("usage = toolUses:%d durationMs:%d, want 14/33203", row.toolUses, row.durationMs)
	}
	if row.lastTool != "Read" {
		t.Errorf("lastTool = %q, want Read", row.lastTool)
	}
	if row.status != "running" {
		t.Errorf("status = %q, want running", row.status)
	}
}

// TestAddSubagentResult_ClosesRunningRow verifies claude's task_notification
// closes the row opened by task_started via ChildSession, applying the
// terminal preview without disturbing the cached identity/usage.
func TestAddSubagentResult_ClosesRunningRow(t *testing.T) {
	s := NewProgressState()
	s.AddSubagentUse(SubagentInfo{
		Status: "running", Type: "general-purpose",
		Title: "调查飞书接口调用", ChildSession: "a31",
		DurationMs: 33203, ToolUses: 14,
	})
	s.AddSubagentResult(SubagentInfo{
		Status: "completed", ChildSession: "a31",
		Preview: "Feishu API 接口已完全映射", OutputBytes: 2067,
	})
	if len(s.subagents) != 1 {
		t.Fatalf("want 1 row, got %d", len(s.subagents))
	}
	row := s.subagents[0]
	if row.status != "completed" {
		t.Errorf("status = %q, want completed", row.status)
	}
	if row.preview != "Feishu API 接口已完全映射" {
		t.Errorf("preview = %q", row.preview)
	}
	if row.outputBytes != 2067 {
		t.Errorf("outputBytes = %d, want 2067", row.outputBytes)
	}
	if row.title != "调查飞书接口调用" {
		t.Errorf("title = %q, want preserved", row.title)
	}
}

// TestAddSubagentResult_NoPriorAppends verifies opencode's single-event shape:
// a terminal result with no matching running row appends fresh (opencode
// collapses the whole lifecycle into one completed event).
func TestAddSubagentResult_NoPriorAppends(t *testing.T) {
	s := NewProgressState()
	s.AddSubagentResult(SubagentInfo{
		Status: "completed", TaskType: "agent", Type: "explore",
		Title: "探索代码规范", ChildSession: "ses_child",
		Preview: "调研已全部完成", OutputBytes: 10240, DurationMs: 662000,
	})
	if len(s.subagents) != 1 {
		t.Fatalf("want 1 appended row, got %d", len(s.subagents))
	}
	if s.subagents[0].status != "completed" {
		t.Errorf("status = %q, want completed", s.subagents[0].status)
	}
	if s.subagents[0].type_ != "explore" {
		t.Errorf("type = %q, want explore", s.subagents[0].type_)
	}
}

// TestRenderSubagentZone_RendersFullBlock verifies the expanded block layout:
// header line (icon + type + status + duration), bold title, capped preview,
// meta line (child id + steps + size).
func TestRenderSubagentZone_RendersFullBlock(t *testing.T) {
	s := NewProgressState()
	s.AddSubagentResult(SubagentInfo{
		Status: "completed", Type: "general-purpose",
		Title: "调查飞书接口调用", ChildSession: "a31153be0cabb040a",
		DurationMs: 71083, ToolUses: 28,
		Preview: "Feishu API 接口已完全映射", OutputBytes: 2067,
	})
	body := renderSubagentZone(s.subagents)
	if body == "" {
		t.Fatal("rendered empty body")
	}
	if !strings.Contains(body, "general-purpose") {
		t.Errorf("missing type: %s", body)
	}
	if !strings.Contains(body, "✅") {
		t.Errorf("missing completed status: %s", body)
	}
	if !strings.Contains(body, "1m") {
		t.Errorf("missing duration 1m (71083ms rounds to 1m): %s", body)
	}
	if !strings.Contains(body, "**调查飞书接口调用**") {
		t.Errorf("missing bold title: %s", body)
	}
	if !strings.Contains(body, "Feishu API 接口已完全映射") {
		t.Errorf("missing preview: %s", body)
	}
	if !strings.Contains(body, "🆔 a31153be0cab") { // truncated to 12 runes
		t.Errorf("missing truncated child id: %s", body)
	}
	if !strings.Contains(body, "28步") {
		t.Errorf("missing tool count: %s", body)
	}
	if !strings.Contains(body, "2.0KB") {
		t.Errorf("missing output size: %s", body)
	}
}

// TestRenderSubagentZone_RunningShowsDescription verifies a running row
// renders the live description (not the preview, which is empty while
// in flight).
func TestRenderSubagentZone_RunningShowsDescription(t *testing.T) {
	s := NewProgressState()
	s.AddSubagentUse(SubagentInfo{
		Status: "running", Type: "general-purpose", Title: "调查",
		Description: "Reading internal/lark/ws/frame.go", ChildSession: "a31",
		LastToolName: "Read",
	})
	body := renderSubagentZone(s.subagents)
	if !strings.Contains(body, "运行中") {
		t.Errorf("missing running status: %s", body)
	}
	if !strings.Contains(body, "Reading internal/lark/ws/frame.go") {
		t.Errorf("missing live description: %s", body)
	}
	if !strings.Contains(body, "最近 Read") {
		t.Errorf("missing last tool hint: %s", body)
	}
}

// TestRenderSubagentZone_FoldsBeyondThreshold verifies the >3 active case
// folds to a todo-style status-count summary plus the most recent detail
// (mixed folding, not pure binary collapse).
func TestRenderSubagentZone_FoldsBeyondThreshold(t *testing.T) {
	s := NewProgressState()
	for i := range 5 {
		s.AddSubagentResult(SubagentInfo{
			Status: "completed", Type: "general-purpose",
			Title: "task-" + string(rune('A'+i)), ChildSession: "c" + string(rune('A'+i)),
			DurationMs: 60000, OutputBytes: 1024,
		})
	}
	body := renderSubagentZone(s.subagents)
	// Status-count summary.
	if !strings.Contains(body, "子代理 5") {
		t.Errorf("missing count summary: %s", body)
	}
	if !strings.Contains(body, "✅5") {
		t.Errorf("missing completed count: %s", body)
	}
	// Most recent detail still visible (task-E, the 5th).
	if !strings.Contains(body, "task-E") {
		t.Errorf("missing most recent detail: %s", body)
	}
	// The first detail (task-A) must be folded into the summary, not listed.
	if strings.Contains(body, "**task-A**") {
		t.Errorf("task-A should be folded, not expanded: %s", body)
	}
}

// TestRenderSubagentZone_FailedNeverFolded verifies failed subagents always
// render as full detail blocks even when active subagents trigger folding.
// Mirrors the errored-zone philosophy: each failure's reason matters.
func TestRenderSubagentZone_FailedNeverFolded(t *testing.T) {
	s := NewProgressState()
	for i := range 5 {
		s.AddSubagentResult(SubagentInfo{Status: "completed", Type: "general", ChildSession: "c" + string(rune('A'+i)), OutputBytes: 100})
	}
	s.AddSubagentResult(SubagentInfo{
		Status: "failed", Type: "general-purpose", Title: "重构渲染层",
		ChildSession: "cF", Preview: "permission denied", OutputBytes: 50,
	})
	body := renderSubagentZone(s.subagents)
	// Failed row renders its full block (title + preview), not just a count.
	if !strings.Contains(body, "**重构渲染层**") {
		t.Errorf("failed title missing: %s", body)
	}
	if !strings.Contains(body, "permission denied") {
		t.Errorf("failed preview missing: %s", body)
	}
	if !strings.Contains(body, "失败") {
		t.Errorf("missing 失败 label: %s", body)
	}
}

// TestRenderSubagentZone_PreviewCappedTo200Runes verifies the preview is
// rune-truncated (multi-byte safe) and never explodes the card even when
// the backend inlined a multi-KB output.
func TestRenderSubagentZone_PreviewCappedTo200Runes(t *testing.T) {
	long := strings.Repeat("调", 500) // 500 multi-byte runes
	s := NewProgressState()
	s.AddSubagentResult(SubagentInfo{
		Status: "completed", Type: "explore", Title: "x", ChildSession: "c1",
		Preview: long, OutputBytes: len(long),
	})
	body := renderSubagentZone(s.subagents)
	// The card body must not carry all 500 repeats; cap is 200 runes + "…".
	if got := strings.Count(body, "调"); got > 210 {
		t.Errorf("preview not capped: %d occurrences of 调 (cap ~200+ellipsis)", got)
	}
	if !strings.Contains(body, "…") {
		t.Errorf("missing truncation ellipsis: %s", body)
	}
}

// TestRenderSubagentZone_OmittedWhenEmpty verifies an empty slice renders no
// zone at all (returns "") so the caller's zone-count check skips it.
func TestRenderSubagentZone_OmittedWhenEmpty(t *testing.T) {
	if body := renderSubagentZone(nil); body != "" {
		t.Errorf("empty zone should render as empty string, got %q", body)
	}
}

// TestSummary_CountsSubagentZone verifies the result-card digest counts
// dedicated-zone subagents (len(s.subagents)) in addition to leaf subagents
// (categoryTotals). Both contribute to the "子代理 N" segment.
func TestSummary_CountsSubagentZone(t *testing.T) {
	s := NewProgressState()
	// One leaf subagent (claude local_bash path: IsSubagent + leaf row).
	s.AddToolUse("Shell", "make test", true, "")
	s.AddToolResult("Shell", "", "ok", false, true, "")
	// Two dedicated-zone subagents (local_agent / opencode task).
	s.AddSubagentResult(SubagentInfo{Status: "completed", Type: "explore", ChildSession: "c1", OutputBytes: 100})
	s.AddSubagentResult(SubagentInfo{Status: "completed", Type: "general", ChildSession: "c2", OutputBytes: 100})
	got := s.Summary()
	// 1 leaf + 2 zone = 3 total.
	if !strings.Contains(got, "子代理 3") {
		t.Errorf("Summary = %q, want contains 子代理 3 (1 leaf + 2 zone)", got)
	}
}

// TestClone_DeepCopiesSubagents verifies Clone duplicates the subagents slice
// so a subsequent AddSubagent* mutation cannot race with a Render on the
// snapshot.
func TestClone_DeepCopiesSubagents(t *testing.T) {
	s := NewProgressState()
	s.AddSubagentUse(SubagentInfo{Status: "running", Type: "explore", ChildSession: "c1"})
	cp := s.Clone()
	if len(cp.subagents) != 1 || cp.subagents[0].childID != "c1" {
		t.Fatalf("clone did not copy subagents: %+v", cp.subagents)
	}
	// Mutate original; clone must be unaffected.
	s.subagents[0].childID = "mutated"
	if cp.subagents[0].childID == "mutated" {
		t.Fatal("clone shares subagents slice with original (not a deep copy)")
	}
}
