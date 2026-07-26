package websocket

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Opcodes per RFC 6455 Section 5.2.
const (
	OpcodeContinuation = 0x0
	OpcodeText         = 0x1
	OpcodeBinary       = 0x2
	OpcodeClose        = 0x8
	OpcodePing         = 0x9
	OpcodePong         = 0xa
)

// CloseGoingAway is a generic close status used when the peer did not send one.
const (
	StatusNormalClosure   = 1000
	StatusGoingAway       = 1001
	StatusProtocolError   = 1002
	StatusUnsupportedData = 1003
	StatusInvalidData     = 1007
	StatusPolicyViolation = 1008
	StatusInternalError   = 1011
)

// maxControlPayload caps a control-frame body (RFC 6455 Section 5.5: ≤ 125).
const maxControlPayload = 125

// ErrCloseSent is returned by WriteMessage after a Close frame has been written.
var ErrCloseSent = errors.New("websocket: close sent")

// CloseError reports that the peer closed the connection. Code is the status
// code from the close frame (1000 if none). Text carries the optional reason.
type CloseError struct {
	Code int
	Text string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket: close %d: %s", e.Code, e.Text)
}

// Conn is a client-side WebSocket connection. Reads return fully reassembled
// messages (fragmented messages are joined transparently, interleaved control
// frames are handled inline). Writes mask the payload as the client role
// requires. Safe for one concurrent reader and one concurrent writer.
type Conn struct {
	nc    net.Conn
	br    *bufio.Reader
	isTLS bool

	writeMu sync.Mutex
	readMu  sync.Mutex

	// Read fragmentation state. messageOpcode is set by the first frame of a
	// fragmented message and cleared once delivered; frameBuf accumulates.
	messageOpcode int
	frameBuf      []byte

	// Close handshake bookkeeping.
	closeSent     bool
	closeReceived bool
	closeMu       sync.Mutex

	readDeadline time.Time
}

func newConn(nc net.Conn, br *bufio.Reader, isTLS bool) *Conn {
	return &Conn{nc: nc, br: br, isTLS: isTLS}
}

// Underlying exposes the raw connection so callers can hook keepalive or
// statistics; lark-bridge uses it for nothing in production.
func (c *Conn) Underlying() net.Conn { return c.nc }

// SetReadDeadline sets the deadline for future ReadMessage calls. A zero time
// clears it. The lark WS client uses this to bound idle reads.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	return c.nc.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future WriteMessage calls.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.nc.SetWriteDeadline(t)
}

// ReadMessage reads one full data message. Fragmented data messages are
// reassembled; ping/pong/close frames arriving mid-message are handled inline
// (ping → pong auto-reply, close → CloseError). Returns the data opcode
// (OpcodeText or OpcodeBinary) and the joined payload.
func (c *Conn) ReadMessage() (int, []byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	c.frameBuf = c.frameBuf[:0]
	c.messageOpcode = -1

	for {
		op, payload, fin, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if isControlFrame(op) {
			if err := c.handleControl(op, payload); err != nil {
				return 0, nil, err
			}
			continue
		}
		switch op {
		case OpcodeContinuation:
			if c.messageOpcode < 0 {
				return 0, nil, errors.New("websocket: unexpected continuation frame")
			}
			c.frameBuf = append(c.frameBuf, payload...)
		case OpcodeText, OpcodeBinary:
			if c.messageOpcode >= 0 {
				return 0, nil, errors.New("websocket: new data frame inside fragmented message")
			}
			c.messageOpcode = op
			c.frameBuf = append(c.frameBuf, payload...)
		default:
			return 0, nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
		if fin {
			op := c.messageOpcode
			out := make([]byte, len(c.frameBuf))
			copy(out, c.frameBuf)
			c.frameBuf = c.frameBuf[:0]
			c.messageOpcode = -1
			return op, out, nil
		}
	}
}

// handleControl dispatches close/ping/pong. Control frames never fragment and
// carry at most 125 bytes (enforced by readFrame). Auto-pong replies to ping.
func (c *Conn) handleControl(opcode int, payload []byte) error {
	switch opcode {
	case OpcodeClose:
		code, text := parseClosePayload(payload)
		c.closeMu.Lock()
		c.closeReceived = true
		c.closeMu.Unlock()
		// Echo the close so the peer sees the handshake complete.
		_ = c.writeControl(OpcodeClose, makeClosePayload(code))
		return &CloseError{Code: code, Text: text}
	case OpcodePing:
		return c.writeControl(OpcodePong, payload)
	case OpcodePong:
		return nil
	default:
		return fmt.Errorf("websocket: unknown control opcode %d", opcode)
	}
}

// readFrame decodes a single WebSocket frame from the wire. Server frames are
// never masked; we reject a masked server frame as a protocol error.
func (c *Conn) readFrame() (op int, payload []byte, fin bool, err error) {
	hdr := make([]byte, 2)
	if _, err = io.ReadFull(c.br, hdr); err != nil {
		return 0, nil, false, err
	}
	fin = hdr[0]&0x80 != 0
	rsv := hdr[0] & 0x70
	if rsv != 0 {
		return 0, nil, false, fmt.Errorf("websocket: reserved bits set: 0x%x", rsv)
	}
	op = int(hdr[0] & 0x0f)
	masked := hdr[1]&0x80 != 0
	rawLen := uint64(hdr[1] & 0x7f)
	switch rawLen {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, false, err
		}
		rawLen = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, false, err
		}
		rawLen = binary.BigEndian.Uint64(ext[:])
	}
	// Cap allocation to a sane ceiling so a hostile/buggy server cannot OOM
	// (or send a 64-bit length that overflows int on read). lark control
	// frames and acks are tiny; 1 MiB is well above any legit single frame
	// and fragmentation handles larger logical messages. Bound the raw value
	// BEFORE the int conversion so a >maxInt64 length cannot wrap negative.
	const maxSingleFrame uint64 = 1 << 20
	if rawLen > maxSingleFrame {
		return 0, nil, false, fmt.Errorf("websocket: frame too large: %d", rawLen)
	}
	payloadLen := int(rawLen)
	if isControlFrame(op) && (payloadLen > maxControlPayload || !fin) {
		return 0, nil, false, errors.New("websocket: invalid control frame")
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err = io.ReadFull(c.br, payload); err != nil {
			return 0, nil, false, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
	}
	return op, payload, fin, nil
}

// WriteMessage writes a single data message (final, masked). opcode is one of
// OpcodeText / OpcodeBinary. Fragmentation is not needed by lark-bridge: the
// largest outbound payload is a ping frame (empty) or an ACK (small JSON).
func (c *Conn) WriteMessage(opcode int, data []byte) error {
	c.closeMu.Lock()
	sent := c.closeSent
	c.closeMu.Unlock()
	if sent {
		return ErrCloseSent
	}
	return c.writeData(opcode, data, true)
}

// writeData frames + masks a data frame. final controls the FIN bit.
func (c *Conn) writeData(opcode int, data []byte, final bool) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeFrameLocked(opcode, data, final, true)
}

// writeControl writes a control frame (no FIN scheduling, payload ≤ 125).
func (c *Conn) writeControl(opcode int, data []byte) error {
	if len(data) > maxControlPayload {
		return fmt.Errorf("websocket: control payload too large: %d", len(data))
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeFrameLocked(opcode, data, true, true)
}

// writeFrameLocked emits one frame. Caller holds writeMu.
func (c *Conn) writeFrameLocked(opcode int, data []byte, final, mask bool) error {
	var hdr [14]byte
	hdr[0] = byte(opcode & 0x0f)
	if final {
		hdr[0] |= 0x80
	}
	var n int
	var maskKey [4]byte
	if mask {
		if _, err := rand.Read(maskKey[:]); err != nil {
			return err
		}
		hdr[1] |= 0x80
	}
	plen := len(data)
	switch {
	case plen <= 125:
		hdr[1] |= byte(plen)
		n = 2
	case plen <= 65535:
		hdr[1] |= 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(plen))
		n = 4
	default:
		hdr[1] |= 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(plen))
		n = 10
	}
	if mask {
		copy(hdr[n:n+4], maskKey[:])
		n += 4
	}
	if _, err := c.nc.Write(hdr[:n]); err != nil {
		return err
	}
	if plen == 0 {
		return nil
	}
	if mask {
		out := make([]byte, plen)
		for i := range plen {
			out[i] = data[i] ^ maskKey[i%4]
		}
		_, err := c.nc.Write(out)
		return err
	}
	_, err := c.nc.Write(data)
	return err
}

// Close starts the closing handshake (sends a close frame) then closes the
// underlying connection. Idempotent.
func (c *Conn) Close() error {
	c.closeMu.Lock()
	if !c.closeSent {
		c.closeSent = true
		_ = c.writeControl(OpcodeClose, makeClosePayload(StatusNormalClosure))
	}
	c.closeMu.Unlock()
	return c.nc.Close()
}

// WritePing sends a ping frame with optional payload. The lark client sends
// its own protobuf ping frame via WriteMessage(binary), so this helper is here
// for protocol completeness/tests.
func (c *Conn) WritePing(payload []byte) error {
	return c.writeControl(OpcodePing, payload)
}

// isControlFrame reports whether op is a control frame (≥ 8).
func isControlFrame(op int) bool {
	return op >= 0x8
}

func parseClosePayload(p []byte) (code int, text string) {
	if len(p) == 0 {
		return StatusNormalClosure, ""
	}
	if len(p) < 2 {
		return StatusInvalidData, ""
	}
	code = int(binary.BigEndian.Uint16(p[:2]))
	text = string(p[2:])
	return code, text
}

func makeClosePayload(code int) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(code)) //nolint:gosec // G115: code is an RFC 6455 status constant (1000-1011), always fits uint16.
	return b
}
