package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark/websocket"
)

// bootstrapServer serves a 200 with the given WS URL + optional client config.
func bootstrapServer(t *testing.T, wsURL string, cfg clientConfig) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"URL": wsURL,
				"ClientConfig": map[string]any{
					"ReconnectCount":    cfg.ReconnectCount,
					"ReconnectInterval": int(cfg.ReconnectInterval / time.Second),
					"ReconnectNonce":    int(cfg.ReconnectNonce / time.Second),
					"PingInterval":      int(cfg.PingInterval / time.Second),
				},
			},
		})
	}))
}

// bootstrapServerNoConfig serves a 200 with only the URL; the client keeps its
// in-process cfg override. Used by tests that need sub-second ping intervals
// (the wire format encodes whole seconds).
func bootstrapServerNoConfig(t *testing.T, wsURL string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"URL": wsURL},
		})
	}))
}

// fakeServer is a minimal RFC 6455 server that performs the handshake and
// then lets a script drive frames against the conn. It records frames the
// client sends (pings, acks) so tests can assert on them.
type fakeServer struct {
	ln        net.Listener
	mu        sync.Mutex
	conns     []net.Conn
	clientRx  [][]byte // frames the client wrote (raw)
	t         *testing.T
	closeOnce sync.Once
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fs := &fakeServer{ln: ln, t: t}
	go fs.accept()
	return fs
}

func (f *fakeServer) URL() string { return "ws://" + f.ln.Addr().String() + "/" }

// connCount returns the number of accepted conns (mutex-guarded for race-safe
// test reads).
func (f *fakeServer) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

func (f *fakeServer) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		go f.handle(c)
	}
}

// handle reads the HTTP upgrade, answers 101, then keeps the conn open. The
// test sends frames via sendFrame (server-side); inbound client frames are
// captured into clientRx for later assertion.
func (f *fakeServer) handle(c net.Conn) {
	// Read upgrade request (one CRLF-terminated block).
	br := make([]byte, 4096)
	n, _ := c.Read(br)
	if !strings.Contains(string(br[:n]), "Upgrade: websocket") {
		return
	}
	key := extractWSKey(string(br[:n]))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"
	if _, err := c.Write([]byte(resp)); err != nil {
		return
	}
	// Drain client frames until close; capture them.
	buf := make([]byte, 8192)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		// Minimal server-side unmask to record the payload.
		if n >= 2 && buf[1]&0x80 != 0 {
			plen := int(buf[1] & 0x7f)
			maskIdx := 2
			mask := [4]byte{}
			if plen < 126 {
				copy(mask[:], buf[2:6])
				maskIdx = 6
			}
			payload := make([]byte, plen)
			copy(payload, buf[maskIdx:maskIdx+plen])
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
			f.mu.Lock()
			f.clientRx = append(f.clientRx, payload)
			f.mu.Unlock()
		}
	}
}

// sendFrame writes a single unmasked binary server frame with the given
// payload (final, no mask — servers don't mask).
func (f *fakeServer) sendFrame(payload []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.conns) == 0 {
		return
	}
	c := f.conns[len(f.conns)-1]
	hdr := []byte{0x82} // FIN + binary
	switch {
	case len(payload) <= 125:
		hdr = append(hdr, byte(len(payload)))
	case len(payload) <= 65535:
		hdr = append(hdr, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		hdr = append(hdr, 127)
		l := uint64(len(payload))
		for i := 7; i >= 0; i-- {
			hdr = append(hdr, byte(l>>(8*i)))
		}
	}
	_, _ = c.Write(append(hdr, payload...))
}

func (f *fakeServer) Close() error {
	f.closeOnce.Do(func() {
		_ = f.ln.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			_ = c.Close()
		}
		f.mu.Unlock()
	})
	return nil
}

// capturedClientFrames returns the unmasked payloads the client wrote.
func (f *fakeServer) capturedClientFrames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.clientRx))
	copy(out, f.clientRx)
	return out
}

// extractWSKey pulls Sec-WebSocket-Key out of the upgrade request headers.
func extractWSKey(req string) string {
	for _, line := range strings.Split(req, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-key:") {
			return strings.TrimSpace(line[len("sec-websocket-key:"):])
		}
	}
	return ""
}

// wsAccept delegates to the real websocket.ComputeAccept (RFC 6455 §1.3).
func wsAccept(key string) string {
	return websocket.ComputeAccept(key)
}

// recordingHandler captures delivered events for assertions. Implements the
// ws.Sink interface (OnMessage/OnCard) rather than the lark.Handler surface,
// since this test lives inside the ws package.
type recordingHandler struct {
	mu    sync.Mutex
	msgs  []*MessageReceive
	cards []*CardAction
}

func (h *recordingHandler) OnMessage(_ context.Context, ev *MessageReceive) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, ev)
	return nil
}

func (h *recordingHandler) OnCard(_ context.Context, ev *CardAction) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cards = append(h.cards, ev)
	return nil
}

func (h *recordingHandler) snapshot() ([]*MessageReceive, []*CardAction) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := make([]*MessageReceive, len(h.msgs))
	copy(msgs, h.msgs)
	cards := make([]*CardAction, len(h.cards))
	copy(cards, h.cards)
	return msgs, cards
}

// waitUntil repeatedly calls check (10ms tick) until it returns true or the
// timeout elapses; returns the last value. Used to synchronise async server
// delivery with test assertions without fixed sleeps.
func waitUntil(check func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return check()
}

// TestWSClient_DeliversEventAndAcks is the end-to-end M3 contract: a fake WS
// server delivers one im.message.receive_v1 frame, the client delivers it to
// the handler AND writes back a {code:200} ACK frame.
func TestWSClient_DeliversEventAndAcks(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.Close()
	bsrv := bootstrapServer(t, fs.URL(), clientConfig{
		ReconnectCount: 0, ReconnectInterval: 0, ReconnectNonce: 0, PingInterval: 600 * time.Second,
	})
	defer bsrv.Close()

	h := &recordingHandler{}
	var ready atomic.Bool
	wc := newWSClient("a", "s", bsrv.URL, bsrv.Client(), Lifecycle{
		OnReady: func() { ready.Store(true) },
	}, h)
	// Make reconnect instant so the test never blocks on backoff.
	wc.cfg = clientConfig{ReconnectCount: 0, ReconnectInterval: 0, ReconnectNonce: 0, PingInterval: 600 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wc.Start(ctx) }()

	if !waitUntil(func() bool { return ready.Load() }, 3*time.Second) {
		t.Fatal("OnReady never fired")
	}

	// Build + send a real event data frame.
	ev := mustMarshal(t, map[string]any{
		"header": map[string]any{"event_id": "evt_1", "event_type": "im.message.receive_v1"},
		"event": map[string]any{
			"sender": map[string]any{"sender_id": map[string]any{"open_id": "ou_sender"}},
			"message": map[string]any{
				"message_id": "om_msg", "chat_id": "oc_chat", "chat_type": "group",
				"message_type": "text", "content": `{"text":"hi"}`,
			},
		},
	})
	frame := Frame{
		SeqID: 1, LogID: 2, Service: 10, Method: MethodData,
		Headers: Headers{
			{Key: HeaderType, Value: TypeEvent},
			{Key: HeaderMessageID, Value: "om_msg"},
			{Key: HeaderSum, Value: "1"},
			{Key: HeaderSeq, Value: "0"},
		},
		Payload: ev,
	}
	fs.sendFrame(mustMarshalFrame(t, &frame))

	if !waitUntil(func() bool {
		msgs, _ := h.snapshot()
		return len(msgs) == 1
	}, 3*time.Second) {
		t.Fatal("event not delivered to handler")
	}
	msgs, _ := h.snapshot()
	if msgs[0].MessageID != "om_msg" || msgs[0].ChatID != "oc_chat" || msgs[0].SenderOpenID != "ou_sender" {
		t.Fatalf("delivered event mismatched: %+v", msgs[0])
	}

	// The client must have written an ACK frame with code:200.
	if !waitUntil(func() bool {
		for _, p := range fs.capturedClientFrames() {
			if strings.Contains(string(p), `"code":200`) {
				return true
			}
		}
		return false
	}, 3*time.Second) {
		t.Fatal("client did not write a code:200 ACK")
	}
}

// TestWSClient_DeliversCardAction verifies the card.action.trigger route.
func TestWSClient_DeliversCardAction(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.Close()
	bsrv := bootstrapServer(t, fs.URL(), clientConfig{PingInterval: 600 * time.Second})
	defer bsrv.Close()
	h := &recordingHandler{}
	wc := newWSClient("a", "s", bsrv.URL, bsrv.Client(), Lifecycle{}, h)
	wc.cfg = clientConfig{PingInterval: 600 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wc.Start(ctx) }()
	if !waitUntil(func() bool { return fs.connCount() > 0 }, 3*time.Second) {
		t.Fatal("server never saw a connection")
	}

	ev := mustMarshal(t, map[string]any{
		"header": map[string]any{"event_id": "evt_c", "event_type": "card.action.trigger"},
		"event": map[string]any{
			"operator": map[string]any{"open_id": "ou_op"},
			"action": map[string]any{
				"value":      map[string]any{"kind": "approve"},
				"form_value": map[string]any{"q_0": "yes"},
			},
			"context": map[string]any{
				"open_message_id": "om_card",
				"open_chat_id":    "oc_card",
			},
		},
	})
	frame := Frame{Method: MethodData, Headers: Headers{{Key: HeaderType, Value: TypeEvent}}}
	frame.Payload = ev
	fs.sendFrame(mustMarshalFrame(t, &frame))
	if !waitUntil(func() bool {
		_, cards := h.snapshot()
		return len(cards) == 1
	}, 3*time.Second) {
		t.Fatal("card action not delivered")
	}
	_, cards := h.snapshot()
	c := cards[0]
	if c.EventID != "evt_c" || c.MessageID != "om_card" || c.ChatID != "oc_card" {
		t.Fatalf("card identity mismatch: %+v", c)
	}
	if c.Operator.OpenID != "ou_op" {
		t.Fatalf("operator = %+v", c.Operator)
	}
	if c.Action.Value["kind"] != "approve" || c.Action.FormValue["q_0"] != "yes" {
		t.Fatalf("action payload mismatch: %+v", c.Action)
	}
}

// TestWSClient_PingSent verifies the client emits a protobuf ping frame on
// the configured interval. Uses bootstrapServerNoConfig so the sub-second
// interval set directly on wc.cfg survives the bootstrap round-trip (the wire
// format encodes whole seconds).
func TestWSClient_PingSent(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.Close()
	bsrv := bootstrapServerNoConfig(t, fs.URL())
	defer bsrv.Close()
	wc := newWSClient("a", "s", bsrv.URL, bsrv.Client(), Lifecycle{}, nil)
	// A short PingInterval makes the first ping fire soon after the connection
	// comes up, and a generous waitUntil window absorbs the bootstrap→dial→
	// handshake startup latency under -race / scheduler load. The previous
	// 200ms+3s pairing was tight enough to flake under contention.
	wc.cfg = clientConfig{PingInterval: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wc.Start(ctx) }()
	if !waitUntil(func() bool {
		for _, p := range fs.capturedClientFrames() {
			var f Frame
			if err := f.Unmarshal(p); err == nil && f.Method == MethodControl &&
				Headers(f.Headers).GetString(HeaderType) == TypePing {
				return true
			}
		}
		return false
	}, 5*time.Second) {
		t.Fatal("client did not send a ping frame")
	}
}

// TestReassembler_ChunksJoin verifies a 3-chunk event is only delivered once
// all pieces arrive, in order regardless of arrival sequence.
func TestReassembler_ChunksJoin(t *testing.T) {
	r := newReassembler()
	// Out-of-order arrival: seq=1, then 0, then 2.
	for _, seq := range []int{1, 0, 2} {
		joined, ok := r.feed("om_x", 3, seq, []byte(fmt.Sprintf("chunk%d", seq)))
		if seq != 2 {
			if ok || joined != nil {
				t.Fatalf("seq %d: expected not-yet-complete, got ok=%v %q", seq, ok, joined)
			}
		} else {
			if !ok {
				t.Fatal("final chunk should complete the group")
			}
			if want := "chunk0chunk1chunk2"; string(joined) != want {
				t.Fatalf("joined = %q, want %q", joined, want)
			}
		}
	}
	// Second completion on the same id is a fresh group.
	if _, ok := r.feed("om_x", 1, 0, []byte("solo")); !ok {
		t.Fatal("unsplit (sum=1) should deliver immediately")
	}
}

// TestReassembler_SweepDropsStale verifies incomplete groups are GC'd so a
// dropped tail chunk cannot leak memory.
func TestReassembler_SweepDropsStale(t *testing.T) {
	r := newReassembler()
	r.pending["om_dead"] = &pendingChunks{chunks: make([][]byte, 2), firstSeen: time.Now().Add(-1 * time.Hour)}
	r.sweep(chunkTTL)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pending["om_dead"]; ok {
		t.Fatal("stale chunk group was not swept")
	}
}

// TestReassembler_RejectsHugeSum pins the §3.5 bound: a "sum" header above
// maxReassembleChunks is refused outright (no allocation), so a malicious
// peer claiming sum=1e9 cannot grow an 8 GB slice header.
func TestReassembler_RejectsHugeSum(t *testing.T) {
	r := newReassembler()
	joined, ok := r.feed("om_big", maxReassembleChunks+1, 0, []byte("x"))
	if ok || joined != nil {
		t.Fatalf("huge sum should be rejected, got ok=%v joined=%q", ok, joined)
	}
	r.mu.Lock()
	_, present := r.pending["om_big"]
	r.mu.Unlock()
	if present {
		t.Fatal("huge-sum group must not allocate a pending entry")
	}
}

// TestFrame_RejectsTooManyHeaders pins the §3.5 bound: a frame carrying more
// than maxFrameHeaders Header submessages fails to decode.
func TestFrame_RejectsTooManyHeaders(t *testing.T) {
	var b buf
	for i := 0; i <= maxFrameHeaders; i++ {
		b.tagBytes(5, marshalHeader(Header{Key: "x", Value: "y"}))
	}
	var f Frame
	if err := f.Unmarshal(b.bytes); err == nil {
		t.Fatalf("frame with %d headers must fail to decode", maxFrameHeaders+1)
	}
}

// TestBootstrap_AuthFailureIsFatal verifies a 514 auth code from bootstrap
// surfaces as a fatal error Start does NOT retry forever.
func TestBootstrap_AuthFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": codeAuthFailed, "msg": "bad secret"})
	}))
	defer srv.Close()
	wc := newWSClient("a", "s", srv.URL, srv.Client(), Lifecycle{}, nil)
	err := wc.Start(context.Background())
	if err == nil {
		t.Fatal("Start should return fatal error")
	}
	if !isFatal(err) {
		t.Fatalf("error not fatal: %v", err)
	}
}

// TestParseServiceID covers the URL→int32 mapping for the ping Service field.
func TestParseServiceID(t *testing.T) {
	if got := parseServiceID("wss://x/ws?service_id=33554678&device_id=1"); got != 33554678 {
		t.Fatalf("got %d", got)
	}
	if got := parseServiceID("wss://x/ws"); got != 0 {
		t.Fatalf("missing param should be 0, got %d", got)
	}
	if got := parseServiceID("wss://x/ws?service_id=notanint"); got != 0 {
		t.Fatalf("non-numeric should be 0, got %d", got)
	}
}

// mustMarshal JSON-marshals v or fails the test.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// mustMarshalFrame protobuf-marshals the frame or fails the test.
func mustMarshalFrame(t *testing.T, f *Frame) []byte {
	t.Helper()
	b, err := f.Marshal()
	if err != nil {
		t.Fatalf("frame marshal: %v", err)
	}
	return b
}

// keep imports used
var _ = io.Discard
