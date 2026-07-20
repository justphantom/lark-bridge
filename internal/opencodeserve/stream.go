package opencodeserve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// subscriberBuf bounds the per-session event channel. Large enough that a
// brief consumer stall does not drop events; small enough that a forgotten
// unsubscribe cannot leak megabytes.
const subscriberBuf = 64

// reconnBackoff is the delay between SSE reconnect attempts. opencode serve
// closes the connection on shutdown and we want to ride a restart without
// losing in-flight turns (their next prompt re-creates a session).
const reconnBackoff = 2 * time.Second

// sseLineLimit bounds the per-line SSE scanner buffer. The opencode serve
// frames are tiny (heartbeat, part deltas) — 1 MiB is a generous ceiling.
const sseLineLimit = 1 << 20

// abortTimeout bounds the POST /abort call when a Run's ctx is cancelled.
// abort is fire-and-forget; if it stalls we do not want to wedge the
// goroutine that has already given up on the turn.
const abortTimeout = 5 * time.Second

// RunOptions describes one agent turn against the serve server. Mirrors
// opencode.RunOptions so the bridge stays mode-agnostic at the call site.
type RunOptions struct {
	// Prompt is sent as the user message text.
	Prompt string
	// SessionID, when non-empty, continues an existing session. Empty
	// triggers POST /session to create one; the id arrives in the
	// session.created event and the bridge persists it for the next turn.
	SessionID string
	// Model optionally overrides the configured model ("provider/model"
	// form). Forwarded as providerID/modelID in the message body.
	Model string
	// Agent optionally overrides the configured agent.
	Agent string
	// Directory sets the session's working directory. Only honoured on
	// initial session creation (opencode serve binds the cwd at session
	// start). Subsequent turns on an existing session keep the original.
	Directory string
	// LineSink receives every raw SSE data line verbatim before parsing.
	// Optional; nil disables teeing (the serve stream is already archived
	// at the bridge layer if configured).
	LineSink io.Writer
}

// sseDispatcher owns the global GET /event subscription and routes frames
// per-session to subscribed Run callers. A single long-lived goroutine
// (started by run) reconnects on transport error with reconnBackoff.
type sseDispatcher struct {
	baseURL    string
	httpClient *http.Client

	logger atomicLogger

	mu     sync.Mutex
	subs   map[string]chan Event
	stopCh chan struct{}
	done   chan struct{}
}

// atomicLogger wraps an atomic pointer so SetLogger is race-free against the
// dispatcher goroutine's reads. Stored as a concrete type to avoid a separate
// import of sync/atomic in this file's name space.
type atomicLogger struct {
	v atomic.Value // *log.Logger
}

func (a *atomicLogger) load() *log.Logger {
	if v, ok := a.v.Load().(*log.Logger); ok && v != nil {
		return v
	}
	return log.Nop()
}

func (a *atomicLogger) store(l *log.Logger) {
	if l != nil {
		a.v.Store(l)
	}
}

func newSSEDispatcher(baseURL string, httpClient *http.Client, logger *log.Logger) *sseDispatcher {
	d := &sseDispatcher{
		baseURL:    baseURL,
		httpClient: httpClient,
		subs:       make(map[string]chan Event),
		stopCh:     make(chan struct{}),
		done:       make(chan struct{}),
	}
	d.logger.store(logger)
	return d
}

func (d *sseDispatcher) setLogger(l *log.Logger) { d.logger.store(l) }

// subscribe reserves a slot for sessionID and returns the channel that will
// receive its events. Always buffered; caller drains until closed.
func (d *sseDispatcher) subscribe(sessionID string) chan Event {
	ch := make(chan Event, subscriberBuf)
	d.mu.Lock()
	// If a previous subscriber exists (rare — a stale Run that did not
	// unsubscribe), close it so its goroutine returns rather than blocks.
	if old, ok := d.subs[sessionID]; ok {
		close(old)
	}
	d.subs[sessionID] = ch
	d.mu.Unlock()
	return ch
}

// unsubscribe removes and closes the subscriber for sessionID. Idempotent.
func (d *sseDispatcher) unsubscribe(sessionID string, ch chan Event) {
	d.mu.Lock()
	if cur, ok := d.subs[sessionID]; ok && cur == ch {
		delete(d.subs, sessionID)
		close(ch)
	}
	d.mu.Unlock()
}

// stop signals the dispatcher goroutine to exit and blocks until it has.
func (d *sseDispatcher) stop() {
	select {
	case <-d.stopCh:
		// already stopped
	default:
		close(d.stopCh)
	}
	<-d.done
}

// run is the dispatcher goroutine. Maintains a single SSE connection to
// /event; on transport error or server-initiated close, reconnects after
// reconnBackoff. Returns when stop() is called.
func (d *sseDispatcher) run() {
	defer close(d.done)
	logger := d.logger.load()
	logger.Debug("sse dispatcher started", "base_url", d.baseURL)
	for {
		select {
		case <-d.stopCh:
			logger.Debug("sse dispatcher stopped")
			return
		default:
		}
		if !d.connect() {
			// stop signal received during connect
			return
		}
		select {
		case <-d.stopCh:
			return
		case <-time.After(reconnBackoff):
		}
	}
}

// connect opens one SSE request and pumps frames until the body closes or
// an error occurs. Returns false if the dispatcher was stopped mid-connect.
// The request context is tied to d.stopCh so stop() unblocks the scanner
// via http.Client's body-close-on-ctx-cancel behaviour.
func (d *sseDispatcher) connect() bool {
	logger := d.logger.load()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-d.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/event", nil)
	if err != nil {
		logger.Warn("sse request", "error", err)
		return true
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := d.httpClient.Do(req) //nolint:gosec // G704: baseURL is trusted config, not user input
	if err != nil {
		if ctx.Err() != nil {
			// stop fired mid-connect; do not retry.
			return false
		}
		logger.Debug("sse connect failed (will retry)", "error", err)
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("sse http status", "status", resp.StatusCode)
		return true
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4<<10), sseLineLimit)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		body := strings.TrimPrefix(line, "data: ")
		d.handleFrame(body)
	}
	// scanner stopped: either body closed (stop) or transport error. The
	// ctx check distinguishes them so stop() does not trigger a retry loop.
	return ctx.Err() == nil
}

// handleFrame parses one SSE data payload and routes the resulting Event to
// the session-bound subscriber. session.idle is handled specially: it
// synthesises an EventResult so a Run that did not see a step-finish
// reason=stop (e.g. a pure chat reply with no tool calls) still terminates.
func (d *sseDispatcher) handleFrame(body string) {
	if sid, ok := parseSessionIdle(body); ok {
		d.deliver(sid, Event{kind: EventResult, sessionID: sid})
		return
	}
	ev, ok := parseEventLine(body)
	if !ok {
		return
	}
	d.deliver(ev.sessionID, ev)
}

// parseSessionIdle reports whether body is a session.idle frame and returns
// the affected sessionID. Used by handleFrame to synthesise a terminal
// EventResult; not part of parseEventLine because the frame carries no
// Event-shape payload.
func parseSessionIdle(body string) (sessionID string, ok bool) {
	var probe struct {
		Type       string `json:"type"`
		Properties struct {
			SessionID string `json:"sessionID"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return "", false
	}
	if probe.Type != "session.idle" || probe.Properties.SessionID == "" {
		return "", false
	}
	return probe.Properties.SessionID, true
}

// deliver writes ev to the subscriber bound to ev.sessionID. Non-blocking:
// a full channel drops the event (the consumer is already behind and will
// catch up via the next frame). Events without a sessionID are dropped
// (they belong to server-level frames that should not surface in any
// specific turn).
func (d *sseDispatcher) deliver(sessionID string, ev Event) {
	if sessionID == "" {
		return
	}
	d.mu.Lock()
	ch, ok := d.subs[sessionID]
	d.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- ev:
	default:
		d.logger.load().Debug("sse subscriber full, dropping event",
			"session_id", sessionID, "event_type", ev.kind)
	}
}

// messageBody is the JSON body POSTed to /session/{id}/message. All fields
// are required by the serve API except Agent (which falls back to the
// session's configured agent when omitted).
type messageBody struct {
	ProviderID string        `json:"providerID,omitempty"`
	ModelID    string        `json:"modelID,omitempty"`
	Agent      string        `json:"agent,omitempty"`
	Role       string        `json:"role"` // always "user"
	Parts      []messagePart `json:"parts"`
}

type messagePart struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// Run starts one turn for opts and returns the parsed event stream. The
// caller MUST drain the channel until close. Run blocks acquiring a
// concurrency slot until ctx is cancelled (returning ctx.Err()) or a slot
// frees up.
//
// Lifecycle:
//  1. Acquire sem.
//  2. Resolve sessionID (use opts.SessionID or POST /session to create).
//  3. Subscribe dispatcher → ch.
//  4. POST /session/{sid}/message?async=true.
//  5. Spawn a goroutine that forwards ch → out, watches ctx for abort, and
//     closes out on terminal event / channel close. The goroutine also
//     releases sem + unsubscribes.
func (c *Client) Run(ctx context.Context, opts RunOptions) (<-chan Event, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	sessionID := opts.SessionID
	if sessionID == "" {
		var err error
		sessionID, err = c.createSession(ctx, opts.Directory)
		if err != nil {
			<-c.sem
			return nil, fmt.Errorf("create session: %w", err)
		}
	}

	ch := c.dispatcher.subscribe(sessionID)
	if err := c.postMessage(ctx, sessionID, opts); err != nil {
		c.dispatcher.unsubscribe(sessionID, ch)
		<-c.sem
		return nil, fmt.Errorf("send message: %w", err)
	}

	out := make(chan Event, subscriberBuf)
	go c.pump(ctx, sessionID, opts, ch, out)
	return out, nil
}

// pump forwards frames from the dispatcher-bound ch to the caller-facing out
// channel. It owns the sem slot and the subscription; both are released
// (LIFO) on exit. On ctx cancellation it POSTs /abort and keeps forwarding
// for a short grace window so the server-emitted idle/error event still
// surfaces as the terminal.
func (c *Client) pump(ctx context.Context, sessionID string, opts RunOptions, ch <-chan Event, out chan<- Event) {
	defer func() { <-c.sem }()
	defer close(out)

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// LineSink tee: verbatim raw isn't available here (the dispatcher
			// parsed the frame). Bridge-side archiving is the canonical path
			// for serve mode; the LineSink hook is preserved for symmetry
			// with the CLI client but is a no-op when nil.
			if opts.LineSink != nil && ev.kind != "" {
				_, _ = io.WriteString(opts.LineSink, ev.kind+"\n")
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
			if ev.kind == EventResult || ev.kind == EventError {
				// Drain any trailing frames buffered behind the terminal
				// without blocking; the dispatcher will keep delivering
				// until unsubscribe closes ch.
				c.drain(ch)
				return
			}
		case <-ctx.Done():
			c.abort(sessionID)
			// Wait briefly for the server's idle/error frame so the caller
			// still sees a terminal event. If none arrives, synthesise one.
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- ev:
				default:
				}
				return
			case <-time.After(abortTimeout):
				select {
				case out <- Event{kind: EventError, sessionID: sessionID, text: "opencode serve run cancelled: " + ctx.Err().Error(), isError: true}:
				default:
				}
				return
			}
		}
	}
}

// drain non-blockingly consumes ch until it empties so a buffered idle event
// arrives before we return. Returns immediately when the channel is empty
// or closed.
func (c *Client) drain(ch <-chan Event) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// createSession POSTs /session with an optional working directory override
// and returns the new session id. opencode serve binds the cwd from the
// server's process dir at creation unless overridden via the "directory"
// field on the body.
func (c *Client) createSession(ctx context.Context, directory string) (string, error) {
	body := map[string]any{}
	if directory != "" {
		body["directory"] = directory
	}
	raw, err := c.postJSON(ctx, "/session", body, nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse /session: %w", err)
	}
	if resp.ID == "" {
		return "", errors.New("session id missing in response")
	}
	return resp.ID, nil
}

// postMessage fires one user message at the session. The async=true query
// makes the call return immediately so all turn events arrive through the
// SSE subscription.
func (c *Client) postMessage(ctx context.Context, sessionID string, opts RunOptions) error {
	mb := messageBody{Role: "user", Parts: []messagePart{{Type: "text", Text: opts.Prompt}}}
	if opts.Model != "" {
		if provider, model, ok := splitProviderModel(opts.Model); ok {
			mb.ProviderID = provider
			mb.ModelID = model
		}
	}
	if opts.Agent != "" {
		mb.Agent = opts.Agent
	}
	_, err := c.postJSON(ctx, "/session/"+sessionID+"/message?async=true", mb, nil)
	return err
}

// abort POSTs /session/{id}/abort. Fire-and-forget; the response carries no
// payload we need. Bounded by abortTimeout so a stalled server cannot wedge
// the pump goroutine.
func (c *Client) abort(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/session/"+sessionID+"/abort", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: baseURL is trusted config
	if err != nil {
		c.logger.Debug("abort session", "session_id", sessionID, "error", err)
		return
	}
	_ = resp.Body.Close()
}

// postJSON is the JSON-POST counterpart to fetchJSON. body is marshalled as
// JSON; the response body (if any) is returned for callers that need it.
// headers is optional.
func (c *Client) postJSON(ctx context.Context, path string, body any, headers map[string]string) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = strings.NewReader(string(buf))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: baseURL is trusted config
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("%s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return io.ReadAll(resp.Body)
}

// splitProviderModel splits a "provider/model" spec into its two halves.
// Returns ok=false for an unsplitable string (the caller leaves both
// ProviderID and ModelID unset and lets the server fall back to its
// configured default).
func splitProviderModel(spec string) (provider, model string, ok bool) {
	idx := strings.IndexByte(spec, '/')
	if idx <= 0 || idx == len(spec)-1 {
		return "", "", false
	}
	return spec[:idx], spec[idx+1:], true
}
