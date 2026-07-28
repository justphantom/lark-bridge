package hostmetrics

import (
	"runtime"
	"testing"
	"time"
)

func TestCollectHost_LinuxSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	h, err := CollectHost(t.TempDir(), time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("CollectHost: %v", err)
	}
	if h.Load1 < 0 || h.Load5 < 0 || h.Load15 < 0 {
		t.Errorf("negative load: %+v", h)
	}
	if h.MemTotalBytes == 0 || h.MemAvailBytes == 0 || h.MemAvailBytes > h.MemTotalBytes {
		t.Errorf("bad mem: %+v", h)
	}
	if h.DiskTotalBytes == 0 || h.DiskUsedBytes > h.DiskTotalBytes {
		t.Errorf("bad disk: %+v", h)
	}
	if h.DiskPath == "" {
		t.Errorf("disk path empty")
	}
	if h.ReportedAt != 1700000000 {
		t.Errorf("ReportedAt = %d, want 1700000000", h.ReportedAt)
	}
}

func TestCollectHost_StatfsFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	// A path that cannot exist → statfs fails → fallback to "/".
	h, err := CollectHost("/nonexistent-dir-\x00", time.Now())
	if err != nil {
		t.Fatalf("CollectHost: %v", err)
	}
	if h.DiskPath != "/" {
		t.Errorf("DiskPath = %q, want /", h.DiskPath)
	}
	if h.DiskTotalBytes == 0 {
		t.Errorf("disk total zero after fallback")
	}
}

func TestSelfCgroupMem_MissingUnit(t *testing.T) {
	// A unit that cannot exist: file absent → ok=false, nil error.
	v, ok, err := SelfCgroupMem("lark-no-such-unit-test.service")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok || v != 0 {
		t.Errorf("got (%d, %v), want (0, false)", v, ok)
	}
	// Empty unit name is a no-op.
	if _, ok, err := SelfCgroupMem(""); err != nil || ok {
		t.Errorf("empty unit: got ok=%v err=%v", ok, err)
	}
}

func TestSelfCgroupMem_LiveService(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	// Best-effort: when running under systemd (e.g. the real deployment) the
	// lark-* unit may be readable; when absent this is a no-op skip.
	v, ok, err := SelfCgroupMem("lark-status-monitor.service")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok && v == 0 {
		t.Errorf("ok but zero memory")
	}
}

func TestOutboundIP(t *testing.T) {
	// UDP dial does a route lookup only; a loopback target always resolves.
	ip, err := OutboundIP("127.0.0.1:6060")
	if err != nil {
		t.Fatalf("OutboundIP: %v", err)
	}
	if ip != "127.0.0.1" {
		t.Errorf("ip = %q, want 127.0.0.1", ip)
	}
}

func TestPrimaryIPv4(t *testing.T) {
	ip := PrimaryIPv4()
	if ip == "" {
		t.Errorf("empty ip")
	}
}
