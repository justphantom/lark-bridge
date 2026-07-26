package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark/websocket"
)

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
// cancelled or a fatal bootstrap error (bad credentials, forbidden) occurs;
// transient network errors trigger reconnection per cfg.
func (c *Client) Start(ctx context.Context) error {
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
			if !c.reconnectSleep(ctx) {
				return ctx.Err()
			}
			continue
		}
		c.fireReady()
		c.runSession(ctx, conn)
		c.fireDisconnected()
		if !c.reconnectSleep(ctx) {
			return ctx.Err()
		}
	}
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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
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
func (c *Client) runSession(ctx context.Context, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }
	go func() {
		defer finish()
		c.receiveLoop(ctx, conn)
	}()
	go func() {
		defer finish()
		c.pingLoop(ctx, conn)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	_ = conn.Close()
	<-done
}

// receiveLoop reads frames until the conn errors or ctx is done. Each inbound
// data frame is reassembled, routed, and ACK'd; each pong refreshes the config.
//
// A read deadline is reset after every successfully read frame to 2× the
// configured ping interval. The lark server replies to each ping with a pong
// (or sends events) well within that window; if nothing arrives for two ping
// cycles the connection is half-open (peer host crashed / network split) and
// the read returns a deadline error → this loop exits → runSession reconnects.
// Without this the original SDK's gorilla SetReadDeadline analogue is gone and
// a silent death would otherwise only surface via the 5-minute frontend
// watchdog, dropping every event in between.
func (c *Client) receiveLoop(ctx context.Context, conn *websocket.Conn) {
	reassembly := newReassembler()
	rt := &router{sink: c.snapshotSink()}
	sweepTicker := time.NewTicker(chunkTTL)
	defer sweepTicker.Stop()
	c.refreshReadDeadline(conn)
	for {
		if ctx.Err() != nil {
			return
		}
		op, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		// Any frame (data, pong, even a stray ping) proves the peer is alive:
		// push the deadline out by another 2× ping interval.
		c.refreshReadDeadline(conn)
		if op != websocket.OpcodeBinary {
			continue
		}
		var frame Frame
		if err := frame.Unmarshal(data); err != nil {
			c.fireError(fmt.Errorf("ws: unmarshal frame: %w", err))
			continue
		}
		switch frame.Method {
		case MethodControl:
			c.handleControl(frame)
		case MethodData:
			c.handleData(ctx, frame, reassembly, rt, conn)
		}
		select {
		case <-sweepTicker.C:
			reassembly.sweep(chunkTTL)
		default:
		}
	}
}

// refreshReadDeadline sets the conn read deadline to now + 2×PingInterval. A
// non-positive PingInterval (misconfigured/bootstrap not yet applied) falls
// back to a safe default so the deadline is always armed.
func (c *Client) refreshReadDeadline(conn *websocket.Conn) {
	c.mu.Lock()
	interval := c.cfg.PingInterval
	c.mu.Unlock()
	if interval <= 0 {
		interval = 90 * time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * interval))
}

// handleControl responds to pong (config refresh). The lark server sends
// pongs in response to our pings, optionally carrying an updated ClientConfig
// in the payload; we ingest it so the server can steer ping/reconnect cadence
// without dropping the connection. Server→client pings do not occur in this
// protocol, so nothing else is handled here.
func (c *Client) handleControl(frame Frame) {
	hs := Headers(frame.Headers)
	if hs.GetString(HeaderType) != TypePong {
		return
	}
	if len(frame.Payload) == 0 {
		return
	}
	var cc struct {
		ReconnectCount    int `json:"ReconnectCount"`
		ReconnectInterval int `json:"ReconnectInterval"`
		ReconnectNonce    int `json:"ReconnectNonce"`
		PingInterval      int `json:"PingInterval"`
	}
	if err := json.Unmarshal(frame.Payload, &cc); err != nil {
		return
	}
	c.mu.Lock()
	c.cfg = clientConfig{
		ReconnectCount:    cc.ReconnectCount,
		ReconnectInterval: time.Duration(cc.ReconnectInterval) * time.Second,
		ReconnectNonce:    time.Duration(cc.ReconnectNonce) * time.Second,
		PingInterval:      time.Duration(cc.PingInterval) * time.Second,
	}
	c.mu.Unlock()
}

// handleData reassembles a data frame, routes it, and writes the ACK. ACKing is
// mandatory: the lark server treats an unacked delivery as a failure and
// redelivers, which the upstream dedup then has to suppress.
func (c *Client) handleData(ctx context.Context, frame Frame, r *reassembler, rt *router, conn frameWriter) {
	hs := Headers(frame.Headers)
	msgID := hs.GetString(HeaderMessageID)
	sum := hs.GetInt(HeaderSum)
	seq := hs.GetInt(HeaderSeq)
	joined, ok := r.feed(msgID, sum, seq, frame.Payload)
	if !ok {
		// Still waiting on more chunks. Do NOT ack — the server will send the
		// remaining pieces; partial ack would confuse the redelivery state.
		return
	}
	if rt.sink == nil {
		c.writeAck(conn, frame, http.StatusOK)
		return
	}
	ackCode := http.StatusOK
	if err := rt.dispatch(ctx, joined); err != nil {
		c.fireError(fmt.Errorf("ws: dispatch %s: %w", hs.GetString(HeaderType), err))
		ackCode = http.StatusInternalServerError
	}
	c.writeAck(conn, frame, ackCode)
}

// frameWriter is the write seam of a websocket.Conn so handleData can be
// tested with a fake. *websocket.Conn satisfies it.
type frameWriter interface {
	WriteMessage(opcode int, data []byte) error
}

// writeAck builds and sends the ACK frame carrying {code: ackCode} payload.
func (c *Client) writeAck(conn frameWriter, in Frame, ackCode int) {
	payload, _ := json.Marshal(map[string]int{"code": ackCode})
	ack := NewAckFrame(in, payload)
	bs, err := ack.Marshal()
	if err != nil {
		return
	}
	if err := conn.WriteMessage(websocket.OpcodeBinary, bs); err != nil {
		c.fireError(fmt.Errorf("ws: write ack: %w", err))
	}
}

// pingLoop sends a protobuf ping frame every PingInterval. A write error ends
// the session (the receive loop will also surface the broken conn).
func (c *Client) pingLoop(ctx context.Context, conn *websocket.Conn) {
	c.mu.Lock()
	interval := c.cfg.PingInterval
	serviceID := c.serviceID
	c.mu.Unlock()
	if interval <= 0 {
		interval = 90 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frame := NewPingFrame(serviceID)
			bs, err := frame.Marshal()
			if err != nil {
				continue
			}
			if err := conn.WriteMessage(websocket.OpcodeBinary, bs); err != nil {
				return
			}
		}
	}
}

// reconnectSleep blocks for the configured nonce/interval before the next
// reconnect attempt. Returns false if ctx was cancelled during the wait.
func (c *Client) reconnectSleep(ctx context.Context) bool {
	c.mu.Lock()
	nonce := c.cfg.ReconnectNonce
	interval := c.cfg.ReconnectInterval
	c.mu.Unlock()
	c.fireReconnecting()
	if nonce > 0 {
		// math/rand/v2 is intentional: the jitter is a reconnect-backoff
		// spread, not a security primitive — crypto/rand would add cost for
		// no benefit (an attacker observing reconnect timing learns nothing
		// of value from the backoff distribution).
		jitter := time.Duration(rand.Int64N(int64(nonce))) //nolint:gosec // G404: reconnect-backoff jitter, not a security primitive.
		if !sleepCtx(ctx, jitter) {
			return false
		}
	}
	return sleepCtx(ctx, interval)
}

// sleepCtx sleeps for d but aborts early (returning false) when ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// fatalError marks an error as non-retryable (auth/credentials). Start returns
// it immediately instead of looping forever.
type fatalError struct{ error }

func (fatalError) IsFatal() {}

// isFatal reports whether the bootstrap/connect path should stop retrying.
func isFatal(err error) bool {
	var f interface{ IsFatal() }
	return errors.As(err, &f)
}

// fireXXX helpers invoke the lifecycle callback if set. They snapshot lc
// under the lock so a post-Start SetLifecycle is safe to call (the contract
// says "before Start", but we stay race-clean regardless).
func (c *Client) fireReady() {
	lc := c.snapshotLC()
	if lc.OnReady != nil {
		lc.OnReady()
	}
}
func (c *Client) fireError(err error) {
	lc := c.snapshotLC()
	if lc.OnError != nil {
		lc.OnError(err)
	}
}
func (c *Client) fireReconnecting() {
	lc := c.snapshotLC()
	if lc.OnReconnecting != nil {
		lc.OnReconnecting()
	}
}
func (c *Client) fireDisconnected() {
	lc := c.snapshotLC()
	if lc.OnDisconnected != nil {
		lc.OnDisconnected()
	}
}

// snapshotLC returns a value copy of the current lifecycle.
func (c *Client) snapshotLC() Lifecycle {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lc
}

// snapshotSink returns the current sink.
func (c *Client) snapshotSink() Sink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sink
}

// parseServiceID extracts the service_id query param as int32 (seeds pings).
func parseServiceID(rawURL string) int32 {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(u.Query().Get(queryServiceID), 10, 32)
	if err != nil {
		return 0
	}
	return int32(v)
}
