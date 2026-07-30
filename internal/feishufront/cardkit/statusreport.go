package cardkit

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// maxTurnsPerBackend caps how many in-flight turns are listed per backend on
// the overview card. A pathological cluster (dozens of turns on one backend)
// would bloat the card toward Feishu's element/size caps (11310/230025); the
// tail collapses to "…另 N 条". 5 keeps the card scannable while identifiable.
const maxTurnsPerBackend = 5

// staleAfterIntervals is how many refresh periods a host/service row may go
// without a new metrics report before it is marked "(stale)" — e.g. a backend
// whose metrics channel broke (old frontend 404) while SSE stays online.
const staleAfterIntervals = 3

// TurnRow is the cardkit view of one in-flight turn for StatusReport. It
// mirrors protocol.TurnInfo's display fields without importing protocol, so
// cardkit stays a pure rendering layer (same convention as Notice taking only
// primitive args).
type TurnRow struct {
	BackendID string
	ChatID    string // full Feishu chat id; rendered via ShortID
	ElapsedS  int64
}

// HostRow is the cardkit view of one host's load snapshot (protocol.HostStats
// without the import). ReportedAt drives the "(stale)" marker.
type HostRow struct {
	IP             string
	Hostname       string // 仅在同 IP 多行时追加到身份行作区分
	Load1          float64
	Load5          float64
	Load15         float64
	MemTotalBytes  uint64
	MemAvailBytes  uint64
	DiskTotalBytes uint64
	DiskUsedBytes  uint64
	ReportedAt     int64
}

// ServiceRow is the cardkit view of one backend process's snapshot
// (protocol.ServiceStat without the import). CgroupMemBytes == 0 renders "—"
// (no instance, or unreadable cgroup); an empty Version renders "unknown" and
// is excluded from drift detection.
type ServiceRow struct {
	BackendID      string
	IP             string
	Version        string
	CgroupMemBytes uint64
	ReportedAt     int64
}

// StatusReportInput carries everything StatusReport renders. An options
// struct (not positional params) because the host/service sections took the
// arity past readability; mirrors the TurnRow convention of cardkit-local
// view types, with the dispatcher doing protocol→view conversion.
type StatusReportInput struct {
	Footer      FooterInfo
	Title       string
	GeneratedAt int64
	IntervalS   int
	InFlight    int
	Backends    []string
	Turns       []TurnRow
	Hosts       []HostRow
	Services    []ServiceRow
}

// ShortID shortens a Feishu id (oc_/om_ + hex) to its last 8 chars so a row
// stays scannable while remaining identifiable. Exported because the
// status-report renderer and any future caller share the same truncation rule.
func ShortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return "…" + id[len(id)-8:]
}

// StatusReport builds the standing overview card pushed by the status-monitor
// backend: a two-line summary (updated time · period / online backends ·
// in-flight count), optional 主机/进程 sections (grouped layout — one bold
// identity line per host/service plus indented metric lines, so nothing
// wraps mid-token on mobile), then per-backend turn groups. A version drift
// (one backend behind the dominant version) marks the group and flips the
// header template from blue to orange. schema 1.0 via Card, same as every
// other card.
func StatusReport(in StatusReportInput) ([]byte, error) {
	_, drifted := versionDrift(in.Services)
	template := "blue"
	if len(drifted) > 0 {
		template = "orange"
	}
	info := HeaderInfo{BackendType: in.Footer.BackendType, Title: in.Title, Template: template}
	if info.Title == "" {
		info.Title = "状态总览"
	}

	var b strings.Builder
	// Summary: two short lines — the one-line form (~30 chars) already hugs
	// the right edge on a phone, so 在线/会话 gets its own line.
	fmt.Fprintf(&b, "更新 %s", time.Unix(in.GeneratedAt, 0).Format("2006-01-02 15:04:05"))
	if in.IntervalS > 0 {
		fmt.Fprintf(&b, " · 周期 %s", formatPeriod(in.IntervalS))
	}
	fmt.Fprintf(&b, "\n在线后端 %d · 会话 %d", len(in.Backends), in.InFlight)

	writeHostSection(&b, in.Hosts, in.GeneratedAt, in.IntervalS)
	writeServiceSection(&b, in.Services, drifted, in.GeneratedAt, in.IntervalS)

	if in.InFlight == 0 || len(in.Turns) == 0 {
		b.WriteString("\n\n当前没有运行中的会话。")
		if len(in.Backends) > 0 {
			b.WriteString("\n\n在线后端：" + strings.Join(in.Backends, " · "))
		}
	} else {
		groups := groupTurns(in.Backends, in.Turns)
		for _, g := range groups {
			fmt.Fprintf(&b, "\n\n▸ %s · %d 个会话", g.backendID, len(g.rows))
			shown := g.rows
			tail := 0
			if len(shown) > maxTurnsPerBackend {
				shown = shown[:maxTurnsPerBackend]
				tail = len(g.rows) - maxTurnsPerBackend
			}
			for _, r := range shown {
				fmt.Fprintf(&b, "\n　　%s  已运行 %s", ShortID(r.ChatID), FormatElapsed(time.Duration(r.ElapsedS)*time.Second))
			}
			if tail > 0 {
				fmt.Fprintf(&b, "\n　　…另 %d 条", tail)
			}
		}
	}

	md := truncateRunes(b.String(), MaxBodyRunes)
	elements := []Element{MarkdownElement(md)}
	return Card(info, in.Footer, elements, nil)
}

// writeHostSection renders the ▸ 主机 block: one group per host, sorted by
// (IP, hostname) for deterministic output — hostname breaks ties so multiple
// hosts behind one NAT IP (same IP, different machine-id) keep a stable order.
// Layout is grouped + one-metric-per-line (an identity line in bold carrying
// the stale mark, then load/mem/disk each on its own indented line) because
// Feishu markdown uses a proportional font — space-padded columns never align,
// and a ~70-char single line wraps at arbitrary points on mobile, splitting
// numbers mid-token. Empty IP renders "?" — a missing probe is display-only
// and must not blank the group.
//
// When two or more rows share an IP (the NAT case: distinct machine-ids behind
// one public IP), the identity line appends " · <hostname>" so each row stays
// distinguishable; a unique-IP row renders the bare IP as before.
func writeHostSection(b *strings.Builder, hosts []HostRow, now int64, intervalS int) {
	if len(hosts) == 0 {
		return
	}
	sorted := make([]HostRow, len(hosts))
	copy(sorted, hosts)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].IP != sorted[j].IP {
			return sorted[i].IP < sorted[j].IP
		}
		return sorted[i].Hostname < sorted[j].Hostname
	})
	// Ambiguous IPs (≥2 rows) get a hostname suffix on the identity line.
	ipCount := make(map[string]int, len(sorted))
	for _, h := range sorted {
		ipCount[h.IP]++
	}
	b.WriteString("\n\n▸ 主机")
	for _, h := range sorted {
		ip := h.IP
		if ip == "" {
			ip = "?"
		}
		identity := ip
		if ipCount[h.IP] > 1 && h.Hostname != "" {
			identity = ip + " · " + h.Hostname
		}
		memPct, diskPct := 0, 0
		if h.MemTotalBytes > 0 {
			memPct = int((h.MemTotalBytes - h.MemAvailBytes) * 100 / h.MemTotalBytes) //nolint:gosec // G115: 比值 ∈ [0,100]
		}
		if h.DiskTotalBytes > 0 {
			diskPct = int(h.DiskUsedBytes * 100 / h.DiskTotalBytes) //nolint:gosec // G115: 比值 ∈ [0,100]
		}
		fmt.Fprintf(b, "\n\n　**%s**%s", identity, staleMark(h.ReportedAt, now, intervalS))
		fmt.Fprintf(b, "\n　　load  %.2f / %.2f / %.2f", h.Load1, h.Load5, h.Load15)
		fmt.Fprintf(b, "\n　　内存  %s / %s (%d%%)",
			formatBytes(h.MemTotalBytes-h.MemAvailBytes), formatBytes(h.MemTotalBytes), memPct)
		fmt.Fprintf(b, "\n　　磁盘  %s / %s (%d%%)",
			formatBytes(h.DiskUsedBytes), formatBytes(h.DiskTotalBytes), diskPct)
	}
}

// writeServiceSection renders the ▸ 进程 block: one group per backend,
// sorted by backendID. The identity line (bold backendID + cgroup memory)
// carries the drift/stale marks so they can never wrap away from their
// owner; IP and the full (unclipped) version share the indented detail
// line. Drifted rows (version != dominant) carry the 🔴 marker.
func writeServiceSection(b *strings.Builder, services []ServiceRow, drifted map[string]bool, now int64, intervalS int) {
	if len(services) == 0 {
		return
	}
	sorted := make([]ServiceRow, len(services))
	copy(sorted, services)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].BackendID < sorted[j].BackendID })
	b.WriteString("\n\n▸ 进程")
	for _, s := range sorted {
		version := s.Version
		if version == "" {
			version = "unknown"
		}
		ip := s.IP
		if ip == "" {
			ip = "?"
		}
		mem := "—"
		if s.CgroupMemBytes > 0 {
			mem = formatBytes(s.CgroupMemBytes)
		}
		fmt.Fprintf(b, "\n\n　**%s** · %s", s.BackendID, mem)
		if drifted[s.BackendID] {
			b.WriteString("  🔴 版本漂移")
		}
		b.WriteString(staleMark(s.ReportedAt, now, intervalS))
		fmt.Fprintf(b, "\n　　%s · %s", ip, version)
	}
}

// versionDrift finds the dominant (mode) version across services with a
// non-empty version and the set of backendIDs that disagree. Drift detection
// only runs when ≥2 backends report a version — a single backend, or all
// "unknown", has nothing to drift from. Ties break to the lexicographically
// smallest version so the verdict is deterministic.
func versionDrift(services []ServiceRow) (dominant string, drifted map[string]bool) {
	counts := map[string]int{}
	for _, s := range services {
		if s.Version != "" {
			counts[s.Version]++
		}
	}
	if len(counts) == 0 {
		return "", nil
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total < 2 {
		return "", nil
	}
	best, bestN := "", 0
	for v, n := range counts {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	drifted = map[string]bool{}
	for _, s := range services {
		if s.Version != "" && s.Version != best {
			drifted[s.BackendID] = true
		}
	}
	return best, drifted
}

// staleMark returns "  (stale)" when the row's last report is older than
// staleAfterIntervals × the refresh period. A zero ReportedAt (metrics never
// pushed) or non-positive interval never marks — there is no cadence to be
// stale against.
func staleMark(reportedAt, now int64, intervalS int) string {
	if reportedAt == 0 || intervalS <= 0 {
		return ""
	}
	if now-reportedAt > int64(staleAfterIntervals)*int64(intervalS) {
		return "  (stale)"
	}
	return ""
}

// formatBytes renders a byte count as "1.7G" / "14M" / "512K": one decimal,
// trailing ".0" trimmed, so columns stay tight on the card.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit && exp < 3; x /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	s := fmt.Sprintf("%.1f", v)
	s = strings.TrimSuffix(s, ".0")
	return fmt.Sprintf("%s%c", s, "KMGT"[exp])
}

// turnGroup is one backend's in-flight turns, sorted by elapsed desc.
type turnGroup struct {
	backendID string
	rows      []TurnRow
}

// groupTurns partitions turns by BackendID. Online backends (from the
// snapshot's Backends list) appear first in their given order so the card
// reads top-to-bottom by familiarity; turns whose backend is offline (in
// Turns but not Backends) follow. Within a group, longest-running first so
// the longest-running turn is the easiest to spot.
func groupTurns(backends []string, turns []TurnRow) []turnGroup {
	byBackend := map[string][]TurnRow{}
	var order []string
	seen := map[string]struct{}{}
	for _, bid := range backends {
		if _, ok := seen[bid]; !ok {
			seen[bid] = struct{}{}
			order = append(order, bid)
		}
	}
	for _, t := range turns {
		if _, ok := seen[t.BackendID]; !ok {
			seen[t.BackendID] = struct{}{}
			order = append(order, t.BackendID)
		}
		byBackend[t.BackendID] = append(byBackend[t.BackendID], t)
	}
	out := make([]turnGroup, 0, len(order))
	for _, bid := range order {
		rows := byBackend[bid]
		sort.Slice(rows, func(i, j int) bool { return rows[i].ElapsedS > rows[j].ElapsedS })
		if len(rows) > 0 {
			out = append(out, turnGroup{backendID: bid, rows: rows})
		}
	}
	return out
}

// formatPeriod renders an interval (seconds) as "60s" / "5m" / "1h30m" for
// the summary line, mirroring FormatElapsed's compactness.
func formatPeriod(seconds int) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
	}
}
