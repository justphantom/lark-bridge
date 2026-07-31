package backendrpc

import (
	"strings"
	"testing"
)

// TestValidateBackendConfig_OK: a fully populated config passes.
func TestValidateBackendConfig_OK(t *testing.T) {
	if err := ValidateBackendConfig("s", "b", "http://localhost:6060"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidateBackendConfig_URLScheme (C12): frontend_url must be an
// absolute http(s) URL with a host; scheme-less and exotic-scheme values
// are rejected at startup with a clear error.
func TestValidateBackendConfig_URLScheme(t *testing.T) {
	bad := []string{
		"localhost:6060",  // no scheme — parses with empty Scheme
		"ftp://host:6060", // exotic scheme
		"http://",         // no host
		"://garbage",      // unparseable
	}
	for _, u := range bad {
		if err := ValidateBackendConfig("s", "b", u); err == nil {
			t.Errorf("frontend_url %q accepted, want rejection", u)
		}
	}
	for _, u := range []string{"http://localhost:6060", "https://front.example.com"} {
		if err := ValidateBackendConfig("s", "b", u); err != nil {
			t.Errorf("frontend_url %q rejected: %v", u, err)
		}
	}
}

// TestValidateBackendConfig_BlankFields: each blank field yields its own
// error mentioning the field name, in the order secret → id → url.
func TestValidateBackendConfig_BlankFields(t *testing.T) {
	cases := []struct {
		secret, id, url, want string
	}{
		{"", "b", "http://h", "ipc_secret"},
		{"s", "", "http://h", "backend_id"},
		{"s", "b", "", "frontend_url"},
	}
	for _, c := range cases {
		err := ValidateBackendConfig(c.secret, c.id, c.url)
		if err == nil {
			t.Errorf("ValidateBackendConfig(%q,%q,%q) = nil; want error mentioning %q",
				c.secret, c.id, c.url, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("error %q does not mention %q", err.Error(), c.want)
		}
	}
}

// TestValidateBackendConfig_PrioritySecretFirst: when multiple fields are
// blank, the secret error wins (it is checked first), so the operator
// sees the most fundamental fix first.
func TestValidateBackendConfig_PrioritySecretFirst(t *testing.T) {
	err := ValidateBackendConfig("", "", "")
	if err == nil || !strings.Contains(err.Error(), "ipc_secret") {
		t.Fatalf("expected ipc_secret error first, got: %v", err)
	}
}
