package backendrpc

import (
	"net"
	"testing"

	"github.com/justphantom/lark-bridge/internal/hostmetrics"
)

// TestProbeOutboundIP_LoopbackFallsBackToPrimary pins the card-identity fix:
// a loopback frontend (localhost / 127.0.0.1 / ::1) must not surface a
// loopback IP — it would never match the frontend's PrimaryIPv4 self-report
// and mergeHostByIP would split one physical host into two rows.
func TestProbeOutboundIP_LoopbackFallsBackToPrimary(t *testing.T) {
	want := hostmetrics.PrimaryIPv4()
	for _, url := range []string{
		"http://localhost:6060",
		"http://127.0.0.1:6060",
		"http://[::1]:6060",
	} {
		got := probeOutboundIP(url)
		if got != want {
			t.Errorf("probeOutboundIP(%q) = %q, want primary %q", url, got, want)
		}
		if ip := net.ParseIP(got); ip != nil && ip.IsLoopback() {
			t.Errorf("probeOutboundIP(%q) = %q, still loopback", url, got)
		}
	}
}

func TestProbeOutboundIP_Unresolvable(t *testing.T) {
	if got := probeOutboundIP("://bad-url"); got != "" {
		t.Errorf("probeOutboundIP(bad) = %q, want empty", got)
	}
}
