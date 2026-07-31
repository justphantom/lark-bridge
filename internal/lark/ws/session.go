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

	// The dispatch pool decouples event handling from the read loop: a slow
	// sink (file download + convert) must not head-of-line block frame
	// processing or delay ACKs. Owned per session so its workers are torn
	// down with everything else below.
	pool := newDispatchPool()

	var wg sync.WaitGroup
	firstExit := make(chan struct{})
	var once sync.Once
	signal := func() { once.Do(func() { close(firstExit) }) }

	wg.Add(3)
	go func() {
		defer wg.Done()
		defer signal()
		c.receiveLoop(ctx, conn, reassembly, pool)
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
	// Force-close so the other loops reading on this conn unblock.
	_ = conn.Close()
	// Cancel the derived ctx so the sweep goroutine (which does NOT touch
	// the conn and therefore is NOT unblocked by conn.Close) also returns;
	// otherwise <-exitCh below blocks forever on a non-ctx disconnect.
	// cancel() is idempotent — the deferred cancel() on line 24 is a no-op
	// after this (context contract).
	cancel()
	// Wait for ALL three loops to return. Without the WaitGroup the
	// surviving loops would outlive runSession as short-lived orphans
	// piling up per reconnect.
	<-exitCh
	// receiveLoop has returned (it is part of wg above), so no submit can
	// race this close. Draining keeps already-queued events deliverable —
	// the cancelled ctx makes well-behaved sinks abort promptly — and the
	// wait guarantees no worker goroutine outlives the session.
	pool.close()
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
func (c *Client) receiveLoop(ctx context.Context, conn *websocket.Conn, reassembly *reassembler, pool *dispatchPool) {
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
			c.handleData(ctx, frame, reassembly, rt, conn, pool)
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
	var sc serverClientConfig
	if err := json.Unmarshal(frame.Payload, &sc); err != nil {
		return
	}
	c.mu.Lock()
	// Field-wise merge: a pong carrying only some fields must not zero the
	// rest (ReconnectCount=0 would exhaust the budget; zero intervals would
	// hot-loop). Intervals are clamped to a floor inside merge.
	c.cfg = c.cfg.merge(sc)
	c.mu.Unlock()
}

// handleData reassembles a data frame, ACKs it, then dispatches it to the
// session's worker pool. ACKing is mandatory: the lark server treats an
// unacked delivery as a failure and redelivers, which the upstream dedup
// then has to suppress.
//
// The ACK is sent the instant the full message is reassembled, BEFORE
// dispatch: with a slow sink (file download + convert, tens of seconds),
// dispatch-then-ACK would hold this frame's ACK — and under the old
// serial read loop every other chat's ACK too — for the whole sink
// latency, inviting redelivery storms whenever the server tightens
// PingInterval. Dispatch failures no longer map to a 500 ACK; a
// deterministic dispatch failure would only loop on redelivery, so the
// error is reported through the job's errf (Lifecycle.OnError) instead.
func (c *Client) handleData(ctx context.Context, frame Frame, r *reassembler, rt *router, conn frameWriter, pool *dispatchPool) {
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
	c.writeAck(conn, frame, http.StatusOK)
	if rt.sink == nil {
		return
	}
	eventType := hs.GetString(HeaderType)
	job := dispatchJob{
		ctx:     ctx,
		rt:      rt,
		payload: joined,
		errf: func(err error) {
			c.fireError(fmt.Errorf("ws: dispatch %s: %w", eventType, err))
		},
	}
	if !pool.submit(job) {
		// Queue saturated (4 workers all busy on slow sinks, 64-deep buffer
		// full): fall back to inline dispatch on the read loop. This
		// deliberately reintroduces head-of-line blocking under sustained
		// overload rather than drop the event — backpressure then throttles
		// the read loop, which is safer than unbounded queue growth or silent
		// data loss. The ACK has already gone out, so no redelivery pressure
		// is added; the job's own recover keeps a panicking sink from killing
		// the read loop.
		job.run()
	}
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
		// NewTimer (not time.After) so the timer is stopped on the ctx-done
		// path — time.After's timer lives until it fires, needlessly pinning
		// up to `interval` of timer heap on every reconnect (W3).
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
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
