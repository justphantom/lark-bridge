package feishu

import (
	"context"
	"errors"
	"testing"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
)

// greenCard is a schema 1.0 card whose header template the verify loop reads
// back to confirm a PATCH persisted.
const greenCard = `{"schema":"1.0","config":{"update_multi":true},"elements":[],"header":{"template":"green","title":{"tag":"plain_text","content":"done"}}}`

// noVerifyBackoff zeroes the verify-loop backoff for the duration of a test so
// the PATCH→GET→retry loop is instant, then restores the production default.
func noVerifyBackoff(t *testing.T) {
	t.Helper()
	prev := cardVerifyBackoff
	cardVerifyBackoff = 0
	t.Cleanup(func() { cardVerifyBackoff = prev })
}

// TestUpdateCardVerified_HappyPath: the PATCH lands and read-back matches →
// one PATCH, one GET, no error. The read-back content reorders keys vs. what
// was sent; the colour fingerprint must match on template alone, not layout.
func TestUpdateCardVerified_HappyPath(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{getMessageContent: `{"elements":[],"header":{"template":"green"},"schema":"1.0"}`}
	b := &Bot{logger: log.Nop(), client: fc}
	if err := b.UpdateCardVerified(context.Background(), "om_x", []byte(greenCard)); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if pc, gc := fc.patchCalls.Load(), fc.getCalls.Load(); pc != 1 || gc != 1 {
		t.Fatalf("want 1 patch + 1 get, got patch=%d get=%d", pc, gc)
	}
}

// TestUpdateCardVerified_RevertRetries: read-back keeps returning the OLD
// colour (bounce-back) → the loop re-PATCHes up to cardVerifyMaxAttempts then
// surfaces ErrCardVerifyMismatch instead of looping forever.
func TestUpdateCardVerified_RevertRetries(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{getMessageContent: `{"header":{"template":"blue"}}`}
	b := &Bot{logger: log.Nop(), client: fc}
	if err := b.UpdateCardVerified(context.Background(), "om_x", []byte(greenCard)); !errors.Is(err, ErrCardVerifyMismatch) {
		t.Fatalf("want ErrCardVerifyMismatch, got %v", err)
	}
	if got := fc.patchCalls.Load(); got != int32(cardVerifyMaxAttempts) {
		t.Fatalf("want %d patches, got %d", cardVerifyMaxAttempts, got)
	}
	if got := fc.getCalls.Load(); got != int32(cardVerifyMaxAttempts) {
		t.Fatalf("want %d read-backs, got %d", cardVerifyMaxAttempts, got)
	}
}

// TestUpdateCardVerified_CardGoneShortCircuits: a withdrawn card (code:230011)
// returns immediately — no GET, no retry — since re-PATCH can never succeed.
func TestUpdateCardVerified_CardGoneShortCircuits(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{patchErr: &lark.APIError{Code: 230011, Msg: "withdrawn"}}
	b := &Bot{logger: log.Nop(), client: fc}
	err := b.UpdateCardVerified(context.Background(), "om_x", []byte(greenCard))
	if err == nil || !IsCardGone(err) {
		t.Fatalf("want card-gone error, got %v", err)
	}
	if pc, gc := fc.patchCalls.Load(), fc.getCalls.Load(); pc != 1 || gc != 0 {
		t.Fatalf("want 1 patch + 0 get, got patch=%d get=%d", pc, gc)
	}
}

// TestUpdateCardVerified_HeaderlessSkipsReadback: a card with no header
// template has nothing to fingerprint → trust the PATCH and skip the GET.
func TestUpdateCardVerified_HeaderlessSkipsReadback(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{}
	b := &Bot{logger: log.Nop(), client: fc}
	if err := b.UpdateCardVerified(context.Background(), "om_x", []byte(`{"schema":"1.0","elements":[]}`)); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if pc, gc := fc.patchCalls.Load(), fc.getCalls.Load(); pc != 1 || gc != 0 {
		t.Fatalf("headerless card should skip read-back; got patch=%d get=%d", pc, gc)
	}
}

// TestUpdateCardVerified_GetErrorRetries: a failing read-back (e.g. missing
// im:message:read scope) is retried, then surfaces the last GET error rather
// than silently trusting an unconfirmable PATCH.
func TestUpdateCardVerified_GetErrorRetries(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{getMessageErr: errors.New("scope denied")}
	b := &Bot{logger: log.Nop(), client: fc}
	err := b.UpdateCardVerified(context.Background(), "om_x", []byte(greenCard))
	if err == nil || err.Error() != "scope denied" {
		t.Fatalf("want scope-denied error, got %v", err)
	}
	if got := fc.getCalls.Load(); got != int32(cardVerifyMaxAttempts) {
		t.Fatalf("want %d get attempts, got %d", cardVerifyMaxAttempts, got)
	}
}

// TestExtractHeaderTemplate covers both the schema 1.0 layout we send and the
// schema 2.0 wrapper Feishu may return on read-back, plus the empty/headerless
// fallbacks.
func TestExtractHeaderTemplate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", ``, ""},
		{"v1 green", `{"header":{"template":"green"}}`, "green"},
		{"v2 wrapped", `{"data":{"template":{"header":{"template":"red"}}}}`, "red"},
		{"headerless", `{"elements":[]}`, ""},
		{"unparseable", `{not json`, ""},
	}
	for _, c := range cases {
		if got := extractHeaderTemplate([]byte(c.in)); got != c.want {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
}
