package websocket

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// startTestServer runs an RFC 6455 server that performs the upgrade handshake
// against the passed checkKey (true = accept), then drives echo/binary/close
// behaviour described by script. Returns the ws:// URL.
//
// Each script entry is one server-side action: send a text/binary frame, read
// one frame and stash it in a side channel, pause, or close. The server masks
// nothing (servers do not mask) and validates that client frames are masked.
func startTestServer(t *testing.T, script func(*serverSession)) (url string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	sess := &serverSession{t: t, closed: make(chan struct{})}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(sess.closed)
			return
		}
		sess.run(conn)
		_ = ln.Close()
		close(sess.closed)
	}()
	script(sess) // register behaviour
	return "ws://" + ln.Addr().String() + "/"
}

type serverSession struct {
	t      *testing.T
	mu     sync.Mutex
	conn   net.Conn
	br     *bufio.Reader
	steps  []func(net.Conn, *bufio.Reader)
	closed chan struct{}
}

func (s *serverSession) sendText(payload string) {
	s.mu.Lock()
	s.steps = append(s.steps, func(c net.Conn, _ *bufio.Reader) {
		writeServerFrame(c, OpcodeText, []byte(payload))
	})
	s.mu.Unlock()
}

func (s *serverSession) sendBinary(payload []byte) {
	s.mu.Lock()
	s.steps = append(s.steps, func(c net.Conn, _ *bufio.Reader) {
		writeServerFrame(c, OpcodeBinary, payload)
	})
	s.mu.Unlock()
}

func (s *serverSession) sendFragmented(opcode int, parts ...[]byte) {
	s.mu.Lock()
	s.steps = append(s.steps, func(c net.Conn, _ *bufio.Reader) {
		for i, p := range parts {
			op := opcode
			fin := i == len(parts)-1
			if i > 0 {
				op = OpcodeContinuation
			}
			writeServerFrameExt(c, op, p, fin)
		}
	})
	s.mu.Unlock()
}

func (s *serverSession) expectRead(opcode int) {
	s.mu.Lock()
	s.steps = append(s.steps, func(_ net.Conn, br *bufio.Reader) {
		op, payload, fin, err := readClientFrame(br)
		if err != nil {
			s.t.Errorf("server expectRead: %v", err)
			return
		}
		if op != opcode {
			s.t.Errorf("server expectRead opcode=%d want %d", op, opcode)
		}
		if !fin {
			s.t.Errorf("server expectRead expected fin, got continuation")
		}
		_ = payload
	})
	s.mu.Unlock()
}

// run drives the upgrade then the script. It blocks until the client closes
// or all steps are consumed.
func (s *serverSession) run(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	// Read + answer the HTTP upgrade.
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + computeAccept(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		return
	}
	for _, step := range s.steps {
		step(conn, br)
	}
	// Wait for client close so the test goroutine exits cleanly.
	io.Copy(io.Discard, br)
}

// writeServerFrame writes a single final unmasked server frame.
func writeServerFrame(w io.Writer, opcode int, payload []byte) {
	writeServerFrameExt(w, opcode, payload, true)
}

func writeServerFrameExt(w io.Writer, opcode int, payload []byte, fin bool) {
	var hdr [10]byte
	hdr[0] = byte(opcode & 0x0f)
	if fin {
		hdr[0] |= 0x80
	}
	n := 1
	switch {
	case len(payload) <= 125:
		hdr[1] = byte(len(payload))
		n = 2
	case len(payload) <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(payload)))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(len(payload)))
		n = 10
	}
	w.Write(hdr[:n])
	w.Write(payload)
}

// readClientFrame reads a frame the client sent. It expects (and strips) the
// mask, since clients must mask per RFC 6455.
func readClientFrame(br *bufio.Reader) (op int, payload []byte, fin bool, err error) {
	hdr := make([]byte, 2)
	if _, err = io.ReadFull(br, hdr); err != nil {
		return 0, nil, false, err
	}
	fin = hdr[0]&0x80 != 0
	op = int(hdr[0] & 0x0f)
	masked := hdr[1]&0x80 != 0
	plen := int(hdr[1] & 0x7f)
	switch plen {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return
		}
		plen = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return
		}
		plen = int(binary.BigEndian.Uint64(ext[:]))
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, plen)
	if plen > 0 {
		if _, err = io.ReadFull(br, payload); err != nil {
			return
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
	}
	if !masked {
		err = errors.New("client frame not masked")
	}
	return
}

func TestDial_HandshakeAndEcho(t *testing.T) {
	url := startTestServer(t, func(s *serverSession) {
		s.sendText("hello")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, resp, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	op, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != OpcodeText || string(data) != "hello" {
		t.Fatalf("got op=%d data=%q, want text/hello", op, data)
	}
}

func TestWriteMessage_ClientMasks(t *testing.T) {
	url := startTestServer(t, func(s *serverSession) {
		s.expectRead(OpcodeBinary)
		// Keep the session alive briefly so the frame flushes.
		s.sendText("ack")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(OpcodeBinary, []byte{1, 2, 3}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage ack: %v", err)
	}
}

func TestReadMessage_ReassemblesFragmentation(t *testing.T) {
	url := startTestServer(t, func(s *serverSession) {
		// Three fragments of one text message, with a ping interleaved
		// between fragments: the reader must auto-pong the ping without
		// losing the in-flight fragmented message.
		s.mu.Lock()
		s.steps = append(s.steps,
			func(c net.Conn, _ *bufio.Reader) {
				writeServerFrameExt(c, OpcodeText, []byte("Hel"), false)
			},
			func(c net.Conn, _ *bufio.Reader) {
				writeServerFrame(c, OpcodePing, []byte("probe"))
			},
			func(c net.Conn, _ *bufio.Reader) {
				writeServerFrameExt(c, OpcodeContinuation, []byte("lo "), false)
			},
			func(c net.Conn, _ *bufio.Reader) {
				writeServerFrameExt(c, OpcodeContinuation, []byte("World"), true)
			},
			// Pong reply space: read the client's pong so the buffered
			// reader drains cleanly for the next step.
			func(_ net.Conn, br *bufio.Reader) {
				if _, _, _, err := readClientFrame(br); err != nil {
					t.Errorf("read pong: %v", err)
				}
			},
		)
		s.mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	op, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != OpcodeText {
		t.Fatalf("op=%d want text", op)
	}
	if want := "Hello World"; string(data) != want {
		t.Fatalf("reassembled payload = %q, want %q", data, want)
	}
}

func TestReadMessage_ServerCloseReturnsCloseError(t *testing.T) {
	url := startTestServer(t, func(s *serverSession) {
		s.mu.Lock()
		s.steps = append(s.steps, func(c net.Conn, _ *bufio.Reader) {
			payload := make([]byte, 2)
			binary.BigEndian.PutUint16(payload, uint16(StatusGoingAway))
			writeServerFrame(c, OpcodeClose, payload)
		})
		s.mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_, _, err = conn.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CloseError, got %v", err)
	}
	if ce.Code != StatusGoingAway {
		t.Fatalf("code=%d want %d", ce.Code, StatusGoingAway)
	}
}

func TestMakeChallengeKey_Uniqueness(t *testing.T) {
	keys := make(map[string]bool)
	for range 5 {
		k, err := makeChallengeKey()
		if err != nil {
			t.Fatalf("makeChallengeKey: %v", err)
		}
		if keys[k] {
			t.Fatalf("duplicate key %q", k)
		}
		keys[k] = true
	}
}

func TestComputeAccept_RFC6455Example(t *testing.T) {
	// RFC 6455 Section 4.2.2 example vectors.
	got := computeAccept("dGhlIHNhbXBsZSBub25jZQ==")
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Fatalf("computeAccept = %q, want %q", got, want)
	}
}

func TestRand_ReadSucceeds(t *testing.T) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
}
