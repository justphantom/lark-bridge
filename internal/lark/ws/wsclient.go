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

// serverClientConfig mirrors the ClientConfig JSON the bootstrap endpoint and
// pong payloads carry (whole-second ints). Kept separate from clientConfig so
// "field absent/zero on the wire" stays distinguishable from a real value.
type serverClientConfig struct {
	ReconnectCount    int `json:"ReconnectCount"`
	ReconnectInterval int `json:"ReconnectInterval"`
	ReconnectNonce    int `json:"ReconnectNonce"`
	PingInterval      int `json:"PingInterval"`
}

// minServerInterval is the floor for server-supplied PingInterval /
// ReconnectInterval. A buggy or hostile config below this would hot-loop
// pings or reconnects; zero is NOT clamped here but treated as "keep the
// current value" by merge.
const minServerInterval = 5 * time.Second

// merge applies sc field-wise onto cfg: a zero wire field keeps the current
// value, so a partial pong/bootstrap cannot wipe the reconnect budget
// (ReconnectCount=0 would exhaust it on the next break) or zero the
// intervals (ReconnectInterval=0 + Nonce=0 would hot-loop). Non-zero
// intervals are clamped up to minServerInterval. ReconnectCount=-1 (infinite)
// still overrides, being non-zero.
func (cfg clientConfig) merge(sc serverClientConfig) clientConfig {
	clamp := func(seconds int) time.Duration {
		d := time.Duration(seconds) * time.Second
		if d < minServerInterval {
			return minServerInterval
		}
		return d
	}
	if sc.ReconnectCount != 0 {
		cfg.ReconnectCount = sc.ReconnectCount
	}
	if sc.ReconnectInterval != 0 {
		cfg.ReconnectInterval = clamp(sc.ReconnectInterval)
	}
	if sc.ReconnectNonce != 0 {
		cfg.ReconnectNonce = time.Duration(sc.ReconnectNonce) * time.Second
	}
	if sc.PingInterval != 0 {
		cfg.PingInterval = clamp(sc.PingInterval)
	}
	return cfg
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
			// A non-zero reconnects means we got here AFTER at least one
			// reconnect cycle — i.e. the connection was restored, not the
			// initial dial. OnReconnected fires here (not on the first
			// connect) so the high-level client can re-subscribe / replay
			// state exactly once per recovery.
			if reconnects > 0 {
				c.fireReconnected()
			}
			reconnects = 0 // healthy session resets the per-session budget
			c.runSession(ctx, conn)
			// Only surface OnDisconnected when the connection actually broke.
			// A ctx cancellation (graceful Stop) is an intentional close, not
			// a flap — firing OnDisconnected there would mislead the watchdog
			// and the OnBackendOnline recovery path.
			if ctx.Err() == nil {
				c.fireDisconnected()
			}
		}
		if !c.reconnectStep(ctx, &reconnects) {
			if ctx.Err() != nil {
				return ctx.Err() // intentional cancellation (Stop)
			}
			// Budget exhausted without ctx cancel: return an explicit error so
			// a supervisor (systemd Restart=on-failure) restarts us, instead
			// of the pre-fix silent nil that left the bot dead with no signal.
			return ErrReconnectBudgetExhausted
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
		// Field-wise merge (NOT wholesale overwrite): a partial bootstrap
		// config must not zero the fields it omits — see clientConfig.merge.
		c.cfg = c.cfg.merge(*cfg)
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
// the server's client config (raw wire form; the caller merges it).
func (c *Client) bootstrap(ctx context.Context) (string, *serverClientConfig, error) {
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
			URL          string              `json:"URL"`
			ClientConfig *serverClientConfig `json:"ClientConfig"`
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
	return out.Data.URL, out.Data.ClientConfig, nil
}

// runSession drives the receive and ping loops for one connection. Blocks
// until the conn breaks (read error / close) or ctx is done, then closes the
// conn so both loops unblock and return.
// runSession, receiveLoop, handleControl, handleData, writeAck, pingLoop and
// refreshReadDeadline live in session.go (one-connection session lifecycle).

// reconnectSleep blocks for the configured nonce/interval before the next
// reconnect helpers (reconnectSleep, sleepCtx, fatalError/isFatal, fireXXX,
// snapshotLC/Sink, parseServiceID) live in reconnect.go.
