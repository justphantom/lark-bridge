package feishu

import (
	"context"
	"errors"
	"testing"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
)

// greenCard is the card the verify loop PATCHes. Its body carries a distinctive
// "已发送" markdown element so the content fingerprint can tell it apart from a
// reverted (old) body. Feishu's GET read-back strips the header, so the header
// colour is irrelevant to verification — the elements fingerprint is what
// matters. The send layout keeps a header because the card still needs its
// colour when rendered.
const greenCard = `{"schema":"1.0","config":{"update_multi":true},"header":{"template":"green","title":{"tag":"plain_text","content":"done"}},"elements":[{"tag":"markdown","content":"已发送：x.md"},{"tag":"div","text":{"content":"已完成"}}]}`

// greenReadback mirrors Feishu's actual GET shape: the header is stripped and
// only elements (plus a promoted title) come back. The elements must match the
// sent card for the fingerprint to confirm persistence.
const greenReadback = `{"title":"done","elements":[{"tag":"markdown","content":"已发送：x.md"},{"tag":"div","text":{"content":"已完成"}}]}`

// noVerifyBackoff zeroes the verify-loop backoff for the duration of a test so
// the PATCH→GET→retry loop is instant, then restores the production default.
func noVerifyBackoff(t *testing.T) {
	t.Helper()
	prev := cardVerifyBackoff
	cardVerifyBackoff = 0
	t.Cleanup(func() { cardVerifyBackoff = prev })
}

// TestUpdateCardVerified_HappyPath: the PATCH lands and the read-back elements
// match → one PATCH, one GET, no error. The read-back reorders keys and drops
// the header; the elements fingerprint must match on content alone.
func TestUpdateCardVerified_HappyPath(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{getMessageContent: `{"title":"done","elements":[{"text":{"content":"已完成"},"tag":"div"},{"content":"已发送：x.md","tag":"markdown"}]}`}
	b := &Bot{logger: log.Nop(), client: fc}
	if err := b.UpdateCardVerified(context.Background(), "om_x", "", []byte(greenCard)); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if pc, gc := fc.patchCalls.Load(), fc.getCalls.Load(); pc != 1 || gc != 1 {
		t.Fatalf("want 1 patch + 1 get, got patch=%d get=%d", pc, gc)
	}
}

// TestUpdateCardVerified_RevertRetries: read-back keeps returning the OLD body
// (bounce-back, different elements) → the loop re-PATCHes up to
// cardVerifyMaxAttempts then surfaces ErrCardVerifyMismatch.
func TestUpdateCardVerified_RevertRetries(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{getMessageContent: `{"title":"选择文件","elements":[{"tag":"markdown","content":"请选择"}]}`}
	b := &Bot{logger: log.Nop(), client: fc}
	if err := b.UpdateCardVerified(context.Background(), "om_x", "", []byte(greenCard)); !errors.Is(err, ErrCardVerifyMismatch) {
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
	err := b.UpdateCardVerified(context.Background(), "om_x", "", []byte(greenCard))
	if err == nil || !IsCardGone(err) {
		t.Fatalf("want card-gone error, got %v", err)
	}
	if pc, gc := fc.patchCalls.Load(), fc.getCalls.Load(); pc != 1 || gc != 0 {
		t.Fatalf("want 1 patch + 0 get, got patch=%d get=%d", pc, gc)
	}
}

// TestUpdateCardVerified_BodylessSkipsReadback: a card whose body cannot be
// canonicalized has nothing to fingerprint → trust the PATCH and skip the GET.
func TestUpdateCardVerified_BodylessSkipsReadback(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{}
	b := &Bot{logger: log.Nop(), client: fc}
	if err := b.UpdateCardVerified(context.Background(), "om_x", "", []byte(`{not json`)); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if pc, gc := fc.patchCalls.Load(), fc.getCalls.Load(); pc != 1 || gc != 0 {
		t.Fatalf("unfingerprintable card should skip read-back; got patch=%d get=%d", pc, gc)
	}
}

// TestUpdateCardVerified_GetErrorRetries: a failing read-back (e.g. missing
// im:message:read scope) is retried, then surfaces the last GET error rather
// than silently trusting an unconfirmable PATCH.
func TestUpdateCardVerified_GetErrorRetries(t *testing.T) {
	noVerifyBackoff(t)
	fc := &fakeClient{getMessageErr: errors.New("scope denied")}
	b := &Bot{logger: log.Nop(), client: fc}
	err := b.UpdateCardVerified(context.Background(), "om_x", "", []byte(greenCard))
	if err == nil || err.Error() != "scope denied" {
		t.Fatalf("want scope-denied error, got %v", err)
	}
	if got := fc.getCalls.Load(); got != int32(cardVerifyMaxAttempts) {
		t.Fatalf("want %d get attempts, got %d", cardVerifyMaxAttempts, got)
	}
}

// TestElementsFingerprint covers the schema 1.0 layout we send, Feishu's
// header-stripped read-back, the v2 body wrapper, key-order independence, and
// the empty/unparseable fallbacks.
func TestElementsFingerprint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", ``, ""},
		{"unparseable", `{not json`, ""},
		{"empty elements", `{"elements":[]}`, ""},
		{"v1 root", `{"header":{"template":"green"},"elements":[{"tag":"markdown","content":"hi"}]}`,
			`{"content":"hi","tag":"markdown"}`},
		{"readback headerless", `{"title":"x","elements":[{"tag":"markdown","content":"hi"}]}`,
			`{"content":"hi","tag":"markdown"}`},
		{"v2 body", `{"body":{"elements":[{"tag":"div","text":{"content":"s"}}]}}`,
			`{"tag":"div","text":{"content":"s"}}`},
	}
	for _, c := range cases {
		if got := elementsFingerprint([]byte(c.in)); got != c.want {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
}

// TestElementsFingerprint_KeyOrderIndependent pins that two byte-different but
// semantically identical bodies (reordered keys) fingerprint equal — the basis
// for comparing our sent card against Feishu's reordered read-back.
func TestElementsFingerprint_KeyOrderIndependent(t *testing.T) {
	a := `{"elements":[{"tag":"markdown","content":"hi"},{"tag":"div","text":{"content":"s"}}]}`
	b := `{"elements":[{"content":"hi","tag":"markdown"},{"text":{"content":"s"},"tag":"div"}],"title":"x"}`
	if elementsFingerprint([]byte(a)) != elementsFingerprint([]byte(b)) {
		t.Error("key order / wrapper must not change the fingerprint")
	}
}
