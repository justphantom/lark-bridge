// Package websocket is a minimal RFC 6455 WebSocket client.
//
// Only the client subset lark-bridge needs is implemented: outbound masking,
// inbound unmasking, text/binary/close/ping/pong opcodes, and transparent
// fragmentation reassembly on read. permessage-deflate and extensions are not
// supported (the lark WS endpoint does not require them). The API mirrors the
// subset of gorilla/websocket the lark SDK used, so the WS client reads the
// same way.
package websocket

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Dial opens a WebSocket connection to the given URL. The HTTP Upgrade
// handshake is performed manually so no third-party HTTP hijack helper is
// needed; wss:// is transparently wrapped in TLS. header is merged into the
// request (use it for subprotocol/origin etc.); it may be nil.
//
// The returned http.Response is the Upgrade response (status 101) with its
// Body already closed; callers may inspect StatusCode/Header. Ownership of the
// underlying TCP/TLS connection transfers to the returned *Conn.
func Dial(ctx context.Context, rawURL string, header http.Header) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("websocket: parse url: %w", err)
	}
	switch u.Scheme {
	case "ws", "ws+unix":
	case "wss":
	case "":
		return nil, nil, errors.New("websocket: url has no scheme")
	default:
		return nil, nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}

	host := u.Host
	if host == "" {
		if u.Path != "" { // unix domain socket path encoded in URL.Path
			host = u.Path
		} else {
			return nil, nil, errors.New("websocket: url has no host")
		}
	}

	useTLS := u.Scheme == "wss"
	dialNet := "tcp"
	dialAddr := host
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		if useTLS {
			dialAddr = host + ":443"
		} else {
			dialAddr = host + ":80"
		}
	}

	// Honour context deadline for the TCP/TLS+handshake phase.
	d := &net.Dialer{Timeout: 30 * time.Second}
	var nc net.Conn
	if useTLS {
		nc, err = tls.DialWithDialer(d, dialNet, dialAddr, &tls.Config{ServerName: tlsServerName(host)})
	} else {
		nc, err = d.DialContext(ctx, dialNet, dialAddr)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("websocket: dial: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}

	key, err := makeChallengeKey()
	if err != nil {
		_ = nc.Close()
		return nil, nil, err
	}

	req := &http.Request{
		Method:     http.MethodGet,
		URL:        u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       requestHost(u, host),
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// path includes query (the lark URL carries signed query params).
	reqURI := u.RequestURI()
	if reqURI == "" {
		reqURI = "/"
	}

	// Write the request by hand so the conn stays under our control (no
	// http.Client pooling/Close semantics interfering with the upgrade).
	if err := writeUpgradeRequest(nc, req, reqURI); err != nil {
		_ = nc.Close()
		return nil, nil, fmt.Errorf("websocket: write upgrade: %w", err)
	}

	br := bufio.NewReaderSize(nc, 4096)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = nc.Close()
		return nil, nil, fmt.Errorf("websocket: read handshake: %w", err)
	}
	// Clear the dial deadline so the live conn is not bound to it.
	_ = nc.SetDeadline(time.Time{})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		_ = nc.Close()
		return nil, resp, fmt.Errorf("websocket: server returned %d, expected 101", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		_ = resp.Body.Close()
		_ = nc.Close()
		return nil, resp, errors.New("websocket: response lacks Upgrade: websocket")
	}
	accept := resp.Header.Get("Sec-WebSocket-Accept")
	if accept != computeAccept(key) {
		_ = resp.Body.Close()
		_ = nc.Close()
		return nil, resp, errors.New("websocket: bad Sec-WebSocket-Accept")
	}
	// Body is the same conn we keep; close it so callers don't double-read.
	_ = resp.Body.Close()

	c := newConn(nc, br, useTLS)
	return c, resp, nil
}

// tlsServerName returns the SNI host for a wss connection: the bare hostname
// (no port).
func tlsServerName(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// requestHost derives the Host header value, preserving the URL's host (which
// includes port when present).
func requestHost(u *url.URL, fallback string) string {
	if u.Host != "" {
		return u.Host
	}
	return fallback
}

// writeUpgradeRequest serialises the GET request line + headers + CRLF to the
// raw connection. We can't use httputil.DumpRequestOut because it would strip
// the Connection/Upgrade headers for proxied semantics; the wire form is
// simple enough to emit directly.
func writeUpgradeRequest(w io.Writer, req *http.Request, requestURI string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", requestURI)
	fmt.Fprintf(&b, "Host: %s\r\n", req.Host)
	for k, vs := range req.Header {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// makeChallengeKey returns a base64-encoded 16-byte random nonce as required
// by RFC 6455 Section 4.1 (Sec-WebSocket-Key).
func makeChallengeKey() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("websocket: rand: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf[:]), nil
}

// computeAccept returns the expected Sec-WebSocket-Accept value for a given
// client key per RFC 6455 Section 1.3 (append magic GUID, SHA-1, base64).
func computeAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ComputeAccept is the exported form for test servers that need to produce a
// valid handshake response without reimplementing RFC 6455 §1.3.
func ComputeAccept(key string) string { return computeAccept(key) }
