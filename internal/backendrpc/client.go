// Package backendrpc is the shared IPC client both backends use to talk to
// the frontend: it long-connects to the frontend SSE endpoint to read
// protocol.Event, and POSTs protocol.Control back.
//
// Reconnect policy: Connect opens one SSE stream and surfaces an error from
// RecvEvent when that stream ends (frontend restart, idle timeout, network
// blip). Run wraps Connect+RecvEvent with bounded exponential backoff so a
// transient disconnect does not terminate the backend.
package backendrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// handshakeTimeout bounds the SSE GET + POST dial/header wait. Without it a
// stalled frontend makes Connect or SendControl block indefinitely.
const handshakeTimeout = 10 * time.Second

// sendControlTimeout is the fallback deadline SendControl applies when the
// caller's ctx has none, so a wedged frontend cannot pin the emit goroutine.
const sendControlTimeout = 15 * time.Second

// statusQueryTimeout is the fallback deadline Status applies when the caller's
// ctx has none, so a wedged frontend cannot block a /running query.
const statusQueryTimeout = 5 * time.Second

// sseEventChanBuf is the buffer for the backend's inbound SSE Event channel. The
// frontend pushes Events here for the bridge to drain; a full channel surfaces
// backpressure rather than unbounded queuing.
const sseEventChanBuf = 256

// SSE scanner buffer bounds. scannerInitBuf is the starting allocation;
// scannerMaxLine is the per-frame ceiling (rich prompts can be large).
const (
	scannerInitBuf = 64 << 10
	scannerMaxLine = 1 << 20
)

// maxErrBody caps how much of a peer's non-2xx response body is read into an
// error message. The frontend writes only short fixed strings via http.Error,
// but a bounded read keeps a misbehaving peer from OOM-ing the backend on an
// oversized error response.
const maxErrBody = 4 << 10

// newHTTPClient returns a client suitable for both SSE streaming and POST.
// A single overall Timeout would kill idle SSE long-polls, so instead we cap
// dial and header-read latency via the Transport and apply per-request
// deadlines at the call sites (SendControl takes a ctx; the SSE handshake
// uses a dedicated connect ctx).
func newHTTPClient() *http.Client {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Fallback to the default transport untouched if the assertion ever
		// breaks (e.g. someone wrapped DefaultTransport). Header timeouts
		// then come from net/http defaults.
		return &http.Client{}
	}
	tr.ResponseHeaderTimeout = handshakeTimeout
	return &http.Client{Transport: tr}
}

// Client is one backend's connection to the frontend.
type Client struct {
	backendID   string
	backendType string
	frontendURL string
	secret      string // shared bearer token; sent on SSE and POST
	httpClient  *http.Client
	eventCh     chan *protocol.Event
	closeCh     chan struct{}
	closed      int32

	// sseBody is the SSE response body; Close closes it so readSSE unblocks
	// instead of hanging forever on scanner.Scan() when the client shuts
	// down locally (vs. a server-side EOF, which readSSE already handles).
	sseBody io.ReadCloser

	// logger surfaces unreadable SSE frames (oversized or malformed) so a
	// dropped event is observable. Defaults to a no-op; main.go wires the
	// real one via SetLogger. atomic so SetLogger (called by main.go after
	// Connect already spawned readSSE) synchronises with the reader without a
	// bare-field data race.
	logger atomic.Pointer[log.Logger]
}

// SetLogger wires the component logger. Called by main.go after Connect; nil is
// rejected so readSSE always has a usable logger.
func (c *Client) SetLogger(l *log.Logger) {
	if l != nil {
		c.logger.Store(l)
	}
}

// Connect opens an SSE connection to the frontend. secret is the shared bearer
// token the frontend validates; pass "" only for a loopback-only frontend
// with no auth configured.
func Connect(backendID, backendType, frontendURL, secret string) (*Client, error) {
	return ConnectWithHTTPClient(backendID, backendType, frontendURL, secret, newHTTPClient())
}

// ConnectWithHTTPClient opens an SSE connection using the given HTTP client
// (lets tests inject a transport). The handshake must succeed (HTTP 200); on
// success the client spawns a goroutine that reads SSE frames into RecvEvent.
func ConnectWithHTTPClient(backendID, backendType, frontendURL, secret string, httpClient *http.Client) (*Client, error) {
	if backendID == "" || backendType == "" || frontendURL == "" {
		return nil, fmt.Errorf("backendID/backendType/frontendURL required")
	}
	c := &Client{
		backendID:   backendID,
		backendType: backendType,
		frontendURL: strings.TrimSuffix(frontendURL, "/"),
		secret:      secret,
		httpClient:  httpClient,
		eventCh:     make(chan *protocol.Event, sseEventChanBuf),
		closeCh:     make(chan struct{}),
	}
	c.logger.Store(log.Nop())
	u, err := url.Parse(c.frontendURL)
	if err != nil {
		return nil, err
	}
	u.Path = u.Path + "/v1/events"
	q := u.Query()
	q.Set("backendID", backendID)
	q.Set("backendType", backendType)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.setAuth(req)
	// The Transport's ResponseHeaderTimeout bounds the handshake; we do NOT
	// attach a deadline-carrying context to the request because that context
	// also governs the response body — cancelling it after Connect returns
	// would close the SSE stream.
	resp, err := httpClient.Do(req) //nolint:gosec // G704: frontendURL is trusted config, not user input
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("sse handshake %d: %s", resp.StatusCode, body)
	}
	c.sseBody = resp.Body
	go c.readSSE(resp.Body)
	return c, nil
}

// readSSE consumes the SSE body line by line, reassembling `data: <json>`
// frames (which may span multiple data: lines) and dispatching decoded
// protocol.Event into eventCh. On EOF/error it closes the client.
func (c *Client) readSSE(r io.ReadCloser) {
	defer func() { _ = r.Close() }() // body drain done; close error is not actionable
	scanner := bufio.NewScanner(r)
	// Frames can be large (rich prompts); raise the per-line cap.
	scanner.Buffer(make([]byte, 0, scannerInitBuf), scannerMaxLine)
	var payload []byte
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Blank line terminates an event block.
			if len(payload) > 0 {
				var ev protocol.Event
				if err := json.Unmarshal(payload, &ev); err == nil {
					if verr := ev.Validate(); verr != nil {
						// Decoded but structurally invalid (missing
						// promptID, unknown type, …). Mirrors the
						// unreadable-frame warning so a malformed
						// event is observable, not silently dropped.
						c.logger.Load().Warn("invalid sse event",
							log.FieldError, verr,
							"bytes", len(payload))
					} else {
						select {
						case c.eventCh <- &ev:
						case <-c.closeCh:
							return
						}
					}
				} else {
					// An unreadable frame (oversized past scannerMaxLine, or
					// corrupt) would otherwise vanish silently. Surface it so
					// a dropped prompt/control is observable.
					c.logger.Load().Warn("parse sse event",
						log.FieldError, err,
						"bytes", len(payload))
				}
				payload = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			if len(payload) > 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, []byte(strings.TrimPrefix(line, "data: "))...)
		}
	}
	// A frame larger than scannerMaxLine makes Scan return false with a
	// bufio.ErrTooLong error that is otherwise indistinguishable from a clean
	// EOF — surface it so a dropped oversized prompt is observable.
	if err := scanner.Err(); err != nil {
		c.logger.Load().Warn("sse scanner", log.FieldError, err)
	}
	_ = c.Close() // SSE goroutine ending; close error not actionable
}

// setAuth adds the shared bearer token to req when a secret is configured.
func (c *Client) setAuth(req *http.Request) {
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
}

// RecvEvent blocks until the next Event arrives or the client is closed.
func (c *Client) RecvEvent() (*protocol.Event, error) {
	select {
	case ev := <-c.eventCh:
		return ev, nil
	case <-c.closeCh:
		return nil, fmt.Errorf("client closed")
	}
}

// SendControl POSTs a Control to the frontend. Validates first; a Control
// whose BackendID is empty is acceptable (the frontend backfills it). When ctx
// has no deadline, SendControl wraps it with sendControlTimeout so a stalled
// frontend cannot block the bridge's emit path indefinitely.
func (c *Client) SendControl(ctx context.Context, ctrl *protocol.Control) error {
	if err := ctrl.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(ctrl)
	if err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sendControlTimeout)
		defer cancel()
	}
	url := fmt.Sprintf("%s/v1/control/%s", c.frontendURL, c.backendID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: frontendURL is trusted config, not user input
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() // control POST fire-and-forget
	if resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return fmt.Errorf("send control %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// Status GETs /v1/status from the frontend, returning the in-flight turn
// snapshot. Used by backends that must observe frontend-owned turn state (e.g.
// deploy-monitor's /running). When ctx has no deadline it is bounded by
// statusQueryTimeout so a stalled frontend cannot block the caller.
func (c *Client) Status(ctx context.Context) (*protocol.StatusSnapshot, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, statusQueryTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.frontendURL+"/v1/status", nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: frontendURL is trusted config, not user input
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, respBody)
	}
	var snap protocol.StatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}
	return &snap, nil
}

// Close shuts the client down. Idempotent. Closes the SSE body so the read
// goroutine unblocks in addition to releasing RecvEvent callers.
func (c *Client) Close() error {
	if atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		if c.sseBody != nil {
			_ = c.sseBody.Close()
		}
		close(c.closeCh)
	}
	return nil
}
