package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
)

// TestSendCardEmptyBody verifies the input guard fires before reaching the client.
func TestSendCardEmptyBody(t *testing.T) {
	b := &Bot{logger: log.Nop(), client: &fakeClient{}}
	if _, err := b.SendCard(context.Background(), "oc_chat", nil, ""); err == nil {
		t.Fatal("expected error for empty card body")
	}
}

// TestUpdateCardEmptyBody/EmptyMessageID/NilClient cover the pre-send guards.
func TestUpdateCardEmptyBody(t *testing.T) {
	b := &Bot{logger: log.Nop(), client: &fakeClient{}}
	if err := b.UpdateCard(context.Background(), "om_msg", nil); err == nil {
		t.Fatal("expected error for empty card body")
	}
}

func TestUpdateCardEmptyMessageID(t *testing.T) {
	b := &Bot{logger: log.Nop(), client: &fakeClient{}}
	if err := b.UpdateCard(context.Background(), "", []byte("{}")); err == nil {
		t.Fatal("expected error for empty messageID")
	}
}

func TestUpdateCardNilClient(t *testing.T) {
	// client nil guard returns a descriptive error before any API call.
	b := &Bot{logger: log.Nop()}
	err := b.UpdateCard(context.Background(), "om_msg", []byte("{}"))
	if err == nil || err.Error() != "feishu: client not initialized" {
		t.Fatalf("expected client-not-initialized error, got %v", err)
	}
}

// TestIsCardContentRejected verifies detection of Feishu CONTENT rejections:
// 230025 (body too large), 11310 (element/table over limit), and the generic
// "over limit" phrase — all of which should trigger a fallback, while
// unrelated errors must not.
func TestIsCardContentRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"230025 body too large", &lark.APIError{Code: 230025, Msg: "content too large"}, true},
		{"11310 table over limit", &lark.APIError{Code: 11310, Msg: "card table number over limit"}, true},
		{"over limit phrase only", errors.New("ErrMsg: column number over limit"), true},
		{"other code", &lark.APIError{Code: 230002, Msg: "other"}, false},
		{"plain error", errors.New("network timeout"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCardContentRejected(c.err); got != c.want {
				t.Errorf("isCardContentRejected(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestSendCard_ContentRejectedReturnsSentinel pins that SendCard surfaces the
// detectable ErrCardContentRejected (so a caller with the original text can
// fall back) instead of silently swallowing.
func TestSendCard_ContentRejectedReturnsSentinel(t *testing.T) {
	fc := &fakeClient{sendErr: &lark.APIError{Code: 11310, Msg: "card table number over limit"}}
	b := &Bot{logger: log.Nop(), client: fc}
	_, err := b.SendCard(context.Background(), "oc_chat", []byte("{}"), "")
	if !errors.Is(err, ErrCardContentRejected) {
		t.Fatalf("SendCard err = %v, want errors.Is ErrCardContentRejected", err)
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

// TestSendCard_RefreshesWatchdog verifies a successful SendCard calls
// markHealthy, so the watchdog does not kill the process during a long
// conversation with no inbound WS traffic.
func TestSendCard_RefreshesWatchdog(t *testing.T) {
	fc := &fakeClient{sendResult: &lark.SendResult{MessageID: "om_test"}}
	b := &Bot{logger: log.Nop(), client: fc}
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
	fc := &fakeClient{sendErr: errors.New("network error")}
	b := &Bot{logger: log.Nop(), client: fc}
	if _, err := b.SendCard(context.Background(), "oc_chat", []byte("{}"), ""); err == nil {
		t.Fatal("expected error from failed send")
	}
	if !b.LastHealthy().IsZero() {
		t.Fatal("expected zero lastHealthy after failed SendCard")
	}
}

// TestUpdateCard_SuccessAndPatch verifies UpdateCard delegates to
// PatchMessage exactly once on success and marks the bot healthy.
func TestUpdateCard_SuccessAndPatch(t *testing.T) {
	fc := &fakeClient{}
	b := &Bot{logger: log.Nop(), client: fc}
	if err := b.UpdateCard(context.Background(), "om_msg", []byte("{}")); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}
	if got := fc.patchCalls.Load(); got != 1 {
		t.Errorf("patch calls = %d, want 1", got)
	}
	if b.LastHealthy().IsZero() {
		t.Error("expected markHealthy after successful UpdateCard")
	}
}

// TestUpdateCard_ContentRejectedFallsBack verifies a 230025 rejection during
// patch triggers the minimal fallback card and returns success.
func TestUpdateCard_ContentRejectedFallsBack(t *testing.T) {
	fc := &fakeClient{
		patchErr:      &lark.APIError{Code: 230025, Msg: "too large"},
		patchErrOnNth: 1, // first patch rejected; second (fallback) succeeds
	}
	b := &Bot{logger: log.Nop(), client: fc}
	if err := b.UpdateCard(context.Background(), "om_msg", []byte("{}")); err != nil {
		t.Fatalf("UpdateCard should swallow content-rejected via fallback: %v", err)
	}
	// Two patches: the original (rejected) + the fallback.
	if got := fc.patchCalls.Load(); got != 2 {
		t.Errorf("patch calls = %d, want 2 (original + fallback)", got)
	}
	if !strings.Contains(fc.patchLast, fallbackText) {
		t.Errorf("fallback patch body missing fallback text: %q", fc.patchLast)
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
		Config struct {
			UpdateMulti bool `json:"update_multi"`
		} `json:"config"`
		Elements []struct {
			Tag     string `json:"tag"`
			Content string `json:"content"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("fallbackCardJSON produced invalid JSON: %v\n%s", err, b)
	}
	if got.Schema != "1.0" {
		t.Errorf("schema = %q, want 1.0", got.Schema)
	}
	if !got.Config.UpdateMulti {
		t.Errorf("update_multi = false, want true")
	}
	if len(got.Elements) != 1 {
		t.Fatalf("elements len = %d, want 1", len(got.Elements))
	}
	el := got.Elements[0]
	if el.Tag != "markdown" {
		t.Errorf("element tag = %q, want markdown", el.Tag)
	}
	if el.Content != fallbackText {
		t.Errorf("element content = %q, want %q", el.Content, fallbackText)
	}
}
