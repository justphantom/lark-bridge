package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateIPCTLS covers the M10-1 validation matrix: pairing, CA
// requiring the pair, file existence, and the non-loopback ⇒ TLS rule.
func TestValidateIPCTLS(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	ca := filepath.Join(dir, "ca.pem")
	for _, p := range []string{cert, key, ca} {
		if err := os.WriteFile(p, []byte("pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		mutate  func(*Core)
		wantErr string
	}{
		{"loopback without TLS ok", func(c *Core) { c.IPCAddr = "127.0.0.1:6060" }, ""},
		{"empty addr without TLS ok", func(c *Core) {}, ""},
		{"non-loopback without TLS rejected", func(c *Core) { c.IPCAddr = "0.0.0.0:6060" }, "non-loopback"},
		{"all-interfaces implicit host rejected", func(c *Core) { c.IPCAddr = ":6060" }, "non-loopback"},
		{"non-loopback with TLS ok", func(c *Core) {
			c.IPCAddr = "0.0.0.0:6060"
			c.IPCTLSCertFile, c.IPCTLSKeyFile = cert, key
		}, ""},
		{"cert without key rejected", func(c *Core) { c.IPCTLSCertFile = cert }, "together"},
		{"key without cert rejected", func(c *Core) { c.IPCTLSKeyFile = key }, "together"},
		{"CA without pair rejected", func(c *Core) { c.IPCTLSClientCAFile = ca }, "requires"},
		{"missing cert file rejected", func(c *Core) {
			c.IPCTLSCertFile, c.IPCTLSKeyFile = filepath.Join(dir, "gone.pem"), key
		}, "ipc_tls_cert_file"},
		{"full mTLS ok", func(c *Core) {
			c.IPCAddr = "10.0.0.5:6060"
			c.IPCTLSCertFile, c.IPCTLSKeyFile, c.IPCTLSClientCAFile = cert, key, ca
		}, ""},
		{"backend client cert without key rejected", func(c *Core) { c.IPCTLSClientCertFile = cert }, "together"},
		{"backend CA file missing rejected", func(c *Core) { c.IPCTLSCAFile = filepath.Join(dir, "gone.pem") }, "ipc_tls_ca_file"},
		{"backend TLS client ok", func(c *Core) {
			c.IPCTLSCAFile = ca
			c.IPCTLSClientCertFile, c.IPCTLSClientKeyFile = cert, key
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Core{}
			tc.mutate(cfg)
			err := validateIPCTLS(cfg)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

// TestLoadWithWarnings_SecretPerm covers low-20: warn only when the file is
// group/other-readable AND carries a plaintext secret.
func TestLoadWithWarnings_SecretPerm(t *testing.T) {
	dir := t.TempDir()

	var seq int
	write := func(body string, mode os.FileMode) string {
		seq++
		p := filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+string(rune('a'+seq))+".json")
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile does not chmod an existing file; make the mode explicit.
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Setenv("CFG_TEST_SECRET", "x")

	cases := []struct {
		name      string
		body      string
		mode      os.FileMode
		wantWarns int
	}{
		{"plaintext secret + loose mode warns", `{"ipc_secret":"abc123"}`, 0o644, 1},
		{"plaintext secret + 0600 silent", `{"ipc_secret":"abc123"}`, 0o600, 0},
		{"env reference + loose mode silent", `{"ipc_secret":"${CFG_TEST_SECRET}"}`, 0o644, 0},
		{"no secrets + loose mode silent", `{"log_level":"info"}`, 0o644, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := write(tc.body, tc.mode)
			_, warns, err := LoadFeishuFrontWithWarnings(path)
			if err != nil {
				t.Fatalf("LoadFeishuFrontWithWarnings: %v", err)
			}
			if len(warns) != tc.wantWarns {
				t.Fatalf("warnings = %v, want %d", warns, tc.wantWarns)
			}
		})
	}
}
