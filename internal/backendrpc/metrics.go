package backendrpc

import (
	"context"
	"net"
	"net/url"
	"time"

	"github.com/justphantom/lark-bridge/internal/hostmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// defaultMetricsInterval mirrors status-monitor's 60s default; applied when
// MetricsOptions.Interval is non-positive (config absent).
const defaultMetricsInterval = 60 * time.Second

// MetricsOptions configures StartMetricsLoop.
type MetricsOptions struct {
	// Interval is the push period — cfg.StatusMonitor.Interval, the single
	// source of truth shared with the status-monitor refresh cadence.
	Interval time.Duration
	// StateDir is the disk-usage mount point source (fallback "/").
	StateDir string
	// UnitName is the systemd unit whose cgroup v2 memory.current is sampled
	// (e.g. "lark-claude-back.service"). Empty skips the sample.
	UnitName string
	// Version is the binary's ldflags-injected build version. Redundant with
	// the SSE handshake; lets the frontend cross-check/fall back.
	Version string
	// Logger receives debug-level push failures; nil → no-op.
	Logger *log.Logger
	// RunningSessions, when non-nil, returns the backend's current in-flight
	// turns. The snapshot is attached to every MetricsReport so the frontend
	// can reconcile its running-session set even if TypeTurnStarted/Finished
	// controls are lost.
	RunningSessions func() []protocol.TurnInfo
}

// StartMetricsLoop probes the outbound IP once, pushes one MetricsReport
// immediately (so the frontend's first interval is not blank), then pushes
// every Interval until ctx is cancelled. It never returns an error: a 404
// from an old frontend (endpoint missing) or any transport failure is logged
// at debug and retried next tick — metrics are best-effort and must never
// disturb the SSE/control paths.
func StartMetricsLoop(ctx context.Context, c *Client, opts MetricsOptions) {
	logger := opts.Logger
	if logger == nil {
		logger = log.Nop()
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultMetricsInterval
	}
	ip := probeOutboundIP(c.frontendURL)
	push := func() {
		report := collectMetrics(opts, ip)
		if err := c.PushMetrics(ctx, report); err != nil {
			logger.Debug("metrics push skipped", log.FieldError, err)
		}
	}
	push()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			push()
		}
	}
}

// collectMetrics assembles one report. Host-collection failure (e.g. /proc
// unreadable — not expected on Linux) zeroes the host fields; the push still
// happens so the frontend sees the process row.
func collectMetrics(opts MetricsOptions, ip string) *protocol.MetricsReport {
	now := time.Now()
	host, _ := hostmetrics.CollectHost(opts.StateDir, now)
	cg, ok, _ := hostmetrics.SelfCgroupMem(opts.UnitName)
	if !ok {
		cg = 0
	}
	report := &protocol.MetricsReport{
		Hostname:       host.Hostname,
		IP:             ip,
		MachineID:      host.MachineID, // 置于顶层：去重层直接取用，与 Host.MachineID 同值
		ReportedAt:     now.Unix(),
		Host:           host,
		Version:        opts.Version,
		CgroupMemBytes: cg,
	}
	if opts.RunningSessions != nil {
		report.Turns = opts.RunningSessions()
	}
	return report
}

// probeOutboundIP resolves the frontend's host:port and dials (UDP route
// lookup only) to learn which local IP the frontend sees. A loopback result
// means the frontend is co-located with this process; the loopback address is
// useless as a card identity (it would never match the frontend's own
// PrimaryIPv4 self-report and split one host into two rows), so fall back to
// the host's primary IPv4. Best-effort: "" when unresolvable — the IP is
// display-only.
func probeOutboundIP(frontendURL string) string {
	u, err := url.Parse(frontendURL)
	if err != nil || u.Host == "" {
		return ""
	}
	ip, err := hostmetrics.OutboundIP(u.Host)
	if err != nil {
		return ""
	}
	if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
		return hostmetrics.PrimaryIPv4()
	}
	return ip
}
