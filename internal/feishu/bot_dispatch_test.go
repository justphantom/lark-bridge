package feishu

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkimv1 "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/justphantom/lark-bridge/internal/log"
)

func ptrString(s string) *string { return &s }

// newTestBot builds a Bot with only the fields handleP2MessageReceiveV1
// touches, avoiding the real WebSocket/channel construction in NewBot.
func newTestBot(t *testing.T, onIncoming IncomingHandler) *Bot {
	t.Helper()
	b := &Bot{logger: log.Nop()}
	if onIncoming != nil {
		b.onIncoming.Store(&onIncoming)
	}
	return b
}

// TestHandleP2MessageReceiveV1_NilGuards covers the panic previously
// reachable via the dead guard: each nil variant must return nil
// without panicking.
func TestHandleP2MessageReceiveV1_NilGuards(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		event *larkimv1.P2MessageReceiveV1
	}{
		{"event nil", nil},
		{"event.Event nil", &larkimv1.P2MessageReceiveV1{}},
		{"event.Event.Message nil", &larkimv1.P2MessageReceiveV1{
			Event: &larkimv1.P2MessageReceiveV1Data{},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBot(t, nil)
			if err := b.handleP2MessageReceiveV1(ctx, tc.event); err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}

// TestHandleP2MessageReceiveV1_NoHandler verifies the no-handler
// branch returns nil and never calls onIncoming.
func TestHandleP2MessageReceiveV1_NoHandler(t *testing.T) {
	b := newTestBot(t, nil)
	chatID := "oc_test"
	msgID := "om_test"
	event := &larkimv1.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: "evt_1"}},
		Event: &larkimv1.P2MessageReceiveV1Data{
			Message: &larkimv1.EventMessage{
				ChatId:      &chatID,
				MessageId:   &msgID,
				ChatType:    ptrString("group"),
				MessageType: ptrString("text"),
				Content:     ptrString(`{"text":"hi"}`),
			},
		},
	}
	if err := b.handleP2MessageReceiveV1(context.Background(), event); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// TestHandleP2MessageReceiveV1_HappyPath verifies a complete event
// invokes onIncoming with a normalized IncomingMessage.
func TestHandleP2MessageReceiveV1_HappyPath(t *testing.T) {
	var called atomic.Bool
	var got *IncomingMessage
	b := newTestBot(t, func(ctx context.Context, m *IncomingMessage) error {
		called.Store(true)
		got = m
		return nil
	})
	chatID := "oc_chat"
	msgID := "om_msg"
	openID := "ou_sender"
	createMs := time.Now().UnixMilli()
	event := &larkimv1.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: "evt_1"}},
		Event: &larkimv1.P2MessageReceiveV1Data{
			Sender: &larkimv1.EventSender{
				SenderId: &larkimv1.UserId{OpenId: &openID},
			},
			Message: &larkimv1.EventMessage{
				ChatId:      &chatID,
				MessageId:   &msgID,
				ChatType:    ptrString("group"),
				MessageType: ptrString("text"),
				Content:     ptrString(`{"text":"hello"}`),
				CreateTime:  ptrString(strconv.FormatInt(createMs, 10)),
			},
		},
	}
	if err := b.handleP2MessageReceiveV1(context.Background(), event); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !called.Load() {
		t.Fatal("onIncoming was not called")
	}
	if got.ChatID != chatID || got.MessageID != msgID || got.Content != "hello" || got.SenderOpenID != openID {
		t.Fatalf("unexpected IncomingMessage: %+v", got)
	}
	if got.MsgType != "text" {
		t.Errorf("MsgType = %q, want text", got.MsgType)
	}
	if got.CreateTimeMs != createMs {
		t.Errorf("CreateTimeMs = %d, want %d", got.CreateTimeMs, createMs)
	}
}

// TestHandleP2MessageReceiveV1_NonTextMsgType verifies non-text messages carry
// their raw MsgType through (the dispatcher rejects them downstream), and the
// content is NOT unwrapped as text.
func TestHandleP2MessageReceiveV1_NonTextMsgType(t *testing.T) {
	var got *IncomingMessage
	b := newTestBot(t, func(ctx context.Context, m *IncomingMessage) error {
		got = m
		return nil
	})
	chatID, msgID := "oc_chat", "om_msg"
	rawContent := `{"image_key":"img_v3_x"}`
	event := &larkimv1.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: "evt_1"}},
		Event: &larkimv1.P2MessageReceiveV1Data{
			Sender: &larkimv1.EventSender{},
			Message: &larkimv1.EventMessage{
				ChatId:      &chatID,
				MessageId:   &msgID,
				MessageType: ptrString("image"),
				Content:     ptrString(rawContent),
			},
		},
	}
	if err := b.handleP2MessageReceiveV1(context.Background(), event); err != nil {
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
