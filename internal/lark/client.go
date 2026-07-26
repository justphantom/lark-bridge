package lark

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/justphantom/lark-bridge/internal/lark/ws"
)

// Option configures a Client at construction time.
type Option func(*config)

type config struct {
	baseURL  string
	logLevel string
}

// WithDomain overrides the Feishu API domain. Accepts the friendly names the
// bridge already uses ("feishu", "larksuite"), a bare host, or a full origin
// URL. Empty defaults to the production Feishu endpoint.
func WithDomain(domain string) Option {
	return func(c *config) { c.baseURL = resolveBaseURL(domain) }
}

// WithLogLevel keeps API parity with the SDK constructor the feishu wrapper
// used to call; the new client does not itself log, so the value is currently
// informational (reserved for a future debug hook).
func WithLogLevel(level string) Option {
	return func(c *config) { c.logLevel = level }
}

// resolveBaseURL maps a friendly/host/URL input to a full origin (scheme+host,
// no trailing slash). Known names cover the two production deployments; a bare
// host is upgraded to https://; a URL with scheme is taken verbatim.
func resolveBaseURL(domain string) string {
	switch strings.ToLower(strings.TrimSpace(domain)) {
	case "", "feishu":
		return "https://open.feishu.cn"
	case "larksuite", "lark":
		return "https://open.larksuite.com"
	}
	if strings.Contains(domain, "://") {
		return strings.TrimRight(domain, "/")
	}
	return "https://" + strings.TrimRight(domain, "/")
}

// Client is the high-level Feishu client: a WebSocket long-poll for inbound
// events/card callbacks plus the three REST endpoints the bridge sends. It
// owns its own context so Stop can interrupt a blocked Start cleanly (the SDK
// could not — Start ended in a bare select{}).
type Client struct {
	rest *restClient
	ws   *ws.Client

	// handler/lc hold the bridge-level Handler and Lifecycle the bridge
	// registered; both are adapted to the ws package's Sink/Lifecycle types
	// before being pushed down, so SetHandler/SetLifecycle can be called in
	// any order relative to NewClient.
	handlerMu sync.Mutex
	handler   Handler
	lcMu      sync.Mutex
	lc        Lifecycle

	startOnce sync.Once
	stopOnce  sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	runErr    error
	runDone   chan struct{}
}

// NewClient constructs a Client. The WebSocket layer is created but not
// connected; Start performs the bootstrap + dial. Send/PatchMessage may be
// called before Start (REST is independent of WS).
func NewClient(appID, appSecret string, opts ...Option) (*Client, error) {
	if appID == "" || appSecret == "" {
		return nil, errors.New("lark: appID and appSecret required")
	}
	cfg := config{baseURL: resolveBaseURL("feishu")}
	for _, o := range opts {
		o(&cfg)
	}
	httpc := newHTTPClient()
	tokens := &tokenManager{appID: appID, appSecret: appSecret, baseURL: cfg.baseURL, http: httpc}
	rest := &restClient{baseURL: cfg.baseURL, http: httpc, tokens: tokens}
	// The WS client receives a nil sink/lifecycle until SetHandler /
	// SetLifecycle wire them; both must be set before Start for the bridge.
	wsc := ws.New(appID, appSecret, cfg.baseURL, httpc)
	return &Client{rest: rest, ws: wsc, handler: HandlerFunc{}, lc: Lifecycle{}}, nil
}

// Start connects the WebSocket and blocks until Stop is called, ctx is
// cancelled, or a fatal (auth) bootstrap error occurs. It is the lark-
// bridge frontend's main goroutine. Safe to call once; a second call returns
// the original termination error.
func (c *Client) Start(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.runDone = make(chan struct{})
	var started bool
	c.startOnce.Do(func() {
		started = true
		c.runErr = c.ws.Start(c.ctx)
		close(c.runDone)
	})
	if !started {
		<-c.runDone
	}
	return c.runErr
}

// Stop signals the WS client to exit and waits for Start to return. Calling
// Stop before Start is a no-op.
func (c *Client) Stop(_ context.Context) error {
	c.stopOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
	})
	if c.runDone != nil {
		<-c.runDone
	}
	return nil
}

// Send creates (or replies to, per ReplyMessageID) a message. Delegates to
// the REST client; independent of the WS connection state.
func (c *Client) Send(ctx context.Context, in *SendInput) (*SendResult, error) {
	return c.rest.SendMessage(ctx, in)
}

// PatchMessage updates an existing message body (used to refresh a card).
func (c *Client) PatchMessage(ctx context.Context, messageID, content string) error {
	return c.rest.PatchMessage(ctx, messageID, content)
}

// SetHandler wires the inbound-event consumer. Must be called before Start;
// the bridge calls it immediately after NewClient.
func (c *Client) SetHandler(h Handler) {
	c.handlerMu.Lock()
	c.handler = h
	c.handlerMu.Unlock()
	c.pushSink()
}

// SetLifecycle wires the connection lifecycle callbacks. Nil fields are no-ops.
func (c *Client) SetLifecycle(lc Lifecycle) {
	c.lcMu.Lock()
	c.lc = lc
	c.lcMu.Unlock()
	c.pushLifecycle()
}

// pushSink adapts the registered lark.Handler into the ws package's Sink
// (converting ws.MessageReceive/CardAction into the public lark types) and
// hands it to the ws client. Called whenever the handler changes.
func (c *Client) pushSink() {
	c.handlerMu.Lock()
	h := c.handler
	c.handlerMu.Unlock()
	if h == nil {
		c.ws.SetSink(nil)
		return
	}
	c.ws.SetSink(handlerSinkAdapter{h: h})
}

// pushLifecycle copies the lark.Lifecycle into the ws-internal copy and hands
// it to the ws client. The two structs differ only in package; the field set
// is identical, so a field-by-field copy is the whole adapter.
func (c *Client) pushLifecycle() {
	c.lcMu.Lock()
	lc := c.lc
	c.lcMu.Unlock()
	c.ws.SetLifecycle(ws.Lifecycle{
		OnReady:        lc.OnReady,
		OnError:        lc.OnError,
		OnReconnecting: lc.OnReconnecting,
		OnReconnected:  lc.OnReconnected,
		OnDisconnected: lc.OnDisconnected,
	})
}

// handlerSinkAdapter bridges lark.Handler (public surface) to ws.Sink (the
// wire-level consumer) by converting ws.MessageReceive/CardAction into the
// lark.* equivalents. The conversion is a flat field copy because the wire
// types and the public types carry the same fields by design.
type handlerSinkAdapter struct{ h Handler }

func (a handlerSinkAdapter) OnMessage(ctx context.Context, ev *ws.MessageReceive) error {
	return a.h.OnMessageReceive(ctx, &MessageReceiveEvent{
		EventID:      ev.EventID,
		MessageID:    ev.MessageID,
		ChatID:       ev.ChatID,
		ChatType:     ev.ChatType,
		MsgType:      ev.MsgType,
		Content:      ev.Content,
		CreateTimeMs: ev.CreateTimeMs,
		SenderOpenID: ev.SenderOpenID,
		Mentions:     convertMentions(ev.Mentions),
	})
}

func (a handlerSinkAdapter) OnCard(ctx context.Context, ev *ws.CardAction) error {
	return a.h.OnCardAction(ctx, &CardActionEvent{
		EventID:   ev.EventID,
		ChatID:    ev.ChatID,
		MessageID: ev.MessageID,
		Operator:  CardActionOperator{OpenID: ev.Operator.OpenID},
		Action: CardActionPayload{
			Value:     ev.Action.Value,
			FormValue: ev.Action.FormValue,
		},
	})
}

func convertMentions(in []ws.Mention) []Mention {
	if len(in) == 0 {
		return nil
	}
	out := make([]Mention, len(in))
	for i, m := range in {
		out[i] = Mention{Key: m.Key, OpenID: m.OpenID, Name: m.Name, IsBot: m.IsBot}
	}
	return out
}
