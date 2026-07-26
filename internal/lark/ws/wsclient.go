package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark/websocket"
)

// maxBootstrapBodyBytes caps the WS bootstrap response read. The legit body
// is a small JSON blob; the bound defends against a hostile/buggy endpoint
// streaming a large body to exhaust memory.
const maxBootstrapBodyBytes = 1 << 20 // 1 MiB

// Lifecycle is the ws-level copy of lark.Lifecycle (callback fields only). It
// is duplicated here so ws does not import the parent lark package and create
// a cycle. The high-level lark.Client converts between the two at the seam.
type Lifecycle struct {
	OnReady        func()
	OnError        func(error)
	OnReconnecting func()
	OnReconnected  func()
	OnDisconnected func()
}

// Client owns the lark WebSocket lifecycle: bootstrap → dial → ping loop +
// receive loop → reconnect. Unlike the SDK's Start (which ends in a bare
// select{} and leaks its goroutine forever), Start here blocks but returns
// promptly when ctx is cancelled or a fatal (auth) bootstrap error occurs.
type Client struct {
	appID     string
	appSecret string
	baseURL   string // bootstrap origin, e.g. "https://open.feishu.cn"
	http      httpClient
	lc        Lifecycle
	sink      Sink

	// dialer is the WS transport seam; defaults to websocket.Dial. Tests
	// inject a fake that returns an in-memory conn pair.
	dialer dialer

	// serviceID seeds the protobuf ping frame's Service field; refreshed
	// from each bootstrap URL's service_id query param.
	mu        sync.Mutex
	serviceID int32
	cfg       clientConfig
}

// httpClient is the bootstrap HTTP seam (*http.Client satisfies it).
type httpClient interface {
	Do(*http.Request) (*http.Response, error)
}

// dialer abstracts websocket.Dial for testing.
type dialer func(ctx context.Context, rawURL string, header http.Header) (*websocket.Conn, *http.Response, error)

// clientConfig carries the server-supplied connection parameters (units
// converted to time.Duration at parse time so callers never re-derive).
type clientConfig struct {
	ReconnectCount    int           // -1 = infinite
	ReconnectInterval time.Duration // between reconnect attempts
	ReconnectNonce    time.Duration // max initial jitter before first retry
	PingInterval      time.Duration // between ping frames
}

// defaultConfig matches the values observed from the production bootstrap
// endpoint; used until the first bootstrap response overrides them.
func defaultConfig() clientConfig {
	return clientConfig{
		ReconnectCount:    -1,
		ReconnectInterval: 90 * time.Second,
		ReconnectNonce:    25 * time.Second,
		PingInterval:      90 * time.Second,
	}
}

// newWSClient constructs a client with the given credentials and lifecycle.
// sink may be nil (events are ACK'd but not delivered) — the feishu wrapper
// always sets one.
func newWSClient(appID, appSecret, baseURL string, httpc httpClient, lc Lifecycle, sink Sink) *Client {
	return &Client{
		appID:     appID,
		appSecret: appSecret,
		baseURL:   baseURL,
		http:      httpc,
		lc:        lc,
		sink:      sink,
		dialer:    websocket.Dial,
		cfg:       defaultConfig(),
	}
}

// New is the exported constructor the high-level lark.Client calls. Sink and
// Lifecycle are wired post-construction via SetSink/SetLifecycle, so both
// start zero-valued (nil-safe).
func New(appID, appSecret, baseURL string, httpc httpClient) *Client {
	return newWSClient(appID, appSecret, baseURL, httpc, Lifecycle{}, nil)
}

// SetSink wires the inbound-event consumer. Must be called before Start.
func (c *Client) SetSink(s Sink) {
	c.mu.Lock()
	c.sink = s
	c.mu.Unlock()
}

// SetLifecycle wires the connection lifecycle callbacks. Nil fields are no-ops.
func (c *Client) SetLifecycle(lc Lifecycle) {
	c.mu.Lock()
	c.lc = lc
	c.mu.Unlock()
}

// Start runs the connect→session→reconnect loop. It returns only when ctx is
// cancelled, a fatal bootstrap error (bad credentials, forbidden) occurs, or
// the server-supplied reconnect budget is exhausted; transient network errors
// trigger reconnection per cfg.
//
// ReconnectCount == -1 means infinite retries (the production server default).
// >= 0 bounds total reconnect attempts per successful session; a successful
// session resets the budget, matching the SDK's per-reconnect-call semantics.
func (c *Client) Start(ctx context.Context) error {
	var reconnects int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := c.connect(ctx)
		if err != nil {
			if isFatal(err) {
				c.fireError(err)
				return err
			}
			c.fireError(err)
		} else {
			c.fireReady()
			reconnects = 0 // healthy session resets the per-session budget
			c.runSession(ctx, conn)
			c.fireDisconnected()
		}
		if !c.reconnectStep(ctx, &reconnects) {
			return ctx.Err()
		}
	}
}

// reconnectStep enforces the ReconnectCount budget then sleeps the nonce/
// interval before the next attempt. Returns false if ctx was cancelled OR the
// budget is exhausted; the caller distinguishes by re-checking ctx.Err().
func (c *Client) reconnectStep(ctx context.Context, reconnects *int) bool {
	c.mu.Lock()
	budget := c.cfg.ReconnectCount
	c.mu.Unlock()
	if budget >= 0 && *reconnects >= budget {
		return false // budget exhausted — Start returns
	}
	*reconnects++
	return c.reconnectSleep(ctx)
}

// connect performs one bootstrap + dial cycle. On success the conn is live and
// the serviceID/cfg fields are refreshed. Returns a fatal error for bad
// credentials so the caller does not retry forever.
func (c *Client) connect(ctx context.Context) (*websocket.Conn, error) {
	wsURL, cfg, err := c.bootstrap(ctx)
	if err != nil {
		return nil, err
	}
	serviceID := parseServiceID(wsURL)
	c.mu.Lock()
	c.serviceID = serviceID
	if cfg != nil {
		c.cfg = *cfg
	}
	c.mu.Unlock()
	// Dial closes resp.Body itself (see its doc); capture the response so the
	// bodyclose analyser sees it handled, then close defensively (idempotent).
	conn, resp, err := c.dialer(ctx, wsURL, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("ws: dial: %w", err)
	}
	return conn, nil
}

// bootstrap posts AppID/AppSecret to the endpoint and returns the WS URL plus
// the server's client config.
func (c *Client) bootstrap(ctx context.Context) (string, *clientConfig, error) {
	body, _ := json.Marshal(map[string]string{"AppID": c.appID, "AppSecret": c.appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+BootstrapEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Locale", "zh") // canonical form; the bootstrap server accepts either casing
	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("ws: bootstrap: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("ws: bootstrap http %d", resp.StatusCode)
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			URL          string `json:"URL"`
			ClientConfig *struct {
				ReconnectCount    int `json:"ReconnectCount"`
				ReconnectInterval int `json:"ReconnectInterval"`
				ReconnectNonce    int `json:"ReconnectNonce"`
				PingInterval      int `json:"PingInterval"`
			} `json:"ClientConfig"`
		} `json:"data"`
	}
	// Bound the decode: the bootstrap body is a small JSON blob; a hostile
	// or buggy endpoint returning a huge body must not exhaust memory.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBootstrapBodyBytes)).Decode(&out); err != nil {
		return "", nil, fmt.Errorf("ws: bootstrap decode: %w", err)
	}
	if out.Code != codeOK {
		// 514 = auth failure: do not retry forever.
		if out.Code == codeAuthFailed {
			return "", nil, fatalError{fmt.Errorf("ws: bootstrap auth failed: %s", out.Msg)}
		}
		return "", nil, fmt.Errorf("ws: bootstrap code %d: %s", out.Code, out.Msg)
	}
	if out.Data.URL == "" {
		return "", nil, errors.New("ws: bootstrap returned empty URL")
	}
	var cfg *clientConfig
	if out.Data.ClientConfig != nil {
		cc := out.Data.ClientConfig
		cfg = &clientConfig{
			ReconnectCount:    cc.ReconnectCount,
			ReconnectInterval: time.Duration(cc.ReconnectInterval) * time.Second,
			ReconnectNonce:    time.Duration(cc.ReconnectNonce) * time.Second,
			PingInterval:      time.Duration(cc.PingInterval) * time.Second,
		}
	}
	return out.Data.URL, cfg, nil
}

// runSession drives the receive and ping loops for one connection. Blocks
// until the conn breaks (read error / close) or ctx is done, then closes the
// conn so both loops unblock and return.
// runSession, receiveLoop, handleControl, handleData, writeAck, pingLoop and
// refreshReadDeadline live in session.go (one-connection session lifecycle).

// reconnectSleep blocks for the configured nonce/interval before the next
// reconnect helpers (reconnectSleep, sleepCtx, fatalError/isFatal, fireXXX,
// snapshotLC/Sink, parseServiceID) live in reconnect.go.
