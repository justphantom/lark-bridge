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
