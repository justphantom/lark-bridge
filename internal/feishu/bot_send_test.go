package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hu/lark-bridge/internal/log"
	sdktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
)

// newSendOnlyBot builds a Bot whose channel/imService are nil. The send
// methods reach their input-validation branches (empty card, empty messageID,
// nil imService) before touching the channel, so this is enough to exercise
// the pre-send guards.
func newSendOnlyBot(t *testing.T) *Bot {
	t.Helper()
	return &Bot{logger: log.Nop()}
}

func TestSendCardEmptyBody(t *testing.T) {
	b := newSendOnlyBot(t)
	_, err := b.SendCard(context.Background(), "oc_chat", nil, "")
	if err == nil {
		t.Fatal("expected error for empty card body")
	}
}

func TestUpdateCardEmptyBody(t *testing.T) {
	b := newSendOnlyBot(t)
	if err := b.UpdateCard(context.Background(), "om_msg", nil); err == nil {
		t.Fatal("expected error for empty card body")
	}
}

func TestUpdateCardEmptyMessageID(t *testing.T) {
	b := newSendOnlyBot(t)
	if err := b.UpdateCard(context.Background(), "", []byte("{}")); err == nil {
		t.Fatal("expected error for empty messageID")
	}
}

func TestUpdateCardNilIMService(t *testing.T) {
	// imService nil guard returns a descriptive error before any API call.
	b := newSendOnlyBot(t)
	err := b.UpdateCard(context.Background(), "om_msg", []byte("{}"))
	if err == nil || err.Error() != "feishu: im service not initialized" {
		t.Fatalf("expected im service error, got %v", err)
	}
}

// TestIsContentTooLarge verifies the 230025 (content too large) detection.
// The SDK classifies 230025 as an unknown code, so detection is by substring.
func TestIsContentTooLarge(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"230025 in message", errors.New("FeishuChannelError(code=unknown): ...code:230025..."), true},
		{"other code", errors.New("feishu: send card: code:230002"), false},
		{"plain error", errors.New("network timeout"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isContentTooLarge(c.err); got != c.want {
				t.Errorf("isContentTooLarge(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestFallbackText verifies the fixed fallback text is short enough to never
// trip the size limit and the card JSON built from it is valid JSON.
func TestFallbackText(t *testing.T) {
	if len(fallbackText) > 200 {
		t.Errorf("fallbackText = %d bytes, want <= 200", len(fallbackText))
	}
	card := fallbackCardJSON()
	var m map[string]any
	if err := json.Unmarshal(card, &m); err != nil {
		t.Fatalf("fallback card json invalid: %v", err)
	}
	if !strings.Contains(string(card), fallbackText) {
		t.Error("fallback card json missing fallback text")
	}
	if len(card) > 500 {
		t.Errorf("fallback card = %d bytes, want <= 500", len(card))
	}
}

// fakeChannel implements sdktypes.Channel for SendCard tests. Only Send is
// configurable; all other methods are no-ops so the test can drive the success
// path without a real WebSocket connection.
type fakeChannel struct {
	sendResult *sdktypes.SendResult
	sendErr    error
}

func (f *fakeChannel) Send(_ context.Context, _ *sdktypes.SendInput) (*sdktypes.SendResult, error) {
	return f.sendResult, f.sendErr
}
func (f *fakeChannel) OnMessage(func(context.Context, *sdktypes.NormalizedMessage) error)  {}
func (f *fakeChannel) OnReaction(func(context.Context, *sdktypes.ReactionEvent) error)     {}
func (f *fakeChannel) OnComment(func(context.Context, *sdktypes.CommentEvent) error)       {}
func (f *fakeChannel) OnBotAdded(func(context.Context, *sdktypes.BotAddedEvent) error)     {}
func (f *fakeChannel) OnCardAction(func(context.Context, *sdktypes.CardActionEvent) error) {}
func (f *fakeChannel) OnReject(func(context.Context, *sdktypes.RejectEvent) error)         {}
func (f *fakeChannel) DownloadFile(context.Context, string, string) ([]byte, error)        { return nil, nil }
func (f *fakeChannel) OnReady(func())                                                      {}
func (f *fakeChannel) OnError(func(error))                                                 {}
func (f *fakeChannel) OnReconnecting(func())                                               {}
func (f *fakeChannel) OnReconnected(func())                                                {}
func (f *fakeChannel) OnDisconnected(func())                                               {}
func (f *fakeChannel) Start(context.Context) error                                         { return nil }
func (f *fakeChannel) Stream(context.Context, *sdktypes.SendInput) (sdktypes.StreamController, error) {
	return nil, nil
}
func (f *fakeChannel) UpdatePolicy(sdktypes.PolicyConfig)                   {}
func (f *fakeChannel) GetPolicy() sdktypes.PolicyConfig                     { return sdktypes.PolicyConfig{} }
func (f *fakeChannel) GetBotIdentity(context.Context) *sdktypes.BotIdentity { return nil }
func (f *fakeChannel) Stop(context.Context) error                           { return nil }

// TestSendCard_RefreshesWatchdog verifies that a successful SendCard calls
// markHealthy, so the frontend watchdog does not kill the process during a
// long conversation with no inbound WS traffic (the root cause of Claude
// being killed mid-conversation).
func TestSendCard_RefreshesWatchdog(t *testing.T) {
	b := &Bot{
		logger: log.Nop(),
		ch: &fakeChannel{
			sendResult: &sdktypes.SendResult{MessageID: "om_test"},
		},
	}
	if !b.LastHealthy().IsZero() {
		t.Fatal("expected zero lastHealthy before any send")
	}
	if _, err := b.SendCard(context.Background(), "oc_chat", []byte("{}"), ""); err != nil {
		t.Fatalf("SendCard: %v", err)
	}
	if b.LastHealthy().IsZero() {
		t.Fatal("expected non-zero lastHealthy after successful SendCard")
	}
}

// TestSendCard_ErrorDoesNotRefreshWatchdog verifies a failed send does not
// refresh the watchdog — only success proves the connection is alive.
func TestSendCard_ErrorDoesNotRefreshWatchdog(t *testing.T) {
	b := &Bot{
		logger: log.Nop(),
		ch: &fakeChannel{
			sendErr: errors.New("network error"),
		},
	}
	if _, err := b.SendCard(context.Background(), "oc_chat", []byte("{}"), ""); err == nil {
		t.Fatal("expected error from failed send")
	}
	if !b.LastHealthy().IsZero() {
		t.Fatal("expected zero lastHealthy after failed SendCard")
	}
}

// TestFallbackCardJSON_Valid verifies the constructed card is valid JSON and
// carries fallbackText. Guards the json.Marshal path: if fallbackText ever
// grows a quote or backslash, string concatenation would have broken the
// Patch silently.
func TestFallbackCardJSON_Valid(t *testing.T) {
	b := fallbackCardJSON()
	var got struct {
		Schema string `json:"schema"`
		Body   struct {
			Elements []struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("fallbackCardJSON produced invalid JSON: %v\n%s", err, b)
	}
	if got.Schema != "2.0" {
		t.Errorf("schema = %q, want 2.0", got.Schema)
	}
	if len(got.Body.Elements) != 1 {
		t.Fatalf("elements len = %d, want 1", len(got.Body.Elements))
	}
	el := got.Body.Elements[0]
	if el.Tag != "markdown" {
		t.Errorf("element tag = %q, want markdown", el.Tag)
	}
	if el.Content != fallbackText {
		t.Errorf("element content = %q, want %q", el.Content, fallbackText)
	}
}
