package feishufront

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSelfSignedCert generates a throwaway self-signed cert/key pair for
// 127.0.0.1 and returns their paths.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "lark-bridge-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// freeTCPPort reserves and releases an ephemeral loopback port. The race
// between release and re-bind is acceptable for tests.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestListen_NonLoopbackRequiresTLS locks in M10-1: a non-loopback bind with
// no TLS configured is refused (bearer would cross the network in cleartext).
func TestListen_NonLoopbackRequiresTLS(t *testing.T) {
	s := NewIPCServer(NewBackendRegistry(), "secret")
	err := s.Listen("0.0.0.0:0")
	if err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("Listen non-loopback without TLS = %v, want TLS-required error", err)
	}
}

// TestListen_NonLoopbackRequiresSecretStillEnforced verifies the pre-existing
// rule survives the TLS addition (TLS does not replace auth).
func TestListen_NonLoopbackRequiresSecretStillEnforced(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	s := NewIPCServer(NewBackendRegistry(), "")
	s.SetTLS(cert, key, "")
	err := s.Listen("0.0.0.0:0")
	if err == nil || !strings.Contains(err.Error(), "ipc_secret") {
		t.Fatalf("Listen non-loopback with TLS but no secret = %v, want secret-required error", err)
	}
}

// TestListen_TLS serves HTTPS end-to-end on loopback: a TLS client gets
// /v1/status; a plaintext HTTP client is rejected by the handshake.
func TestListen_TLS(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	s := NewIPCServer(NewBackendRegistry(), "")
	s.SetTLS(cert, key, "")
	addr := freeTCPPort(t)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Listen(addr) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only self-signed cert
	}}
	var resp *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for {
		var err error
		resp, err = client.Get("https://" + addr + "/v1/status")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TLS listener never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/status over TLS = %d: %s", resp.StatusCode, body)
	}

	// Plaintext HTTP against the TLS listener must not serve: the handshake
	// either errors outright or the server answers a 400 over the bare
	// connection — never a 200 with the API payload.
	if presp, err := http.Get("http://" + addr + "/v1/status"); err == nil {
		defer func() { _ = presp.Body.Close() }()
		if presp.StatusCode == http.StatusOK {
			t.Fatal("plaintext HTTP against TLS listener got a 200 response")
		}
	}
}

// TestListen_BadClientCA verifies a garbage CA file is rejected at startup
// rather than mid-handshake.
func TestListen_BadClientCA(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	dir := t.TempDir()
	badCA := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(badCA, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewIPCServer(NewBackendRegistry(), "")
	s.SetTLS(cert, key, badCA)
	err := s.Listen(freeTCPPort(t))
	if err == nil || !strings.Contains(err.Error(), "client_ca") {
		t.Fatalf("Listen with garbage CA = %v, want CA error", err)
	}
}
