package lark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark/ws"
)

// TestResolveBaseURL pins the friendly-name/host/URL mapping the feishu
// wrapper's WithDomain option feeds into.
func TestResolveBaseURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "https://open.feishu.cn"},
		{"feishu", "https://open.feishu.cn"},
		{"FEISHU", "https://open.feishu.cn"},
		{"larksuite", "https://open.larksuite.com"},
		{"lark", "https://open.larksuite.com"},
		{"open.feishu.cn", "https://open.feishu.cn"},
		{"https://example.com/", "https://example.com"},
		{"http://127.0.0.1:9000", "http://127.0.0.1:9000"},
	}
	for _, c := range cases {
		if got := resolveBaseURL(c.in); got != c.want {
			t.Errorf("resolveBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNewClient_RequiresCredentials verifies the missing-credential guard
// surfaces before any network activity.
func TestNewClient_RequiresCredentials(t *testing.T) {
	if _, err := NewClient("", "secret"); err == nil {
		t.Fatal("expected error for missing appID")
	}
	if _, err := NewClient("app", ""); err == nil {
		t.Fatal("expected error for missing appSecret")
	}
}

// TestClient_SendDelegatesToREST verifies the public Send path routes through
// the REST client (create vs reply), using a stub HTTP server.
func TestClient_SendDelegatesToREST(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_ = json.NewEncoder(w).Encode(tokenResponse{Code: 0, TenantAccessToken: "t", Expire: 7200})
			return
		}
		seenPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(imResponse{Code: 0, Data: json.RawMessage(`{"message_id":"om_x"}`)})
	}))
	defer srv.Close()

	c, err := NewClient("app", "secret", WithDomain(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := c.Send(context.Background(), &SendInput{ChatID: "oc_c", Text: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID != "om_x" {
		t.Fatalf("MessageID = %q", res.MessageID)
	}
	if seenPath != "/open-apis/im/v1/messages" {
		t.Errorf("path = %q", seenPath)
	}
}

// captureHandler records what the adapter delivered.
type captureHandler struct {
	gotMsg  *MessageReceiveEvent
	gotCard *CardActionEvent
}

func (h *captureHandler) OnMessageReceive(_ context.Context, ev *MessageReceiveEvent) error {
	h.gotMsg = ev
	return nil
}
func (h *captureHandler) OnCardAction(_ context.Context, ev *CardActionEvent) ([]byte, error) {
	h.gotCard = ev
	return nil, nil
}

// TestHandlerSinkAdapter_ConvertsMessage verifies the ws.MessageReceive →
// lark.MessageReceiveEvent conversion at the adapter seam (the place where a
// field-drop would silently lose data the bridge depends on).
func TestHandlerSinkAdapter_ConvertsMessage(t *testing.T) {
	h := &captureHandler{}
	a := handlerSinkAdapter{h: h}
	in := &ws.MessageReceive{
		EventID: "evt_1", MessageID: "om_1", ChatID: "oc_1", ChatType: "group",
		MsgType: "text", Content: `{"text":"hi"}`, CreateTimeMs: 1234,
		SenderOpenID: "ou_s",
		Mentions: []ws.Mention{
			{Key: "@_user_1", Name: "Alice", OpenID: "ou_a"},
			{Key: "@_user_2", IsBot: true},
		},
	}
	if err := a.OnMessage(context.Background(), in); err != nil {
		t.Fatalf("OnMessage: %v", err)
	}
	if h.gotMsg == nil {
		t.Fatal("not delivered")
	}
	g := h.gotMsg
	if g.EventID != "evt_1" || g.MessageID != "om_1" || g.ChatID != "oc_1" {
		t.Errorf("identity lost: %+v", g)
	}
	if g.ChatType != "group" || g.MsgType != "text" || g.Content != `{"text":"hi"}` {
		t.Errorf("content lost: %+v", g)
	}
	if g.CreateTimeMs != 1234 || g.SenderOpenID != "ou_s" {
		t.Errorf("meta lost: %+v", g)
	}
	if len(g.Mentions) != 2 || g.Mentions[0].Name != "Alice" || !g.Mentions[1].IsBot {
		t.Errorf("mentions lost: %+v", g.Mentions)
	}
}

// TestHandlerSinkAdapter_ConvertsCard verifies the ws.CardAction →
// lark.CardActionEvent conversion preserves operator + form value.
func TestHandlerSinkAdapter_ConvertsCard(t *testing.T) {
	h := &captureHandler{}
	a := handlerSinkAdapter{h: h}
	in := &ws.CardAction{
		EventID: "evt_c", ChatID: "oc_c", MessageID: "om_c",
		Operator: ws.CardActionOperator{OpenID: "ou_op"},
		Action: ws.CardActionPayload{
			Value:     map[string]any{"k": "v"},
			FormValue: map[string]any{"q": "a"},
		},
	}
	if _, err := a.OnCard(context.Background(), in); err != nil {
		t.Fatalf("OnCard: %v", err)
	}
	if h.gotCard == nil {
		t.Fatal("card not delivered")
	}
	c := h.gotCard
	if c.EventID != "evt_c" || c.ChatID != "oc_c" || c.MessageID != "om_c" {
		t.Errorf("identity lost: %+v", c)
	}
	if c.Operator.OpenID != "ou_op" {
		t.Errorf("operator lost: %+v", c.Operator)
	}
	if c.Action.Value["k"] != "v" || c.Action.FormValue["q"] != "a" {
		t.Errorf("action payload lost: %+v", c.Action)
	}
}

// TestClient_StopBeforeStartIsNoOp verifies calling Stop without Start returns
// promptly without panicking.
func TestClient_StopBeforeStartIsNoOp(t *testing.T) {
	c, err := NewClient("app", "secret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = c.Stop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop without Start blocked")
	}
}

// TestClient_StartDoubleCallNoDeadlock pins the §3.3 fix: calling Start a
// second time (a programming error) must not deadlock on a fresh never-closed
// runDone channel. Both callers receive the same run result once the single
// underlying ws.Start returns.
func TestClient_StartDoubleCallNoDeadlock(t *testing.T) {
	c, err := NewClient("app", "secret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 2)
	for range 2 {
		go func() { errCh <- c.Start(ctx) }()
	}
	// Let both callers park on <-c.runDone, then unblock via cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	for i := range 2 {
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("Start caller #%d deadlocked", i)
		}
	}
}

// keep imports used
var _ = time.Second
