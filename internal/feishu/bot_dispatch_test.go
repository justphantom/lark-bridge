package feishu

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
)

// newTestBot builds a Bot with only the fields handleMessageReceive touches,
// avoiding the real client construction in NewBot.
func newTestBot(t *testing.T, onIncoming IncomingHandler) *Bot {
	t.Helper()
	b := &Bot{logger: log.Nop()}
	if onIncoming != nil {
		b.onIncoming.Store(&onIncoming)
	}
	return b
}

// TestHandleMessageReceive_NilGuards covers the panic previously reachable
// via a dead guard: a nil event must return nil without panicking.
func TestHandleMessageReceive_NilGuards(t *testing.T) {
	ctx := context.Background()
	b := newTestBot(t, nil)
	if err := b.handleMessageReceive(ctx, nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// TestHandleMessageReceive_NoHandler verifies the no-handler branch returns
// nil and never calls onIncoming.
func TestHandleMessageReceive_NoHandler(t *testing.T) {
	b := newTestBot(t, nil)
	event := &lark.MessageReceiveEvent{
		EventID:   "evt_1",
		MessageID: "om_test",
		ChatID:    "oc_test",
		MsgType:   "text",
		Content:   `{"text":"hi"}`,
	}
	if err := b.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// TestHandleMessageReceive_HappyPath verifies a complete event invokes
// onIncoming with a normalized IncomingMessage, including text-content
// unwrapping and create-time parsing.
func TestHandleMessageReceive_HappyPath(t *testing.T) {
	var called atomic.Bool
	var got *IncomingMessage
	b := newTestBot(t, func(ctx context.Context, m *IncomingMessage) error {
		called.Store(true)
		got = m
		return nil
	})
	createMs := time.Now().UnixMilli()
	event := &lark.MessageReceiveEvent{
		EventID:      "evt_1",
		MessageID:    "om_msg",
		ChatID:       "oc_chat",
		ChatType:     "group",
		MsgType:      "text",
		Content:      `{"text":"hello"}`,
		SenderOpenID: "ou_sender",
		CreateTimeMs: createMs,
	}
	if err := b.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !called.Load() {
		t.Fatal("onIncoming was not called")
	}
	if got.ChatID != "oc_chat" || got.MessageID != "om_msg" || got.Content != "hello" || got.SenderOpenID != "ou_sender" {
		t.Fatalf("unexpected IncomingMessage: %+v", got)
	}
	if got.MsgType != "text" {
		t.Errorf("MsgType = %q, want text", got.MsgType)
	}
	if got.CreateTimeMs != createMs {
		t.Errorf("CreateTimeMs = %d, want %d", got.CreateTimeMs, createMs)
	}
}

// TestHandleMessageReceive_NonTextMsgType verifies non-text messages carry
// their raw MsgType through (the dispatcher rejects them downstream), and the
// content is NOT unwrapped as text.
func TestHandleMessageReceive_NonTextMsgType(t *testing.T) {
	var got *IncomingMessage
	b := newTestBot(t, func(ctx context.Context, m *IncomingMessage) error {
		got = m
		return nil
	})
	rawContent := `{"image_key":"img_v3_x"}`
	event := &lark.MessageReceiveEvent{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		MsgType:   "image",
		Content:   rawContent,
	}
	if err := b.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.MsgType != "image" {
		t.Errorf("MsgType = %q, want image", got.MsgType)
	}
	// Non-text content must pass through verbatim (no text-unwrap).
	if got.Content != rawContent {
		t.Errorf("Content = %q, want raw %q", got.Content, rawContent)
	}
}

// TestHandleMessageReceive_MentionsCopied verifies mention translation from
// the lark type to the feishu type preserves Key/OpenID/Name/IsBot.
func TestHandleMessageReceive_MentionsCopied(t *testing.T) {
	var got *IncomingMessage
	b := newTestBot(t, func(ctx context.Context, m *IncomingMessage) error {
		got = m
		return nil
	})
	event := &lark.MessageReceiveEvent{
		MsgType: "text",
		Content: `{"text":"x"}`,
		Mentions: []lark.Mention{
			{Key: "@_user_1", Name: "Alice", OpenID: "ou_a"},
			{Key: "@_user_2", IsBot: true},
		},
	}
	if err := b.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(got.Mentions) != 2 {
		t.Fatalf("mentions len = %d, want 2", len(got.Mentions))
	}
	if got.Mentions[0].Name != "Alice" || got.Mentions[1].IsBot != true {
		t.Fatalf("mentions not copied: %+v", got.Mentions)
	}
}
