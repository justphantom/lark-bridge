package feishufront

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

func TestParseBackendCommand(t *testing.T) {
	cases := []struct {
		in   string
		cmd  string
		rest string
	}{
		{"/backend list", "/backend", "list"},
		{"/backend use claude-1", "/backend", "use claude-1"},
		{"/backend", "/backend", ""},
		{"hello", "", ""},
		{"/model x", "", ""},
		// "/backendfoo" must NOT match — only the complete "/backend" token does.
		{"/backendfoo list", "", ""},
		{"/backendlist", "", ""},
	}
	for _, c := range cases {
		gotCmd, gotRest := parseBackendCommand(c.in)
		if gotCmd != c.cmd || gotRest != c.rest {
			t.Errorf("parseBackendCommand(%q) = (%q,%q), want (%q,%q)", c.in, gotCmd, gotRest, c.cmd, c.rest)
		}
	}
}

func TestRequestIDFromValue(t *testing.T) {
	if got := requestIDFromValue(map[string]any{"requestID": "r1"}); got != "r1" {
		t.Errorf("got %q, want r1", got)
	}
	if got := requestIDFromValue(map[string]any{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestDispatchIncoming_NonTextRejected verifies an image/file/post message is
// answered with a notice card and never creates a turn (which would forward
// the raw content to the backend as a prompt).
func TestDispatchIncoming_NonTextRejected(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)

	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_img",
		MessageID: "om_img",
		ChatID:    "oc_chat",
		MsgType:   "image",
		Content:   `{"image_key":"img_v3_x"}`,
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 notice card sent, got %d", len(sink.sends))
	}
	if _, ok := d.turns.Get("om_img"); ok {
		t.Error("image message must not start a turn (would forward raw content)")
	}
}

// TestDispatchIncoming_TextStillWorks confirms the text path is unaffected by
// the non-text guard.
func TestDispatchIncoming_TextStillWorks(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)

	// router == nil → text path reaches the "路由未就绪" notice rather than
	// the non-text rejection. That proves the message passed the MsgType guard.
	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_text",
		MessageID: "om_text",
		ChatID:    "oc_chat",
		MsgType:   "text",
		Content:   `{"text":"hello"}`,
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 send on the text path, got %d", len(sink.sends))
	}
}

func TestDispatcherDedup(t *testing.T) {
	d := NewDispatcher(nil, NewBackendRegistry(), NewTurnManager(), nil)
	if !d.eventIDs.Add("e1") {
		t.Fatal("first Add should be true")
	}
	if d.eventIDs.Add("e1") {
		t.Fatal("second Add of same id should be false")
	}
}

// TestDispatcherDedupDelete verifies Delete re-arms an id so a redelivery
// after a failed first attempt is reprocessed instead of dropped. This pins
// the N3 fix: DispatchIncoming clears the marker on the pre-turn error paths.
func TestDispatcherDedupDelete(t *testing.T) {
	d := NewDispatcher(nil, NewBackendRegistry(), NewTurnManager(), nil)
	if !d.eventIDs.Add("e1") {
		t.Fatal("first Add should be true")
	}
	d.eventIDs.Delete("e1")
	if !d.eventIDs.Add("e1") {
		t.Fatal("Add after Delete should be true (redelivery reprocessed)")
	}
	// Empty id is a no-op for both Add (returns true) and Delete.
	d.eventIDs.Delete("")
}

// TestDispatcherUnknownControl asserts an unknown control type yields an
// error without touching the (nil) bot — the switch default returns before
// any bot call.
func TestDispatcherUnknownControl(t *testing.T) {
	d := NewDispatcher(nil, NewBackendRegistry(), NewTurnManager(), nil)
	err := d.DispatchControl(nil, RoutedControl{BackendID: "b", Control: &protocol.Control{Type: "bogus"}})
	if err == nil {
		t.Fatal("expected error for unknown control type")
	}
}

// TestDispatcherRememberAction verifies requestID idempotency for card
// actions.
func TestDispatcherRememberAction(t *testing.T) {
	d := NewDispatcher(nil, NewBackendRegistry(), NewTurnManager(), nil)
	if !d.actionIDs.Add("r1") {
		t.Fatal("first Add should be true")
	}
	if d.actionIDs.Add("r1") {
		t.Fatal("second Add of same id should be false")
	}
}

// TestDedupSetMaxEntries verifies the LRU cap evicts the oldest entry once
// capacity is reached, so a high-volume burst cannot grow eventIDs unbounded.
func TestDedupSetMaxEntries(t *testing.T) {
	s := newDedupSet(time.Hour, 3) // long TTL so expiry never triggers here
	if !s.Add("a") {
		t.Fatal("first Add a should be true")
	}
	if !s.Add("b") {
		t.Fatal("first Add b should be true")
	}
	if !s.Add("c") {
		t.Fatal("first Add c should be true")
	}
	// At capacity; adding d evicts a (oldest).
	if !s.Add("d") {
		t.Fatal("Add d at capacity should evict oldest and return true")
	}
	// a was evicted; re-adding it succeeds. That re-add in turn evicts b
	// (now the oldest), so c and d survive.
	if !s.Add("a") {
		t.Fatal("Add a after eviction should be true (a was evicted)")
	}
	if s.Add("c") {
		t.Error("Add c should be false (still present)")
	}
	if s.Add("d") {
		t.Error("Add d should be false (still present)")
	}
}

// TestDedupSetMaxEntriesZero confirms maxEntries<=0 disables the cap so the
// existing TTL-only behavior is preserved for actionIDs/terminals.
func TestDedupSetMaxEntriesZero(t *testing.T) {
	s := newDedupSet(time.Hour, 0)
	for i := range 1500 {
		if !s.Add("id" + itoa(i)) {
			t.Fatalf("Add id%d should be true (unbounded)", i)
		}
	}
}

// TestDispatcherIsStale pins the stale-check policy: only a create_time past
// the window counts; missing/zero and future timestamps pass through.
func TestDispatcherIsStale(t *testing.T) {
	d := NewDispatcher(nil, NewBackendRegistry(), NewTurnManager(), nil) // default 300s window
	now := time.Now()
	cases := []struct {
		name     string
		createMs int64
		want     bool
	}{
		{"past_300s_stale", now.Add(-301 * time.Second).UnixMilli(), true},
		{"past_299s_fresh", now.Add(-299 * time.Second).UnixMilli(), false},
		{"zero_passthrough", 0, false},
		{"future_passthrough", now.Add(time.Minute).UnixMilli(), false},
	}
	for _, c := range cases {
		if got := d.isStale(c.createMs); got != c.want {
			t.Errorf("isStale(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestDispatchIncomingStaleDropped verifies an expired message is dropped
// before any turn/sink work AND does not pollute the dedup table (a later
// delivery of a different event for the same logical message is unaffected).
func TestDispatchIncomingStaleDropped(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), boundRouter{"b"})

	old := time.Now().Add(-time.Hour).UnixMilli()
	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:      "evt_old",
		MessageID:    "om_old",
		ChatID:       "oc_chat",
		MsgType:      "text",
		Content:      "hello",
		CreateTimeMs: old,
	})
	if err != nil {
		t.Fatalf("DispatchIncoming stale: %v", err)
	}
	sends, _ := sink.counts()
	if sends != 0 {
		t.Fatalf("stale message must not trigger SendCard, got %d sends", sends)
	}
	if _, ok := d.turns.Get("om_old"); ok {
		t.Error("stale message must not start a turn")
	}
	// evt_old must NOT be in the dedup table (stale path skips Add).
	if !d.eventIDs.Add("evt_old") {
		t.Error("stale message polluted dedup table: evt_old remembered")
	}
}

// TestDispatchIncomingStaleConfigOverride verifies SetDedupConfig narrows the
// window so a message that passes the default 300s check is dropped under a
// tighter override.
func TestDispatchIncomingStaleConfigOverride(t *testing.T) {
	// 15s old: fresh under default 300s, stale under override 10s.
	createMs := time.Now().Add(-15 * time.Second).UnixMilli()

	// Default window: message flows (reaches SendCard, not dropped as stale).
	dflt := NewDispatcher(&fakeSink{}, NewBackendRegistry(), NewTurnManager(), boundRouter{"b"})
	// Use a distinct EventID per dispatcher so dedup doesn't interfere.
	_ = dflt.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID: "evt_dflt", MessageID: "om1", ChatID: "oc_c", MsgType: "text",
		Content: "hi", CreateTimeMs: createMs,
	})
	// Overridden window: same age, dropped.
	tight := NewDispatcher(&fakeSink{}, NewBackendRegistry(), NewTurnManager(), boundRouter{"b"})
	tight.SetDedupConfig(10*time.Second, 0, 0)
	_ = tight.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID: "evt_tight", MessageID: "om2", ChatID: "oc_c", MsgType: "text",
		Content: "hi", CreateTimeMs: createMs,
	})
	if _, ok := tight.turns.Get("om2"); ok {
		t.Error("message within override window (15s > 10s) should be dropped")
	}
}

type boundRouter struct{ backendID string }

func (b boundRouter) Resolve(string) (string, error) { return b.backendID, nil }
func (boundRouter) Set(string, string) error         { return nil }
func (boundRouter) ChatsOf(string) []string          { return nil }

// TestDispatchIncoming_SkillStripsPrefix verifies that a "/skill ..." message
// strips the wrapper, sets the Skill flag, and forwards the rest to the bound
// backend as a normal prompt.
func TestDispatchIncoming_SkillStripsPrefix(t *testing.T) {
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	const backendID = "claude-1"
	reg.Register(backendID, "claude")
	conn, ok := reg.Get(backendID)
	if !ok {
		t.Fatal("registered backend not found")
	}

	d := NewDispatcher(sink, reg, NewTurnManager(), boundRouter{backendID: backendID})
	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_skill",
		MessageID: "om_skill",
		ChatID:    "oc_chat",
		MsgType:   "text",
		Content:   "/skill /gsd-check-status 1",
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}

	select {
	case ev := <-conn.eventCh:
		if ev.Type != protocol.TypePrompt {
			t.Fatalf("want prompt event, got %q", ev.Type)
		}
		p := ev.Prompt
		if p == nil {
			t.Fatal("prompt payload is nil")
		}
		if !p.Skill {
			t.Error("want Skill=true")
		}
		if p.Text != "/gsd-check-status 1" {
			t.Errorf("Text = %q, want /gsd-check-status 1", p.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for forwarded event")
	}
}

// TestDispatchIncoming_SkillEmptyShowsUsage verifies that bare "/skill" returns
// a usage notice instead of forwarding an empty prompt.
func TestDispatchIncoming_SkillEmptyShowsUsage(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)

	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_skill_empty",
		MessageID: "om_skill_empty",
		ChatID:    "oc_chat",
		MsgType:   "text",
		Content:   "/skill",
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 usage notice, got %d sends", len(sink.sends))
	}
	if !strings.Contains(string(sink.sends[0].card), "/skill") {
		t.Errorf("usage notice should contain /skill usage, got %s", sink.sends[0].card)
	}
}

func TestParseQuestionFormValue(t *testing.T) {
	// single select + custom input
	choices, custom := parseQuestionFormValue(map[string]any{
		"q_0": "a", "custom_0": "note",
	})
	if len(choices) != 1 || choices[0] != "a" || custom != "note" {
		t.Fatalf("got choices=%v custom=%q", choices, custom)
	}

	// multi-select: values arrive as []any and join with "," on one line
	choices, _ = parseQuestionFormValue(map[string]any{
		"q_0": []any{"a", "b"},
	})
	if len(choices) != 1 || choices[0] != "a,b" {
		t.Fatalf("multi-select choices=%v, want [a,b]", choices)
	}

	// multiple questions: ordered by idx
	choices, custom = parseQuestionFormValue(map[string]any{
		"q_1": "y", "q_0": "x", "custom_1": "c1", "custom_0": "c0",
	})
	if len(choices) != 2 || choices[0] != "x" || choices[1] != "y" {
		t.Fatalf("ordered choices=%v, want [x y]", choices)
	}
	if custom != "c0\nc1" {
		t.Fatalf("ordered custom=%q, want c0\\nc1", custom)
	}

	// unknown names are ignored
	choices, custom = parseQuestionFormValue(map[string]any{"submit": "x"})
	if len(choices) != 0 || custom != "" {
		t.Fatalf("unknown got choices=%v custom=%q", choices, custom)
	}
}

// TestSendNoticeReplacesProgressCard verifies that a TypeNotice for a prompt
// that already has a progress card (e.g. a slash-command reply) Updates that
// card in place rather than SendCard-ing a new one — the cause of the
// "two replies per slash command" bug.
func TestSendNoticeReplacesProgressCard(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)

	// Simulate DispatchIncoming having already sent the "starting" progress
	// card and recorded the turn.
	const promptID = "om_prompt"
	d.turns.Start(promptID, "oc_chat", "om_progress", "claude-1")

	err := d.DispatchControl(context.Background(), RoutedControl{
		BackendID: "claude-1",
		Control: &protocol.Control{
			Type:     protocol.TypeNotice,
			PromptID: promptID,
			ChatID:   "oc_chat",
			Notice:   &protocol.NoticePayload{Level: "success", Title: "已切换模型", Message: "sonnet"},
		},
	})
	if err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}

	if len(sink.sends) != 0 {
		t.Errorf("expected no new SendCard (should update in place), got %d sends", len(sink.sends))
	}
	if len(sink.updates) != 1 || sink.updates[0].messageID != "om_progress" {
		t.Errorf("expected one UpdateCard of om_progress, got %+v", sink.updates)
	}
	if _, ok := d.turns.Get(promptID); ok {
		t.Error("turn should be finished after notice replaces the progress card")
	}
}

// TestSendNoticeFallbackToSendCard verifies that a TypeNotice without a prior
// progress card (no turn) falls back to sending a fresh card.
func TestSendNoticeFallbackToSendCard(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)

	err := d.DispatchControl(context.Background(), RoutedControl{
		BackendID: "claude-1",
		Control: &protocol.Control{
			Type:     protocol.TypeNotice,
			PromptID: "om_no_turn",
			ChatID:   "oc_chat",
			Notice:   &protocol.NoticePayload{Level: "info", Title: "提示", Message: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("expected one SendCard fallback, got %d", len(sink.sends))
	}
	if len(sink.updates) != 0 {
		t.Errorf("expected no UpdateCard, got %d", len(sink.updates))
	}
}

// TestDispatcherDedupTerminal verifies that a duplicate terminal control
// (Result/Error/Notice) for the same PromptID is dropped, so a retried backend
// POST does not double-post the final card.
func TestDispatcherDedupTerminal(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	const promptID = "p_dedup"
	d.turns.Start(promptID, "c1", "om_progress", "claude-1")

	mk := func() *protocol.Control {
		return &protocol.Control{
			Type:     protocol.TypeResult,
			PromptID: promptID,
			ChatID:   "c1",
			Result:   &protocol.ResultPayload{Text: "done"},
		}
	}
	if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: mk()}); err != nil {
		t.Fatalf("first result: %v", err)
	}
	// Duplicate terminal control should be dropped before reaching the bot.
	if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: mk()}); err != nil {
		t.Fatalf("second result: %v", err)
	}
	sends, updates := sink.counts()
	// First result updated the existing om_progress card in place; the
	// duplicate must not have produced any extra send or update.
	if sends != 0 {
		t.Errorf("expected 0 SendCard on duplicate, got %d", sends)
	}
	if updates != 1 {
		t.Errorf("expected exactly 1 UpdateCard, got %d", updates)
	}
}

// stubRouter implements just enough ChatRouter for the online/offline tests.
type stubRouter struct{ chats []string }

func (stubRouter) Resolve(string) (string, error) { return "", nil }
func (stubRouter) Set(string, string) error       { return nil }
func (s stubRouter) ChatsOf(string) []string      { return s.chats }

// blockingSink blocks SendCard until the request context is cancelled, then
// returns the ctx error — proving the notice path propagates a deadline.
type blockingSink struct{}

func (blockingSink) SendCard(ctx context.Context, _ string, _ []byte, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
func (blockingSink) UpdateCard(context.Context, string, []byte) error { return nil }

// TestOnBackendOffline_BoundedByTimeout verifies a stalled Feishu send cannot
// wedge the notify loop: SendCard honors the context deadline and returns
// within noticeSendTimeout, not hanging forever.
func TestOnBackendOffline_BoundedByTimeout(t *testing.T) {
	d := NewDispatcher(blockingSink{}, NewBackendRegistry(), NewTurnManager(), stubRouter{chats: []string{"oc_a"}})

	done := make(chan struct{})
	go func() {
		d.OnBackendOffline("back-1", "claude")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(noticeSendTimeout + 2*time.Second):
		t.Fatal("OnBackendOffline was not bounded by noticeSendTimeout")
	}
}

// TestFireCallback_RecoversPanic verifies a panicking online/offline callback
// is recovered and logged, not propagated to crash the process.
func TestFireCallback_RecoversPanic(t *testing.T) {
	srv := NewIPCServer(NewBackendRegistry(), "")
	boom := func(string, string) { panic("callback boom") }
	srv.onOffline.Store(&boom)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.fireCallback(srv.onOffline.Load(), "back-1", "claude", "offline")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fireCallback did not return after panic")
	}
}

// TestDispatchIncoming_PromptTooLong verifies an oversized prompt is rejected
// with a notice and never forwarded to the backend.
func TestDispatchIncoming_PromptTooLong(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)

	huge := strings.Repeat("a", maxPromptBytes+1)
	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_big",
		MessageID: "om_big",
		ChatID:    "oc_chat",
		MsgType:   "text",
		Content:   `{"text":"` + huge + `"}`,
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 notice card (too long), got %d sends", len(sink.sends))
	}
	if _, ok := d.turns.Get("om_big"); ok {
		t.Error("oversized prompt must not start a turn")
	}
}

// TestSendResult_LateProgressDoesNotClobber verifies a straggler progress
// update arriving after the terminal result is dropped (the messageID is
// finalized) so the debouncer cannot flush a stale progress frame over the
// final card.
func TestSendResult_LateProgressDoesNotClobber(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	const promptID = "om_p"
	d.turns.Start(promptID, "oc_chat", "om_card", "claude-1")

	// Terminal result → marks om_card finalized.
	result := &protocol.Control{
		Type:     protocol.TypeResult,
		PromptID: promptID,
		ChatID:   "oc_chat",
		Result:   &protocol.ResultPayload{Text: "done"},
	}
	if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: result}); err != nil {
		t.Fatalf("result: %v", err)
	}
	updatesAfterResult := len(sink.updates)

	// Straggler text delta for the same prompt arrives after the terminal.
	// The turn was finished by sendResult, so updateProgress's turns.Get
	// returns !ok and the update is skipped at that layer too — but the
	// finalized guard is the defense when a turn still exists. Restart a turn
	// to exercise the finalized path directly.
	d.turns.Start(promptID, "oc_chat", "om_card", "claude-1")
	d.markFinalized("om_card")
	late := &protocol.Control{
		Type:     protocol.TypeText,
		PromptID: promptID,
		Text:     &protocol.TextPayload{Delta: "stale"},
	}
	if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: late}); err != nil {
		t.Fatalf("late progress: %v", err)
	}
	if len(sink.updates) != updatesAfterResult {
		t.Errorf("late progress should be dropped (finalized), got %d updates (was %d)",
			len(sink.updates), updatesAfterResult)
	}
}

// TestSendResult_IncludesSummary verifies the result card carries the
// execution summary derived from the prior progress state (tool/subagent
// counts): the dispatcher snapshots ProgressState.Summary() before cleanup
// and forwards it to RenderResult. This pins the contract end-to-end so a
// future refactor cannot silently drop the summary line.
func TestSendResult_IncludesSummary(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	const promptID = "om_prompt"
	d.turns.Start(promptID, "oc_chat", "om_progress", "claude-1")

	ctx := context.Background()
	backendID := "claude-1"
	must := func(c *protocol.Control) {
		t.Helper()
		if err := d.DispatchControl(ctx, RoutedControl{BackendID: backendID, Control: c}); err != nil {
			t.Fatalf("DispatchControl: %v", err)
		}
	}
	// Build a progress state: two reads + one subagent row.
	must(&protocol.Control{Type: protocol.TypeToolUse, PromptID: promptID, ToolUse: &protocol.ToolUsePayload{Name: "read", Input: "/a.go"}})
	must(&protocol.Control{Type: protocol.TypeToolResult, PromptID: promptID, ToolResult: &protocol.ToolResultPayload{Name: "read", Output: "body"}})
	must(&protocol.Control{Type: protocol.TypeToolUse, PromptID: promptID, ToolUse: &protocol.ToolUsePayload{Name: "read", Input: "/b.go"}})
	must(&protocol.Control{Type: protocol.TypeToolResult, PromptID: promptID, ToolResult: &protocol.ToolResultPayload{Name: "read", Output: "body"}})
	must(&protocol.Control{Type: protocol.TypeToolUse, PromptID: promptID, ToolUse: &protocol.ToolUsePayload{Name: "Explore Agent", Input: "explore", IsSubagent: true}})
	must(&protocol.Control{Type: protocol.TypeToolResult, PromptID: promptID, ToolResult: &protocol.ToolResultPayload{Name: "Explore Agent", Output: "done", IsSubagent: true}})
	// Terminal result replaces the progress card in place.
	must(&protocol.Control{Type: protocol.TypeResult, PromptID: promptID, ChatID: "oc_chat",
		Result: &protocol.ResultPayload{Text: "done", Tokens: 10}})

	if len(sink.updates) == 0 {
		t.Fatalf("expected an UpdateCard for the result, got none")
	}
	got := string(sink.updates[len(sink.updates)-1].card)
	for _, want := range []string{"读取 2", "子代理 1", "10 tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("result card missing %q: %s", want, got)
		}
	}
}

// ctxSensitiveSink is a CardSink whose UpdateCard rejects calls made with an
// already-cancelled context. It mirrors how the real Feishu SDK treats a
// cancelled ctx (returns immediately), letting the debouncer tests distinguish
// "flushed with a live ctx" from "flushed with the cancelled d.ctx (no-op)".
type ctxSensitiveSink struct {
	mu      sync.Mutex
	updates int
}

func (c *ctxSensitiveSink) SendCard(ctx context.Context, _ string, _ []byte, _ string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	c.mu.Lock()
	c.updates++
	c.mu.Unlock()
	return "om_x", nil
}
func (c *ctxSensitiveSink) UpdateCard(ctx context.Context, _ string, _ []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	c.updates++
	c.mu.Unlock()
	return nil
}
func (c *ctxSensitiveSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updates
}

// TestDebouncer_FinalFlushUsesLiveContext pins the R1 fix: when the debouncer's
// lifecycle ctx is cancelled (shutdown), the final flush must still deliver
// pending frames via a fresh ctx. flushCtx(freshCtx) reaches the sink; the old
// flush() path (reusing d.ctx) is a no-op and the frame is lost.
func TestDebouncer_FinalFlushUsesLiveContext(t *testing.T) {
	sink := &ctxSensitiveSink{}
	ctx, cancel := context.WithCancel(context.Background())
	d := newCardDebouncer(ctx, sink, 50*time.Millisecond)
	defer cancel()

	d.enqueue("om_card", []byte("final"))
	// Simulate shutdown: the lifecycle ctx is cancelled before the ticker fires.
	cancel()

	freshCtx, freshCancel := context.WithTimeout(context.Background(), time.Second)
	defer freshCancel()
	d.flushCtx(freshCtx)

	if sink.count() != 1 {
		t.Fatalf("final flush with fresh ctx: got %d updates, want 1", sink.count())
	}
}

// TestDebouncer_NormalFlushDropsOnCancelledCtx is the contrast: once d.ctx is
// cancelled, plain flush() (which reuses d.ctx) is a no-op against a
// ctx-sensitive sink — the exact silent drop the fresh-ctx fix repairs.
func TestDebouncer_NormalFlushDropsOnCancelledCtx(t *testing.T) {
	sink := &ctxSensitiveSink{}
	ctx, cancel := context.WithCancel(context.Background())
	d := newCardDebouncer(ctx, sink, 50*time.Millisecond)

	d.enqueue("om_card", []byte("pending"))
	cancel()
	d.flush() // reuses the now-cancelled d.ctx

	if sink.count() != 0 {
		t.Fatalf("flush() with cancelled ctx: got %d updates, want 0 (proving the fresh-ctx path is needed)", sink.count())
	}
}

// TestOnBackendOffline_ReleasesInFlightTurn pins the M3 fix: a disconnecting
// backend never sends the terminal control that would Finish its turn, so
// OnBackendOffline must release that backend's in-flight turn and progress
// state instead of leaving them to accumulate across reconnects.
func TestOnBackendOffline_ReleasesInFlightTurn(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), stubRouter{chats: []string{"oc_a"}})

	d.turns.Start("p-1", "oc_a", "om_card", "back-A")
	d.progressMu.Lock()
	d.progress["p-1"] = nil // a live turn owns a progress slot (value presence is what matters)
	d.progressMu.Unlock()

	d.OnBackendOffline("back-A", "claude")

	if _, ok := d.turns.Get("p-1"); ok {
		t.Fatal("in-flight turn for offline backend should be released")
	}
	d.progressMu.Lock()
	_, leak := d.progress["p-1"]
	d.progressMu.Unlock()
	if leak {
		t.Fatal("progress state for offline backend should be cleaned up")
	}
}

// TestOnBackendOffline_PreservesOtherBackends ensures the cleanup only touches
// the offline backend's turns — a turn owned by a still-online backend survives.
func TestOnBackendOffline_PreservesOtherBackends(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), stubRouter{chats: []string{"oc_a"}})

	d.turns.Start("p-A", "oc_a", "om_A", "back-A")
	d.turns.Start("p-B", "oc_b", "om_B", "back-B")

	d.OnBackendOffline("back-A", "claude")

	if _, ok := d.turns.Get("p-A"); ok {
		t.Fatal("back-A turn should be released")
	}
	if _, ok := d.turns.Get("p-B"); !ok {
		t.Fatal("back-B turn must survive back-A going offline")
	}
}
