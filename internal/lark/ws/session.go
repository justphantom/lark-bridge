package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark/websocket"
)

// runSession drives the receive, ping and chunk-sweep loops for one
// connection. It blocks until EITHER a loop exits (conn broken / read
// deadline fired) OR ctx is done, then force-closes the conn so the other
// loops unblock and WaitGroup confirms every goroutine has returned before
// runSession itself returns — no orphan goroutines accumulate across
// reconnects. The reassembler is owned here (not in receiveLoop) so the
// sweep loop can run independently of frame arrival (a silent peer would
// otherwise leave partial chunks in memory until the next frame).
func (c *Client) runSession(ctx context.Context, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reassembly := newReassembler()
	sweepTicker := time.NewTicker(chunkTTL)
	defer sweepTicker.Stop()

	var wg sync.WaitGroup
	firstExit := make(chan struct{})
	var once sync.Once
	signal := func() { once.Do(func() { close(firstExit) }) }

	wg.Add(3)
	go func() {
		defer wg.Done()
		defer signal()
		c.receiveLoop(ctx, conn, reassembly)
	}()
	go func() {
		defer wg.Done()
		defer signal()
		c.pingLoop(ctx, conn)
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sweepTicker.C:
				reassembly.sweep(chunkTTL)
			}
		}
	}()

	exitCh := make(chan struct{})
	go func() { wg.Wait(); close(exitCh) }()
	select {
	case <-firstExit: // a loop exited → conn is broken or being torn down
	case <-ctx.Done():
	}
	// Force-close so the other loops reading on this conn unblock, then wait
	// for ALL three to return. Without the WaitGroup the surviving loops
	// would outlive runSession as short-lived orphans piling up per reconnect.
	_ = conn.Close()
	<-exitCh
}

// receiveLoop reads frames until the conn errors or ctx is done. Each inbound
// data frame is reassembled via the session-owned reassembler, routed, and
// ACK'd; each pong refreshes the config.
//
// A read deadline is reset after every successfully read frame to 2× the
// configured ping interval. The lark server replies to each ping with a pong
// (or sends events) well within that window; if nothing arrives for two ping
// cycles the connection is half-open (peer host crashed / network split) and
// the read returns a deadline error → this loop exits → runSession reconnects.
// Without this the original SDK's gorilla SetReadDeadline analogue is gone and
// a silent death would otherwise only surface via the 5-minute frontend
// watchdog, dropping every event in between.
func (c *Client) receiveLoop(ctx context.Context, conn *websocket.Conn, reassembly *reassembler) {
	rt := &router{sink: c.snapshotSink()}
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
		c.writeAck(conn, frame, http.StatusOK, nil)
		return
	}
	ackCode := http.StatusOK
	var businessResponse []byte
	br, err := rt.dispatch(ctx, joined)
	if err != nil {
		c.fireError(fmt.Errorf("ws: dispatch %s: %w", hs.GetString(HeaderType), err))
		ackCode = http.StatusInternalServerError
	} else {
		businessResponse = br
	}
	c.writeAck(conn, frame, ackCode, businessResponse)
}

// frameWriter is the write seam of a websocket.Conn so handleData can be
// tested with a fake. *websocket.Conn satisfies it.
type frameWriter interface {
	WriteMessage(opcode int, data []byte) error
}

// writeAck builds and sends the ACK frame. ackCode is the transport-level
// status (200/500). businessResponse, when non-nil, is a JSON object carrying
// the card.action.trigger business payload (toast/card). Feishu expects these
// fields at the TOP LEVEL of the ACK body (same shape as the webhook response
// body), not nested under "data" — wrapping caused 300000 internal errors.
// The "code" field is kept alongside so the transport ACK stays well-formed.
func (c *Client) writeAck(conn frameWriter, in Frame, ackCode int, businessResponse []byte) {
	payload := map[string]any{"code": ackCode}
	if len(businessResponse) > 0 {
		var br map[string]any
		if json.Unmarshal(businessResponse, &br) == nil {
			for k, v := range br {
				payload[k] = v
			}
		}
	}
	payloadBytes, _ := json.Marshal(payload)
	ack := NewAckFrame(in, payloadBytes)
	bs, err := ack.Marshal()
	if err != nil {
		return
	}
	if err := conn.WriteMessage(websocket.OpcodeBinary, bs); err != nil {
		c.fireError(fmt.Errorf("ws: write ack: %w", err))
	}
}

// pingLoop sends a protobuf ping frame every PingInterval. The interval is
// re-read on every tick (not latched at start) so a server-driven cadence
// change delivered via a pong payload's ClientConfig takes effect without
// reconnecting. A write error ends the session (the receive loop will also
// surface the broken conn).
func (c *Client) pingLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		c.mu.Lock()
		interval := c.cfg.PingInterval
		serviceID := c.serviceID
		c.mu.Unlock()
		if interval <= 0 {
			interval = 90 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
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
