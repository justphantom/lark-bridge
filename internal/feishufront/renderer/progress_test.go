package renderer

import (
	"strconv"
	"strings"
	"testing"
)

// TestNormalizeToolName_MCP shortens "mcp__<server>__<tool>" to "mcp:<tool>"
// so a 40+ char MCP tool name doesn't wrap on mobile, while keeping the mcp
// origin visible. Non-MCP names keep the first-letter capitalisation.
func TestNormalizeToolName_MCP(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"mcp long", "mcp__codebase-memory-mcp__index_repository", "mcp:index_repository"},
		{"mcp simple", "mcp__srv__do_thing", "mcp:do_thing"},
		{"read", "read", "Read"},
		{"bash", "bash", "Bash"},
		{"empty", "", "?"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeToolName(tc.in); got != tc.want {
				t.Errorf("normalizeToolName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAddToolUse_DedupCounts locks in the collapse-and-count behaviour:
// repeated identical calls (same name+desc) fold into one row whose count
// rises, instead of spawning duplicate rows.
func TestAddToolUse_DedupCounts(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("read", "/x.go", false, "")
	s.AddToolUse("read", "/x.go", false, "")
	s.AddToolUse("read", "/x.go", false, "")
	if n := len(s.tools); n != 1 {
		t.Fatalf("want 1 collapsed row, got %d", n)
	}
	if s.tools[0].count != 3 {
		t.Errorf("count = %d, want 3", s.tools[0].count)
	}
	if got := formatToolLine(s.tools[0]); !strings.Contains(got, "×3") {
		t.Errorf("formatToolLine = %q, want to contain ×3", got)
	}
}

// TestAddToolUse_DifferentDescSeparates ensures two reads of different files
// stay as distinct rows (not collapsed), each count 1, no ×N suffix.
func TestAddToolUse_DifferentDescSeparates(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("read", "/a.go", false, "")
	s.AddToolUse("read", "/b.go", false, "")
	if n := len(s.tools); n != 2 {
		t.Fatalf("want 2 rows, got %d", n)
	}
	for i, row := range s.tools {
		if row.count != 1 {
			t.Errorf("row %d count = %d, want 1", i, row.count)
		}
		if got := formatToolLine(row); strings.Contains(got, "×") {
			t.Errorf("row %d should have no ×N: %q", i, got)
		}
	}
}

// TestAddToolResult_MatchesMostRecentRunning verifies the reverse-scan match:
// with two running reads, the result closes the most recent one ("b.go").
func TestAddToolResult_MatchesMostRecentRunning(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("read", "/a.go", false, "")
	s.AddToolUse("read", "/b.go", false, "")
	s.AddToolResult("read", "", "done", false, false, "")
	if s.tools[0].status != "running" {
		t.Errorf("first row (/a.go) should stay running, got %q", s.tools[0].status)
	}
	if s.tools[1].status != "completed" {
		t.Errorf("second row (/b.go) should complete, got %q", s.tools[1].status)
	}
	if s.tools[1].output != "done" {
		t.Errorf("output = %q, want done", s.tools[1].output)
	}
}

// TestAddToolResult_NoRunningAppendsDesc: a result with no prior matching
// running row appends a fresh completed row, using the input summary (desc)
// as its description. This is the opencode path (one completed event, no use).
func TestAddToolResult_NoRunningAppendsDesc(t *testing.T) {
	s := NewProgressState()
	s.AddToolResult("read", "/opt/README.md", "file contents", false, false, "")
	if n := len(s.tools); n != 1 {
		t.Fatalf("want 1 appended row, got %d", n)
	}
	row := s.tools[0]
	if row.status != "completed" {
		t.Errorf("status = %q, want completed", row.status)
	}
	if row.desc != "/opt/README.md" {
		t.Errorf("desc = %q, want /opt/README.md", row.desc)
	}
	if row.name != "Read" {
		t.Errorf("name = %q, want Read (normalized)", row.name)
	}
}

// TestAddToolResult_PreservesExistingDesc: a plain tool result carries the
// same input summary as its ToolUse (opencode) or an empty one (claude); in
// neither case should the row's desc change. Only a terminal notification
// with a richer summary (TestAddToolResult_UpdatesDescOnNotification) updates it.
func TestAddToolResult_PreservesExistingDesc(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("bash", "make test", false, "")
	// Same desc as the use → no change.
	s.AddToolResult("bash", "make test", "PASS", false, false, "")
	if s.tools[0].desc != "make test" {
		t.Errorf("desc = %q, want make test (same desc must not change it)", s.tools[0].desc)
	}
	// Empty desc (claude tool_result carries no input summary) → no change.
	s.AddToolUse("read", "/a.go", false, "")
	s.AddToolResult("read", "", "body", false, false, "")
	if s.tools[1].desc != "/a.go" {
		t.Errorf("desc = %q, want /a.go (empty result desc must not clear it)", s.tools[1].desc)
	}
}

// TestAddToolResult_ErrorFlagsStatus: an error result marks the row error and
// renders the ❌ icon.
func TestAddToolResult_ErrorFlagsStatus(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("bash", "git push", false, "")
	s.AddToolResult("bash", "", "requires approval", true, false, "")
	if s.tools[0].status != "error" {
		t.Errorf("status = %q, want error", s.tools[0].status)
	}
	if got := formatToolLine(s.tools[0]); !strings.Contains(got, "❌") {
		t.Errorf("formatToolLine = %q, want ❌", got)
	}
}

// TestFormatToolLine_CompletedHidesOutput confirms a completed tool's output is
// not rendered — the progress card shows actions, not their output.
func TestFormatToolLine_CompletedHidesOutput(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("read", "/a.go", false, "")
	s.AddToolResult("read", "", "the full file contents", false, false, "")
	got := formatToolLine(s.tools[0])
	if strings.Contains(got, "the full file contents") {
		t.Errorf("completed output should be hidden, got %q", got)
	}
	if !strings.Contains(got, "✅") || !strings.Contains(got, "/a.go") {
		t.Errorf("completed row should still show icon+desc, got %q", got)
	}
}

// TestFormatToolLine_ErrorShowsExcerpt confirms a failed tool's output is kept
// as a short excerpt so the user can see why it failed.
func TestFormatToolLine_ErrorShowsExcerpt(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("bash", "git push", false, "")
	s.AddToolResult("bash", "", "exit 1: permission denied", true, false, "")
	got := formatToolLine(s.tools[0])
	if !strings.Contains(got, "exit 1: permission denied") {
		t.Errorf("error excerpt should be shown, got %q", got)
	}
}

// TestAddToolResult_UpdatesDescOnNotification verifies a terminal description
// (a subagent notification summary with cumulative usage) supersedes the live
// progress description the row held while running.
func TestAddToolResult_UpdatesDescOnNotification(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("Explore Agent", "Explore codebase architecture", true, "")
	// Notification closes the row with a richer terminal description.
	s.AddToolResult("Explore Agent", "Explore codebase architecture · 66步 · 107k tokens", "", false, true, "")
	if !strings.Contains(s.tools[0].desc, "66步") || !strings.Contains(s.tools[0].desc, "107k tokens") {
		t.Errorf("desc = %q, want terminal summary with cumulative usage", s.tools[0].desc)
	}
	if s.tools[0].status != "completed" {
		t.Errorf("status = %q, want completed", s.tools[0].status)
	}
}

// TestRender_RunningToolsCapped verifies that when running tools exceed
// maxRunningTools, older ones are collapsed into a "... 及 N 个运行中"
// summary and only the most recent maxRunningTools are shown.
func TestRender_RunningToolsCapped(t *testing.T) {
	s := NewProgressState()
	// Add maxRunningTools+2 distinct running tools (different desc so they
	// don't collapse). Tools are appended in order; the last ones are the
	// most recent.
	total := maxRunningTools + 2
	for i := range total {
		s.AddToolUse("Read", "/file"+strconv.Itoa(i)+".go", false, "")
	}
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(b)
	// Summary line for the collapsed older running tools.
	if !strings.Contains(body, "... 及 2 个运行中") {
		t.Errorf("expected collapse summary for 2 running tools, got: %s", body)
	}
	// The most recent maxRunningTools should be visible.
	for i := total - maxRunningTools; i < total; i++ {
		if !strings.Contains(body, "/file"+strconv.Itoa(i)+".go") {
			t.Errorf("expected /file%d.go in rendered card (most recent running), got: %s", i, body)
		}
	}
	// The oldest collapsed ones should NOT appear by their desc.
	for i := range total - maxRunningTools {
		if strings.Contains(body, "/file"+strconv.Itoa(i)+".go") {
			t.Errorf("collapsed running tool /file%d.go should not appear in card", i)
		}
	}
}

// TestRender_RunningToolsUnderCap verifies all running tools are shown when
// the count is within maxRunningTools (no collapse summary).
func TestRender_RunningToolsUnderCap(t *testing.T) {
	s := NewProgressState()
	for i := range maxRunningTools {
		s.AddToolUse("Read", "/file"+strconv.Itoa(i)+".go", false, "")
	}
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(b)
	if strings.Contains(body, "个运行中") {
		t.Errorf("should not collapse when within cap, got: %s", body)
	}
	for i := range maxRunningTools {
		if !strings.Contains(body, "/file"+strconv.Itoa(i)+".go") {
			t.Errorf("expected /file%d.go in rendered card, got: %s", i, body)
		}
	}
}

// TestProgressRender_HrBetweenSections locks the divider behaviour: when
// multiple tool zones (running / completed) are non-empty, hrs separate them;
// with only one zone, no hr is emitted. (The abort button is an action, not
// a zone, so it does not add a divider.)
func TestProgressRender_HrBetweenSections(t *testing.T) {
	// one running + one completed → 1 divider between 2 zones.
	s := NewProgressState()
	s.AddToolUse("bash", "ls", false, "")
	s.AddToolResult("read", "/a", "out", false, false, "")
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.Count(string(b), `"tag":"hr"`); got != 1 {
		t.Errorf("hr count = %d, want 1 (between 2 zones)", got)
	}

	// Single zone → no divider.
	s2 := NewProgressState()
	s2.AddToolUse("bash", "ls", false, "")
	b2, err := s2.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.Count(string(b2), `"tag":"hr"`); got != 0 {
		t.Errorf("hr count = %d, want 0 for a single zone", got)
	}
}

// TestSummary_CountsByCategory exercises the result-card digest: reads/execs
// sum each row's folded count, subagents are detected by name (claude renders
// them as "<Type> Agent"; opencode's subagent tool is "task" → "Task"), and a
// subagent's count does NOT leak into reads/execs.
func TestSummary_CountsByCategory(t *testing.T) {
	s := NewProgressState()
	// claude shape: repeated reads fold into one row with count.
	s.AddToolUse("read", "/a.go", false, "")
	s.AddToolUse("read", "/a.go", false, "")
	s.AddToolResult("read", "", "done", false, false, "") // closes most-recent /a.go
	s.AddToolUse("bash", "make test", false, "")
	s.AddToolResult("bash", "", "PASS", false, false, "")
	s.AddToolUse("Explore Agent", "explore", true, "")
	s.AddToolResult("Explore Agent", "explore · 66步", "", false, true, "")
	if got := s.Summary(); got != "📎 读取 2 · 执行 1 · 子代理 1" {
		t.Errorf("claude summary = %q", got)
	}

	// opencode shape: task subagent arrives result-only (no prior AddToolUse).
	s2 := NewProgressState()
	s2.AddToolResult("task", "研究前端", "<report>", false, true, "")
	if got := s2.Summary(); got != "📎 子代理 1" {
		t.Errorf("opencode summary = %q", got)
	}

	// No tools → empty summary (pure chat reply).
	if got := NewProgressState().Summary(); got != "" {
		t.Errorf("empty summary = %q, want empty", got)
	}
}

// TestRender_GroupedSummaryBeyondCap verifies the scheme-2 grouped summary:
// when completed tools exceed maxCompletedTools the bare "… 及 N 个已完成"
// count is replaced by "… 另完成 读取 N · 执行 N · …" so the user sees the
// category mix during long tasks (实测 023537: Read 127, 058123: MCP 32).
func TestRender_GroupedSummaryBeyondCap(t *testing.T) {
	s := NewProgressState()
	// 7 reads of distinct files (> maxCompletedTools=3) + 2 edits + 1 mcp.
	for i := range 7 {
		s.AddToolUse("read", "/f"+strconv.Itoa(i)+".go", false, "")
		s.AddToolResult("read", "", "ok", false, false, "")
	}
	s.AddToolUse("edit", "/a.go", false, "")
	s.AddToolResult("edit", "/a.go", "ok", false, false, "")
	s.AddToolUse("edit", "/b.go", false, "")
	s.AddToolResult("edit", "/b.go", "ok", false, false, "")
	s.AddToolUse("mcp__codebase-memory-mcp__search_graph", "query", false, "")
	s.AddToolResult("mcp__codebase-memory-mcp__search_graph", "query", "ok", false, false, "")
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(b)
	// Grouped summary should name each category with its count.
	if !strings.Contains(body, "另完成 读取 7") {
		t.Errorf("missing grouped 读取 7: %s", body)
	}
	if !strings.Contains(body, "编辑 2") {
		t.Errorf("missing grouped 编辑 2: %s", body)
	}
	if !strings.Contains(body, "mcp 1") {
		t.Errorf("missing grouped mcp 1: %s", body)
	}
	// The bare legacy count line must not appear alongside the grouped summary.
	if strings.Contains(body, "个已完成") {
		t.Errorf("legacy count line should be replaced by grouped summary: %s", body)
	}
	// Near-window: only the most recent maxCompletedTools detail rows show.
	// The oldest read (/f0.go) must be folded into the summary, not listed.
	if strings.Contains(body, "/f0.go") {
		t.Errorf("oldest read should be folded into summary, not listed: %s", body)
	}
}

// TestSummary_IncludesEditAndMCP locks the scheme-2 Summary() category
// expansion: Edit/Write/MCP now appear (previously only Read/Bash/sub), so a
// code-editing turn reports what was edited alongside reads/execs.
func TestSummary_IncludesEditAndMCP(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("read", "/a.go", false, "")
	s.AddToolResult("read", "", "ok", false, false, "")
	s.AddToolUse("read", "/a.go", false, "")
	s.AddToolResult("read", "", "ok", false, false, "")
	s.AddToolUse("edit", "/a.go", false, "")
	s.AddToolResult("edit", "/a.go", "ok", false, false, "")
	s.AddToolUse("mcp__codebase-memory-mcp__search_code", "pattern", false, "")
	s.AddToolResult("mcp__codebase-memory-mcp__search_code", "pattern", "ok", false, false, "")
	s.AddToolUse("write", "/new.go", false, "")
	s.AddToolResult("write", "/new.go", "ok", false, false, "")
	got := s.Summary()
	want := "📎 读取 2 · 编辑 1 · 写入 1 · mcp 1"
	if got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}

// subagent's started/progress/notification carry the same taskID but their
// descriptions drift each tick. Matching by taskID folds the whole lifecycle
// into one row (count rises, desc updates in place) instead of spawning a new
// row per progress tick.
func TestAddToolUse_SubagentFoldsByTaskID(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("Explore Agent", "Explore codebase", true, "t1")
	// Progress ticks change the description (实测 023537: 51 ticks, 50 unique descs).
	s.AddToolUse("Explore Agent", "Reading handler_prompt.go", true, "t1")
	s.AddToolUse("Explore Agent", "Reading stream_loop.go", true, "t1")
	// Notification closes by taskID.
	s.AddToolResult("Agent", "Explore codebase · 66步", "", false, true, "t1")
	if n := len(s.tools); n != 1 {
		t.Fatalf("want 1 folded row (lifecycle by taskID), got %d rows", n)
	}
	if s.tools[0].count != 3 {
		t.Errorf("count = %d, want 3 (started + 2 progress folded)", s.tools[0].count)
	}
	if s.tools[0].status != "completed" {
		t.Errorf("status = %q, want completed (notification closed by taskID)", s.tools[0].status)
	}
	if !strings.Contains(s.tools[0].desc, "66步") {
		t.Errorf("desc = %q, want terminal summary", s.tools[0].desc)
	}
}

// TestAddToolResult_ConcurrentSubagentsCloseByTaskID reproduces the concurrency
// bug (实测 159865/022265): two subagents run interleaved with drifting names
// (notification subagent_type is empty → "Agent"). The result must close the
// row whose taskID matches, NOT the most-recent same-name running row.
func TestAddToolResult_ConcurrentSubagentsCloseByTaskID(t *testing.T) {
	s := NewProgressState()
	// A (Explore) starts, then B (local_bash, name degrades to "Shell") starts.
	s.AddToolUse("Explore Agent", "Count links", true, "A")
	s.AddToolUse("Shell", "Fetch page", true, "B")
	// B finishes first — its notification must close B, leaving A running.
	s.AddToolResult("Agent", "Fetch page · done", "", false, true, "B")
	// B's row (index 1) closed, A's row (index 0) still running.
	if s.tools[0].status != "running" || s.tools[0].taskID != "A" {
		t.Errorf("A should still be running, got status=%q taskID=%q", s.tools[0].status, s.tools[0].taskID)
	}
	if s.tools[1].status != "completed" || s.tools[1].taskID != "B" {
		t.Errorf("B should be completed, got status=%q taskID=%q", s.tools[1].status, s.tools[1].taskID)
	}
	// Now A finishes.
	s.AddToolResult("Agent", "Count links · done", "", false, true, "A")
	if s.tools[0].status != "completed" {
		t.Errorf("A should be completed after its notification, got %q", s.tools[0].status)
	}
}

// TestAddToolResult_SubagentFailureFlagsError verifies a subagent that ended
// with a non-completed status (实测 159865/022265/506910: status="stopped") is
// flagged error so the row renders ❌, and the error excerpt is kept.
func TestAddToolResult_SubagentFailureFlagsError(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("Shell", "Try wget fallback", true, "F")
	// stream_loop maps status!="completed" → IsError=true; the result closes
	// by taskID and marks the row error.
	s.AddToolResult("Agent", "Try wget fallback", "exit 1: timeout", true, true, "F")
	if s.tools[0].status != "error" {
		t.Errorf("status = %q, want error for failed subagent", s.tools[0].status)
	}
	got := formatToolLine(s.tools[0])
	if !strings.Contains(got, "❌") {
		t.Errorf("failed subagent should render ❌, got %q", got)
	}
	if !strings.Contains(got, "timeout") {
		t.Errorf("error excerpt should be kept, got %q", got)
	}
}

// TestAddToolResult_SubagentFoldedThenNotificationNoPanic covers the accepted
// residual: when a subagent's running row was collapsed out by maxRunningTools,
// a later notification (carrying the same taskID) cannot retroactively reopen
// the card. It must not panic and must not drive any count negative; the
// notification lands as an orphan completed row (documented residual).
func TestAddToolResult_SubagentFoldedThenNotificationNoPanic(t *testing.T) {
	s := NewProgressState()
	// Spawn more running subagents than maxRunningTools so the oldest collapse.
	total := maxRunningTools + 2
	for i := range total {
		s.AddToolUse("Explore Agent", "task "+strconv.Itoa(i), true, "T"+strconv.Itoa(i))
	}
	// Notification for the first (oldest, collapsed) subagent.
	s.AddToolResult("Agent", "task 0 · done", "", false, true, "T0")
	// No panic; tool count is non-negative and at least the original total
	// (the orphan completed row is appended, not subtracted).
	if n := len(s.tools); n < total {
		t.Errorf("tools = %d, want >= %d (no negative collapse)", n, total)
	}
}

// TestRender_ErrorExcerptCapped pins maxToolOutputLen: a long error output is
// truncated to 50 runes so a verbose stack trace cannot crowd the error zone.
func TestRender_ErrorExcerptCapped(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("bash", "make test", false, "")
	long := strings.Repeat("x", maxToolOutputLen+20)
	s.AddToolResult("bash", "", long, true, false, "")
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(b)
	// The cap+1'th rune must not survive; "…" marks truncation.
	if strings.Contains(body, strings.Repeat("x", maxToolOutputLen+1)) {
		t.Errorf("error excerpt should be capped at %d runes: %s", maxToolOutputLen, body)
	}
	if !strings.Contains(body, "…") {
		t.Errorf("capped excerpt should end with …: %s", body)
	}
}

// TestRender_ErrorZoneExcludesCompleted verifies the four-zone separation: an
// error row lands in its own zone (after completed), and the completed zone's
// grouped summary counts only successes — the failed action must NOT inflate
// any category count.
func TestRender_ErrorZoneExcludesCompleted(t *testing.T) {
	s := NewProgressState()
	// 1 success + 1 failure of the same name (Read).
	s.AddToolUse("read", "/ok.go", false, "")
	s.AddToolResult("read", "", "body", false, false, "")
	s.AddToolUse("read", "/fail.go", false, "")
	s.AddToolResult("read", "", "permission denied", true, false, "")
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(b)
	// Only one success → no grouped summary at all (completed count 1 < cap).
	// The error's excerpt and the ❌ icon must appear in a separate zone.
	if !strings.Contains(body, "permission denied") {
		t.Errorf("error excerpt missing: %s", body)
	}
	if !strings.Contains(body, "❌") {
		t.Errorf("error icon missing: %s", body)
	}
	// The successful row keeps ✅; both icons coexist (separate zones).
	if !strings.Contains(body, "✅") {
		t.Errorf("success icon missing: %s", body)
	}
	// Error must come after the completed row in the rendered byte stream —
	// the completed zone precedes the error zone by spec.
	completedIdx := strings.Index(body, "/ok.go")
	errorIdx := strings.Index(body, "/fail.go")
	if completedIdx < 0 || errorIdx < 0 {
		t.Fatalf("both rows should be present: %s", body)
	}
	if errorIdx < completedIdx {
		t.Errorf("error zone should follow completed zone: errorIdx=%d completedIdx=%d", errorIdx, completedIdx)
	}
}

// TestRender_ThreeZoneOrder locks the spec's zone order — executing →
// completed → error — by checking hr dividers appear between adjacent
// non-empty zones. With all three content zones populated the card carries
// exactly 2 hrs.
func TestRender_ThreeZoneOrder(t *testing.T) {
	s := NewProgressState()
	// Zone 1: executing (one still running).
	s.AddToolUse("read", "/running.go", false, "")
	// Zone 2: completed.
	s.AddToolUse("bash", "make build", false, "")
	s.AddToolResult("bash", "", "ok", false, false, "")
	// Zone 3: error.
	s.AddToolUse("bash", "git push", false, "")
	s.AddToolResult("bash", "", "denied", true, false, "")
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(b)
	// Three content zones → 2 dividers between them. (The abort button is an
	// action, not a zone.)
	if got := strings.Count(body, `"tag":"hr"`); got != 2 {
		t.Errorf("hr count = %d, want 2 (between 3 zones): %s", got, body)
	}
	// Order check by byte index: running < completed < error.
	idx := func(sub string) int { return strings.Index(body, sub) }
	for _, sub := range []string{"/running.go", "make build", "denied"} {
		if idx(sub) < 0 {
			t.Fatalf("missing %q in render: %s", sub, body)
		}
	}
	if idx("/running.go") >= idx("make build") || idx("make build") >= idx("denied") {
		t.Errorf("zone order wrong (want running<completed<error): %s", body)
	}
}

// TestRender_NoAbortButton verifies the progress card never carries an abort
// button — the button was removed; users stop via /session-abort text command.
func TestRender_NoAbortButton(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("bash", "ls", false, "") // running
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(b), `"kind":"abort"`) {
		t.Errorf("abort button should not appear in progress card: %s", b)
	}
}

// TestRender_TitleCountsCompletedOnly locks the title tool count: it tracks
// settled actions (completed + errored), not the volatile running set which
// has its own zone.
func TestRender_TitleCountsCompletedOnly(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("read", "/a.go", false, "")            // running
	s.AddToolUse("bash", "make", false, "")             // running
	s.AddToolResult("bash", "", "ok", false, false, "") // 1 completed
	s.AddToolUse("bash", "fail", false, "")
	s.AddToolResult("bash", "", "err", true, false, "") // 1 errored
	b, err := s.Render(hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	title := string(b)
	if !strings.Contains(title, "已完成 2") {
		t.Errorf("title should count 2 settled (1 completed + 1 errored): %s", title)
	}
	if strings.Contains(title, "个工具") {
		t.Errorf("legacy '个工具' wording should be gone: %s", title)
	}
}
