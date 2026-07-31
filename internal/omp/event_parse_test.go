//go:build linux || darwin

package omp

import (
	"testing"
)

// parseOne parses line and returns the single Event, failing the test if the
// line is ignored or errors.
func parseOne(t *testing.T, line string) Event {
	t.Helper()
	ev, ok, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parseEvent(%q): %v", line, err)
	}
	if !ok {
		t.Fatalf("parseEvent(%q): ignored (ok=false)", line)
	}
	return ev
}

// TestParseEvent_EmptyAndGarbage verifies resilience: empty lines and
// non-JSON (the minified-JS debris of an uncaught exception, §A.2) are
// rejected, not panicked on.
func TestParseEvent_EmptyAndGarbage(t *testing.T) {
	if _, ok, err := parseEvent(""); ok || err != nil {
		t.Errorf(`"" → ok=%v err=%v, want ok=false err=nil`, ok, err)
	}
	if _, ok, err := parseEvent("   "); ok || err != nil {
		t.Errorf(`"   " → ok=%v err=%v, want ok=false err=nil`, ok, err)
	}
	if _, ok, err := parseEvent("not json at all"); ok || err == nil {
		t.Errorf(`garbage → ok=%v err=%v, want ok=false err!=nil`, ok, err)
	}
}

// TestParseEvent_SessionHeader verifies the first-line `session` header
// yields EventSession with the id extracted (§A.1).
func TestParseEvent_SessionHeader(t *testing.T) {
	ev := parseOne(t, `{"type":"session","version":3,"id":"019facba-e4f7-7000-8941-00b1691dc91a","timestamp":"2026-07-29T07:15:57.303Z","cwd":"/home/user/ZCodeProject/lark-bridge"}`)
	if ev.Type != EventSession {
		t.Errorf("Type = %q, want %q", ev.Type, EventSession)
	}
	if ev.SessionID != "019facba-e4f7-7000-8941-00b1691dc91a" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
}

// TestParseEvent_AgentStartEnd verifies agent lifecycle mapping.
func TestParseEvent_AgentStartEnd(t *testing.T) {
	if ev := parseOne(t, `{"type":"agent_start"}`); ev.Type != EventAgentStart {
		t.Errorf("agent_start → Type = %q", ev.Type)
	}
	if ev := parseOne(t, `{"type":"agent_end","messages":[],"isTerminal":true}`); ev.Type != EventAgentEnd {
		t.Errorf("agent_end → Type = %q", ev.Type)
	}
}

// TestParseEvent_MessageUpdateText verifies text_delta routes to
// EventMessageUpdate carrying the delta (§6.3).
func TestParseEvent_MessageUpdateText(t *testing.T) {
	ev := parseOne(t, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hello"}}`)
	if ev.Type != EventMessageUpdate {
		t.Errorf("Type = %q, want %q", ev.Type, EventMessageUpdate)
	}
	if ev.Text != "hello" {
		t.Errorf("Text = %q, want hello", ev.Text)
	}
}

// TestParseEvent_MessageUpdateThinkingEnd verifies thinking_end routes to
// EventThinking carrying the full block content. (thinking_delta is IGNORED —
// see TestParseEvent_MessageUpdateTextEndIgnored for the doubling rationale.)
func TestParseEvent_MessageUpdateThinkingEnd(t *testing.T) {
	ev := parseOne(t, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_end","contentIndex":0,"content":"the full trace"}}`)
	if ev.Type != EventThinking {
		t.Errorf("Type = %q, want %q", ev.Type, EventThinking)
	}
	if ev.Text != "the full trace" {
		t.Errorf("Text = %q, want the full block content", ev.Text)
	}
}

// TestParseEvent_MessageUpdateTextEndNowEvent verifies text_end now returns
// EventTextEnd (used as fallback by the bridge when no text_delta was
// received, preventing empty assistant replies). thinking_delta is still
// ignored (Replace=true would clobber the zone with each partial).
func TestParseEvent_MessageUpdateTextEndNowEvent(t *testing.T) {
	line := `{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0,"content":"whole block"}}`
	ev, ok, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parseEvent(text_end) → err=%v, want nil", err)
	}
	if !ok {
		t.Fatal("parseEvent(text_end) → ok=false, want ok=true (EventTextEnd)")
	}
	if ev.Type != EventTextEnd {
		t.Errorf("parseEvent(text_end) → Type=%q, want %q", ev.Type, EventTextEnd)
	}
	if ev.Text != "whole block" {
		t.Errorf("parseEvent(text_end) → Text=%q, want %q", ev.Text, "whole block")
	}

	// thinking_delta still ignored.
	line2 := `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"partial"}}`
	if _, ok2, err2 := parseEvent(line2); err2 != nil || ok2 {
		t.Errorf("parseEvent(thinking_delta) → ok=%v err=%v, want ok=false err=nil", ok2, err2)
	}
}

// TestParseEvent_MessageUpdateToolcallIgnored verifies toolcall_* deltas are
// dropped (the authoritative source is tool_execution_*, §6.3).
func TestParseEvent_MessageUpdateToolcallIgnored(t *testing.T) {
	for _, line := range []string{
		`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_delta","contentIndex":0,"delta":"{}"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_end","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"done","reason":"stop"}}`,
	} {
		if _, ok, err := parseEvent(line); err != nil || ok {
			t.Errorf("parseEvent(%q) → ok=%v err=%v, want ok=false err=nil", line, ok, err)
		}
	}
}

// TestParseEvent_MessageStartIgnored verifies message_start is ignored
// (usage is 0 here and toolCall content is redundant, §6.3).
func TestParseEvent_MessageStartIgnored(t *testing.T) {
	line := `{"type":"message_start","message":{"role":"assistant","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"total":0}}}}`
	if _, ok, err := parseEvent(line); err != nil || ok {
		t.Errorf("message_start → ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// TestParseEvent_MessageEndAssistantUsage verifies usage is extracted from a
// role=assistant message_end in camelCase (§A.6).
func TestParseEvent_MessageEndAssistantUsage(t *testing.T) {
	ev := parseOne(t, `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input":21223,"output":44,"cacheRead":100,"cacheWrite":200,"totalTokens":21567,"cost":{"input":0.01,"output":0.02,"cacheRead":0.03,"cacheWrite":0.04,"total":0.10}},"stopReason":"stop"}}`)
	if ev.Type != EventMessageEnd {
		t.Errorf("Type = %q, want %q", ev.Type, EventMessageEnd)
	}
	if ev.Role != "assistant" {
		t.Errorf("Role = %q, want assistant", ev.Role)
	}
	if ev.InputTokens != 21223 {
		t.Errorf("InputTokens = %d, want 21223", ev.InputTokens)
	}
	if ev.OutputTokens != 44 {
		t.Errorf("OutputTokens = %d, want 44", ev.OutputTokens)
	}
	if ev.CacheRead != 100 {
		t.Errorf("CacheRead = %d, want 100", ev.CacheRead)
	}
	if ev.CacheWrite != 200 {
		t.Errorf("CacheWrite = %d, want 200", ev.CacheWrite)
	}
	if ev.CostUSD != 0.10 {
		t.Errorf("CostUSD = %v, want 0.10", ev.CostUSD)
	}
}

// TestParseEvent_MessageEndToolResultNoUsage verifies role=toolResult
// message_end yields EventMessageEnd with Role set but zero usage (§6.3).
func TestParseEvent_MessageEndToolResultNoUsage(t *testing.T) {
	ev := parseOne(t, `{"type":"message_end","message":{"role":"toolResult","toolCallId":"call_x","content":[{"type":"text","text":"ok"}]}}`)
	if ev.Type != EventMessageEnd {
		t.Errorf("Type = %q, want %q", ev.Type, EventMessageEnd)
	}
	if ev.Role != "toolResult" {
		t.Errorf("Role = %q, want toolResult", ev.Role)
	}
	if ev.InputTokens != 0 || ev.OutputTokens != 0 {
		t.Errorf("usage should be zero for toolResult, got in=%d out=%d", ev.InputTokens, ev.OutputTokens)
	}
}

// TestParseEvent_MessageEndStopReasonError verifies a model error
// (stopReason:error + errorMessage) is surfaced as a terminal EventError
// (§10.10), carrying the upstream message.
func TestParseEvent_MessageEndStopReasonError(t *testing.T) {
	ev := parseOne(t, `{"type":"message_end","message":{"role":"assistant","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"total":0}},"stopReason":"error","errorStatus":403,"errorMessage":"403 forbidden"}}`)
	if ev.Type != EventError {
		t.Errorf("Type = %q, want %q", ev.Type, EventError)
	}
	if !ev.IsError {
		t.Errorf("IsError = false, want true")
	}
	if ev.Text != "403 forbidden" {
		t.Errorf("Text = %q, want 403 forbidden", ev.Text)
	}
	if ev.ErrorMessage != "403 forbidden" {
		t.Errorf("ErrorMessage = %q", ev.ErrorMessage)
	}
}

// TestParseEvent_TurnEndForwarded verifies turn_end is now forwarded as
// EventTurnEnd (it carries the round's assistant message, used by the bridge
// as a fallback reply source) rather than ignored. turn_start still yields
// EventTurnStart — see TestParseEvent_TurnStartBoundary.
func TestParseEvent_TurnEndForwarded(t *testing.T) {
	ev := parseOne(t, `{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	if ev.Type != EventTurnEnd {
		t.Errorf("Type = %q, want %q", ev.Type, EventTurnEnd)
	}
}

// TestParseEvent_TurnStartBoundary verifies turn_start yields EventTurnStart.
// This is the per-round boundary the bridge resets the text accumulator on
// (verified against agnes-2.0-flash: without it, a tool-call turn's round-1
// inline-thinking preamble leaks into the reply).
func TestParseEvent_TurnStartBoundary(t *testing.T) {
	ev := parseOne(t, `{"type":"turn_start"}`)
	if ev.Type != EventTurnStart {
		t.Errorf("Type = %q, want %q", ev.Type, EventTurnStart)
	}
}

// TestParseEvent_ToolExecutionStart verifies intent wins over args for the
// tool input summary (§6.4).
func TestParseEvent_ToolExecutionStart(t *testing.T) {
	ev := parseOne(t, `{"type":"tool_execution_start","toolCallId":"call_1","toolName":"read","args":{"path":"README.md"},"intent":"Read the first 3 lines of README.md"}`)
	if ev.Type != EventToolStart {
		t.Errorf("Type = %q, want %q", ev.Type, EventToolStart)
	}
	if ev.ToolName != "read" {
		t.Errorf("ToolName = %q", ev.ToolName)
	}
	if ev.ToolInput != "Read the first 3 lines of README.md" {
		t.Errorf("ToolInput = %q, want intent", ev.ToolInput)
	}
}

// TestParseEvent_ToolExecutionStartArgsFallback verifies args JSON is used
// when intent is absent (§6.4).
func TestParseEvent_ToolExecutionStartArgsFallback(t *testing.T) {
	ev := parseOne(t, `{"type":"tool_execution_start","toolCallId":"call_2","toolName":"bash","args":{"command":"ls -la"}}`)
	if ev.Type != EventToolStart {
		t.Errorf("Type = %q, want %q", ev.Type, EventToolStart)
	}
	// args JSON compacted.
	if ev.ToolInput != `{"command":"ls -la"}` {
		t.Errorf("ToolInput = %q, want compacted args JSON", ev.ToolInput)
	}
}

// TestParseEvent_ToolExecutionEnd verifies the result.content[].text is
// extracted and isError propagated (§A.6).
func TestParseEvent_ToolExecutionEnd(t *testing.T) {
	ev := parseOne(t, `{"type":"tool_execution_end","toolCallId":"call_1","toolName":"read","result":{"content":[{"type":"text","text":"1:# lark-bridge\n2:\n3:bridge"}]}}`)
	if ev.Type != EventToolEnd {
		t.Errorf("Type = %q, want %q", ev.Type, EventToolEnd)
	}
	if ev.ToolName != "read" {
		t.Errorf("ToolName = %q", ev.ToolName)
	}
	if ev.ToolOutput != "1:# lark-bridge\n2:\n3:bridge" {
		t.Errorf("ToolOutput = %q", ev.ToolOutput)
	}
	if ev.IsToolError {
		t.Errorf("IsToolError = true, want false")
	}
}

// TestParseEvent_ToolExecutionEndIsError verifies the isError flag is surfaced.
func TestParseEvent_ToolExecutionEndIsError(t *testing.T) {
	ev := parseOne(t, `{"type":"tool_execution_end","toolCallId":"call_3","toolName":"bash","result":{"content":[{"type":"text","text":"boom"}]},"isError":true}`)
	if !ev.IsToolError {
		t.Errorf("IsToolError = false, want true")
	}
}

// TestParseEvent_AutoRetry verifies the retry attempt is surfaced so the
// bridge can emit a progress banner (§A.3).
func TestParseEvent_AutoRetry(t *testing.T) {
	ev := parseOne(t, `{"type":"auto_retry_start","attempt":1,"maxAttempts":10,"delayMs":391.0,"errorMessage":"stream timed out","errorId":397312}`)
	if ev.Type != EventAutoRetry {
		t.Errorf("Type = %q, want %q", ev.Type, EventAutoRetry)
	}
	if ev.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", ev.Attempt)
	}
}

// TestParseEvent_Notice verifies the notice event forwards its level +
// human-readable message body (the body shares the "message" JSON key with
// the assistant-message envelope but is a bare string for notices).
func TestParseEvent_Notice(t *testing.T) {
	ev := parseOne(t, `{"type":"notice","level":"info","message":"xd://: mounted mcp__codegraph_explore","source":"xdev"}`)
	if ev.Type != EventNotice {
		t.Errorf("Type = %q, want %q", ev.Type, EventNotice)
	}
	if ev.NoticeLevel != "info" {
		t.Errorf("NoticeLevel = %q, want info", ev.NoticeLevel)
	}
	if ev.NoticeMessage != "xd://: mounted mcp__codegraph_explore" {
		t.Errorf("NoticeMessage = %q", ev.NoticeMessage)
	}
}

// TestParseEvent_NoticeLevelDefaultsInfo verifies a notice without an explicit
// level still defaults to "info" (never empty) so the renderer always has a
// valid level to style against.
func TestParseEvent_NoticeLevelDefaultsInfo(t *testing.T) {
	ev := parseOne(t, `{"type":"notice","message":"bare notice"}`)
	if ev.NoticeLevel != "info" {
		t.Errorf("NoticeLevel = %q, want info (default)", ev.NoticeLevel)
	}
	if ev.NoticeMessage != "bare notice" {
		t.Errorf("NoticeMessage = %q, want bare notice", ev.NoticeMessage)
	}
}

// TestParseEvent_UnknownForwarded verifies an unrecognised type is forwarded
// verbatim so a schema change is observable (forward-compat).
func TestParseEvent_UnknownForwarded(t *testing.T) {
	ev := parseOne(t, `{"type":"some_future_event","payload":42}`)
	if ev.Type != "some_future_event" {
		t.Errorf("Type = %q, want forwarded verbatim", ev.Type)
	}
}

// TestParseEvent_FullSuccessFlow drives a representative slice of the §A.6
// stream through the parser end-to-end to confirm the event sequence the
// bridge's streamRun expects is produced.
func TestParseEvent_FullSuccessFlow(t *testing.T) {
	lines := []string{
		`{"type":"session","version":3,"id":"019fad2b-564c-7000-9570-fe6592a468f5","cwd":"/tmp"}`,
		`{"type":"agent_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Reading "}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"README"}}`,
		`{"type":"tool_execution_start","toolCallId":"call_x","toolName":"read","args":{"path":"README.md"},"intent":"Read README"}`,
		`{"type":"tool_execution_end","toolCallId":"call_x","toolName":"read","result":{"content":[{"type":"text","text":"# title"}]}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[],"usage":{"input":21223,"output":44,"cacheRead":0,"cacheWrite":0,"totalTokens":21267,"cost":{"total":0.1}},"stopReason":"stop"}}`,
		`{"type":"agent_end","messages":[],"isTerminal":true}`,
	}
	var (
		gotSession, gotText, gotToolStart, gotToolEnd, gotUsage, gotTerminal bool
		accInput, accOutput                                                  int
	)
	for _, line := range lines {
		ev, ok, err := parseEvent(line)
		if err != nil {
			t.Fatalf("parseEvent(%q): %v", line, err)
		}
		if !ok {
			continue
		}
		switch ev.Type {
		case EventSession:
			gotSession = ev.SessionID == "019fad2b-564c-7000-9570-fe6592a468f5"
		case EventMessageUpdate:
			gotText = ev.Text == "Reading " || ev.Text == "README"
		case EventToolStart:
			gotToolStart = ev.ToolName == "read"
		case EventToolEnd:
			gotToolEnd = ev.ToolOutput == "# title"
		case EventMessageEnd:
			if ev.Role == "assistant" {
				accInput += ev.InputTokens
				accOutput += ev.OutputTokens
				gotUsage = true
			}
		case EventAgentEnd:
			gotTerminal = true
		}
	}
	if !gotSession {
		t.Error("did not capture session header")
	}
	if !gotText {
		t.Error("did not capture a text delta")
	}
	if !gotToolStart || !gotToolEnd {
		t.Error("did not capture tool start/end")
	}
	if !gotUsage || accInput != 21223 || accOutput != 44 {
		t.Errorf("usage accumulation wrong: got=%v in=%d out=%d", gotUsage, accInput, accOutput)
	}
	if !gotTerminal {
		t.Error("did not capture terminal agent_end")
	}
}

// TestParseEvent_SessionTitleCwd verifies the session header forwards title +
// cwd alongside the id (previously only the id was consumed).
func TestParseEvent_SessionTitleCwd(t *testing.T) {
	ev := parseOne(t, `{"type":"session","id":"s1","title":"统计代码行数","cwd":"/home/user/ZCodeProject/lark-bridge"}`)
	if ev.Type != EventSession {
		t.Fatalf("Type = %q, want %q", ev.Type, EventSession)
	}
	if ev.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", ev.SessionID)
	}
	if ev.SessionTitle != "统计代码行数" {
		t.Errorf("SessionTitle = %q", ev.SessionTitle)
	}
	if ev.SessionCwd != "/home/user/ZCodeProject/lark-bridge" {
		t.Errorf("SessionCwd = %q", ev.SessionCwd)
	}
}

// TestParseEvent_ThinkingLevelChanged verifies the resolved thinking level is
// forwarded (via Text) so the bridge can surface the level the model actually
// uses, distinct from the configured/auto value.
func TestParseEvent_ThinkingLevelChanged(t *testing.T) {
	ev := parseOne(t, `{"type":"thinking_level_changed","thinkingLevel":"high","configured":"auto","resolved":"high"}`)
	if ev.Type != EventThinkingLevelChanged {
		t.Fatalf("Type = %q, want %q", ev.Type, EventThinkingLevelChanged)
	}
	if ev.Text != "high" {
		t.Errorf("Text (resolved) = %q, want high", ev.Text)
	}
}

// TestParseEvent_TodoReminderAndTTSR verify the in-run reminder events are
// forwarded as named types (not "unknown") so the unknown-event metric stops
// ticking on them, while the bridge still treats them as no-op diagnostics.
func TestParseEvent_TodoReminderAndTTSR(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"todo_reminder", `{"type":"todo_reminder","todos":[],"attempt":1}`, EventTodoReminder},
		{"ttsr_triggered", `{"type":"ttsr_triggered"}`, EventTTSRTriggered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := parseOne(t, tc.line)
			if ev.Type != tc.want {
				t.Errorf("Type = %q, want %q", ev.Type, tc.want)
			}
		})
	}
}

// TestParseEvent_MessageEndErrorCodes verifies errorStatus/errorId are captured
// on a stopReason=error message_end so the bridge can append the code (e.g.
// [429/135168]) to the surfaced error text.
func TestParseEvent_MessageEndErrorCodes(t *testing.T) {
	ev, ok, err := parseEvent(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorStatus":429,"errorId":135168,"errorMessage":"429 限流","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"total":0}}}}`)
	if err != nil || !ok {
		t.Fatalf("parseEvent: ok=%v err=%v", ok, err)
	}
	if ev.Type != EventError {
		t.Errorf("Type = %q, want %q", ev.Type, EventError)
	}
	if ev.ErrorStatus != 429 {
		t.Errorf("ErrorStatus = %d, want 429", ev.ErrorStatus)
	}
	if ev.ErrorID != 135168 {
		t.Errorf("ErrorID = %d, want 135168", ev.ErrorID)
	}
	if ev.Text != "429 限流" {
		t.Errorf("Text = %q, want 429 限流", ev.Text)
	}
}

// TestParseEvent_MessageEndCustomRole verifies a role=custom system nudge
// (e.g. mid-run-todo-nudge) surfaces as EventMessageEnd with Role=custom and
// the customType in Text, never routed to the error path even if it carried an
// incidental stopReason.
func TestParseEvent_MessageEndCustomRole(t *testing.T) {
	ev, ok, err := parseEvent(`{"type":"message_end","message":{"role":"custom","customType":"mid-run-todo-nudge","content":"<system-reminder>10 todos open</system-reminder>","stopReason":"stop"}}`)
	if err != nil || !ok {
		t.Fatalf("parseEvent: ok=%v err=%v", ok, err)
	}
	if ev.Type != EventMessageEnd {
		t.Errorf("Type = %q, want %q (not EventError)", ev.Type, EventMessageEnd)
	}
	if ev.Role != "custom" {
		t.Errorf("Role = %q, want custom", ev.Role)
	}
	if ev.Text != "mid-run-todo-nudge" {
		t.Errorf("Text = %q, want mid-run-todo-nudge", ev.Text)
	}
}

// TestParseEvent_ToolExecutionStartCarriesArgs verifies the raw args JSON is
// stashed on the start event (the end event carries no args), keyed by
// toolCallId, so the bridge can drive todowrite/task special handling at end.
func TestParseEvent_ToolExecutionStartCarriesArgs(t *testing.T) {
	ev := parseOne(t, `{"type":"tool_execution_start","toolCallId":"call_1","toolName":"task","args":{"agent":"scout","task":"探索","i":"explore"},"intent":"Explore repo"}`)
	if ev.ToolCallID != "call_1" {
		t.Errorf("ToolCallID = %q, want call_1", ev.ToolCallID)
	}
	if ev.ToolInput != "Explore repo" {
		t.Errorf("ToolInput = %q, want intent Explore repo", ev.ToolInput)
	}
	if ev.ToolArgs != `{"agent":"scout","task":"探索","i":"explore"}` {
		t.Errorf("ToolArgs = %q", ev.ToolArgs)
	}
}

// TestParseEvent_ToolExecutionEndCarriesCallID verifies the end event carries
// the toolCallId (for bridge start→end joining) even though it has no args.
func TestParseEvent_ToolExecutionEndCarriesCallID(t *testing.T) {
	ev := parseOne(t, `{"type":"tool_execution_end","toolCallId":"call_1","toolName":"read","result":{"content":[{"type":"text","text":"# title"}]},"isError":false}`)
	if ev.ToolCallID != "call_1" {
		t.Errorf("ToolCallID = %q, want call_1", ev.ToolCallID)
	}
	if ev.ToolArgs != "" {
		t.Errorf("ToolArgs = %q, want empty (end carries no args)", ev.ToolArgs)
	}
	if ev.ToolOutput != "# title" {
		t.Errorf("ToolOutput = %q, want # title", ev.ToolOutput)
	}
}
