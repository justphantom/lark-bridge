package ipcserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestMetrics_UnregisteredBackendRejected locks the anti-forgery gate: a push
// for an ID with no live SSE connection must not invent a row.
func TestMetrics_UnregisteredBackendRejected(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	body, _ := json.Marshal(protocol.MetricsReport{IP: "10.0.0.1"})
	resp, err := http.Post(ts.URL+"/v1/metrics/ghost", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestMetrics_RoundTrip covers the full pipeline: a backend connects over SSE
// with a version, pushes a MetricsReport, and GET /v1/status aggregates the
// host/service rows plus the frontend's self-report.
func TestMetrics_RoundTrip(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := NewIPCServer(reg, "s")
	srv.SetSelfMetrics(func() (protocol.HostStats, protocol.ServiceStat) {
		return protocol.HostStats{IP: "10.0.0.1", MemTotalBytes: 8 << 30, ReportedAt: 100},
			protocol.ServiceStat{BackendID: "feishu-front", IP: "10.0.0.1", Version: "v1.5.0", ReportedAt: 100}
	})
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client, err := backendrpc.Connect(backendrpc.ConnectOptions{
		BackendID: "b1", BackendType: "claude", FrontendURL: ts.URL, Secret: "s", Version: "v1.4.0",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	err = client.PushMetrics(context.Background(), &protocol.MetricsReport{
		Hostname:   "m2",
		IP:         "10.0.0.2",
		ReportedAt: 200,
		Host: protocol.HostStats{
			Hostname: "m2", Load1: 0.5, MemTotalBytes: 16 << 30, MemAvailBytes: 8 << 30,
			DiskTotalBytes: 50 << 30, DiskUsedBytes: 10 << 30, DiskPath: "/var/lib", ReportedAt: 200,
		},
		Version:        "v1.4.0",
		CgroupMemBytes: 14 << 20,
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer s")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Hosts) != 2 {
		t.Fatalf("hosts = %d, want 2 (backend + self): %+v", len(got.Hosts), got.Hosts)
	}
	var b1Host *protocol.HostStats
	for i := range got.Hosts {
		if got.Hosts[i].IP == "10.0.0.2" {
			b1Host = &got.Hosts[i]
		}
	}
	if b1Host == nil || b1Host.Load1 != 0.5 || b1Host.ReportedAt != 200 {
		t.Errorf("backend host row wrong: %+v", b1Host)
	}

	var b1Svc, selfSvc *protocol.ServiceStat
	for i := range got.Services {
		switch got.Services[i].BackendID {
		case "b1":
			b1Svc = &got.Services[i]
		case "feishu-front":
			selfSvc = &got.Services[i]
		}
	}
	if b1Svc == nil {
		t.Fatalf("b1 service row missing: %+v", got.Services)
	}
	if b1Svc.Version != "v1.4.0" || b1Svc.CgroupMemBytes != 14<<20 || b1Svc.IP != "10.0.0.2" {
		t.Errorf("b1 service row wrong: %+v", b1Svc)
	}
	if selfSvc == nil || selfSvc.Version != "v1.5.0" {
		t.Errorf("self service row wrong: %+v", selfSvc)
	}
}

// TestMergeHostByKey locks the self-row merge under the machine-id-first
// priority: same machine-id collapses (frontend + co-located backend), the
// (IP, Hostname) fallback collapses two rows that share both, and a real
// mismatch (same IP, different hostname, no machine-id) stays distinct.
func TestMergeHostByKey(t *testing.T) {
	// machine-id match: different IP/hostname but same machine → replace.
	hosts := []protocol.HostStats{{IP: "10.0.0.1", Hostname: "backend-view", MachineID: "M"}}
	out := mergeHostByKey(hosts, protocol.HostStats{IP: "10.0.0.2", Hostname: "self-view", MachineID: "M"})
	if len(out) != 1 || out[0].Hostname != "self-view" {
		t.Errorf("machine-id merge = %+v", out)
	}
	// (IP, Hostname) fallback: no machine-id, both fields equal → replace.
	out = mergeHostByKey([]protocol.HostStats{{IP: "10.0.0.9", Hostname: "host-A"}},
		protocol.HostStats{IP: "10.0.0.9", Hostname: "host-A", MemTotalBytes: 1})
	if len(out) != 1 || out[0].MemTotalBytes != 1 {
		t.Errorf("(IP,Hostname) fallback merge = %+v", out)
	}
	// mismatch: same IP, different hostname, no machine-id → append (NAT, distinct hosts).
	out = mergeHostByKey([]protocol.HostStats{{IP: "10.0.0.1", Hostname: "host-A"}},
		protocol.HostStats{IP: "10.0.0.1", Hostname: "host-B"})
	if len(out) != 2 {
		t.Errorf("distinct hosts collapsed = %+v", out)
	}
}

// TestRegistrySnapshot_DedupesHostsByIP locks the (IP, Hostname) fallback:
// two backends on one IP with no machine-id collapse to the latest report.
func TestRegistrySnapshot_DedupesHostsByIP(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	reg.Register("a", "claude")
	reg.Register("b", "opencode")
	_ = reg.SetMetrics("a", &protocol.MetricsReport{IP: "10.0.0.1", ReportedAt: 100,
		Host: protocol.HostStats{Load1: 1}})
	_ = reg.SetMetrics("b", &protocol.MetricsReport{IP: "10.0.0.1", ReportedAt: 200,
		Host: protocol.HostStats{Load1: 2}})

	hosts, services := reg.Snapshot()
	if len(hosts) != 1 {
		t.Fatalf("hosts = %d, want 1: %+v", len(hosts), hosts)
	}
	if hosts[0].ReportedAt != 200 || hosts[0].Load1 != 2 {
		t.Errorf("latest push did not win: %+v", hosts[0])
	}
	if len(services) != 2 {
		t.Errorf("services = %d, want 2", len(services))
	}
}

// TestRegistrySnapshot_DedupesByMachineID locks the primary dedup path: two
// backends reporting the same machine-id fold to one row even if their IPs
// differ (a host with multiple NICs / a re-probed outbound IP).
func TestRegistrySnapshot_DedupesByMachineID(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	reg.Register("a", "claude")
	reg.Register("b", "opencode")
	_ = reg.SetMetrics("a", &protocol.MetricsReport{IP: "10.0.0.1", MachineID: "mid-1", ReportedAt: 100,
		Host: protocol.HostStats{Load1: 1}})
	_ = reg.SetMetrics("b", &protocol.MetricsReport{IP: "10.0.0.2", MachineID: "mid-1", ReportedAt: 200,
		Host: protocol.HostStats{Load1: 2}})

	hosts, _ := reg.Snapshot()
	if len(hosts) != 1 {
		t.Fatalf("hosts = %d, want 1 (same machine-id): %+v", len(hosts), hosts)
	}
	if hosts[0].Load1 != 2 {
		t.Errorf("latest push did not win: %+v", hosts[0])
	}
}

// TestRegistrySnapshot_NATSameIPDistinctMachines locks the NAT fix: two
// backends behind one public IP but with different machine-ids stay as two
// rows (the bug the IP-only dedup had collapsed into one).
func TestRegistrySnapshot_NATSameIPDistinctMachines(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	reg.Register("a", "claude")
	reg.Register("b", "opencode")
	_ = reg.SetMetrics("a", &protocol.MetricsReport{IP: "203.0.113.1", MachineID: "mid-1", ReportedAt: 100,
		Host: protocol.HostStats{Hostname: "host-A", Load1: 1}})
	_ = reg.SetMetrics("b", &protocol.MetricsReport{IP: "203.0.113.1", MachineID: "mid-2", ReportedAt: 200,
		Host: protocol.HostStats{Hostname: "host-B", Load1: 2}})

	hosts, _ := reg.Snapshot()
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2 (distinct machines behind NAT): %+v", len(hosts), hosts)
	}
}

// TestRegistrySnapshot_NoMetricsYet: a freshly registered backend with no push
// yields a service row with only its version, and no host row.
func TestRegistrySnapshot_NoMetricsYet(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	reg.Register("a", "claude")
	reg.SetVersion("a", "v1.2.3")
	hosts, services := reg.Snapshot()
	if len(hosts) != 0 {
		t.Errorf("hosts = %+v, want none", hosts)
	}
	if len(services) != 1 || services[0].Version != "v1.2.3" || services[0].ReportedAt != 0 {
		t.Errorf("services = %+v", services)
	}
}

// TestMetrics_OversizedBodyRejected caps abuse: the 64KiB limit must trip.
func TestMetrics_OversizedBodyRejected(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	reg.Register("b1", "claude")
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	big := bytes.Repeat([]byte("x"), maxMetricsBody+1)
	resp, err := http.Post(ts.URL+"/v1/metrics/b1", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestPushMetrics_OldFrontend404: a frontend without the endpoint answers
// 404; PushMetrics surfaces it as an error the metrics loop logs and skips —
// never a panic, never a reconnect.
func TestPushMetrics_OldFrontend404(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	client, err := backendrpc.ConnectWithHTTPClient(
		backendrpc.ConnectOptions{BackendID: "b1", BackendType: "claude", FrontendURL: ts.URL}, ts.Client())
	if err == nil {
		client.Close()
		t.Fatalf("connect to 404 server should fail the SSE handshake")
	}
	// PushMetrics against a bare 404 server (no SSE) via a hand-built client:
	// use a metrics-capable server that only 404s /v1/metrics.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	ts2 := httptest.NewServer(mux)
	defer ts2.Close()
	c2, err := backendrpc.ConnectWithHTTPClient(
		backendrpc.ConnectOptions{BackendID: "b1", BackendType: "claude", FrontendURL: ts2.URL}, ts2.Client())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c2.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c2.PushMetrics(ctx, &protocol.MetricsReport{}); err == nil {
		t.Fatalf("want 404 error")
	}
}
