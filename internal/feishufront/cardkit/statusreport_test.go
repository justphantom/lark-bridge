package cardkit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStatusReport_RenderAndGroups(t *testing.T) {
	footer := FooterInfo{BackendID: "status-1", BackendType: "status-monitor", Status: "总览"}
	turns := []TurnRow{
		{BackendID: "claude-1", ChatID: "oc_1111111111111111aaaaaaaaaaaaaaaa", ElapsedS: 30},
		{BackendID: "claude-1", ChatID: "oc_2222222222222222bbbbbbbbbbbbbbbb", ElapsedS: 745}, // 12m25s, longest first
		{BackendID: "opencode-1", ChatID: "oc_3333333333333333cccccccccccccccc", ElapsedS: 5},
	}
	card, err := StatusReport(StatusReportInput{
		Footer: footer, Title: "总览", GeneratedAt: 1700000000, IntervalS: 60,
		InFlight: 2, Backends: []string{"claude-1", "opencode-1", "status-1"}, Turns: turns,
	})
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(card, &m); err != nil {
		t.Fatalf("invalid card json: %v", err)
	}
	if m["schema"] != "2.0" {
		t.Errorf("schema = %v, want 2.0", m["schema"])
	}
	body, _ := m["body"].(map[string]any)
	if body == nil {
		t.Fatal("schema 2.0 card must carry body")
	}
	elems, _ := body["elements"].([]any)
	if len(elems) < 2 {
		t.Fatalf("want ≥2 elements (body+footer), got %d", len(elems))
	}
	md, _ := elems[0].(map[string]any)["content"].(string)
	if !strings.Contains(md, "在线后端 3 · 会话 2") {
		t.Errorf("summary missing; body=%q", md)
	}
	if !strings.Contains(md, "12m25s") {
		t.Errorf("elapsed 745s not rendered as 12m25s; body=%q", md)
	}
	if !strings.Contains(md, "…aaaaaaaa") {
		t.Errorf("short id (last 8) missing; body=%q", md)
	}
	// opencode-1 group present too.
	if !strings.Contains(md, "opencode-1") {
		t.Errorf("opencode-1 group missing; body=%q", md)
	}
}

func TestStatusReport_IdleNoTurns(t *testing.T) {
	card, err := StatusReport(StatusReportInput{
		Footer: FooterInfo{BackendType: "status-monitor"}, GeneratedAt: 1700000000,
		IntervalS: 60, Backends: []string{"claude-1"},
	})
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	var m map[string]any
	json.Unmarshal(card, &m)
	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)
	md, _ := elems[0].(map[string]any)["content"].(string)
	if !strings.Contains(md, "当前没有运行中的会话") {
		t.Errorf("idle body missing; body=%q", md)
	}
	if !strings.Contains(md, "在线后端：claude-1") {
		t.Errorf("online list missing; body=%q", md)
	}
}

func TestStatusReport_TruncatesHeavyBackend(t *testing.T) {
	turns := make([]TurnRow, maxTurnsPerBackend+3)
	for i := range turns {
		turns[i] = TurnRow{BackendID: "claude-1", ChatID: "oc_x", ElapsedS: int64(i)}
	}
	card, err := StatusReport(StatusReportInput{
		Footer: FooterInfo{BackendType: "status-monitor"}, Title: "t", GeneratedAt: 1,
		IntervalS: 60, InFlight: len(turns), Backends: []string{"claude-1"}, Turns: turns,
	})
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	var m map[string]any
	json.Unmarshal(card, &m)
	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)
	md, _ := elems[0].(map[string]any)["content"].(string)
	if !strings.Contains(md, "…另 3 条") {
		t.Errorf("tail collapse missing; body=%q", md)
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("oc_abcdef1234567890"); got != "…34567890" {
		t.Errorf("ShortID = %q, want …34567890", got)
	}
	if got := ShortID("short"); got != "short" {
		t.Errorf("ShortID(short) = %q, want short", got)
	}
}

func cardBody(t *testing.T, card []byte) (md string, m map[string]any) {
	t.Helper()
	if err := json.Unmarshal(card, &m); err != nil {
		t.Fatalf("invalid card json: %v", err)
	}
	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)
	if len(elems) == 0 {
		t.Fatalf("no elements")
	}
	md, _ = elems[0].(map[string]any)["content"].(string)
	return md, m
}

func TestStatusReport_HostAndServiceSections(t *testing.T) {
	const now = 1700000000
	card, err := StatusReport(StatusReportInput{
		Footer: FooterInfo{BackendType: "status-monitor"}, GeneratedAt: now, IntervalS: 60,
		Backends: []string{"claude-1", "status-1"},
		Hosts: []HostRow{
			{
				IP: "192.168.1.10", Load1: 1.42, Load5: 1.10, Load15: 0.95,
				MemTotalBytes: 16 << 30, MemAvailBytes: 11 << 30,
				DiskTotalBytes: 50 << 30, DiskUsedBytes: 12 << 30,
				ReportedAt: now,
			},
			{
				IP: "192.168.1.5", Load1: 0.95, Load5: 0.38, Load15: 0.33,
				MemTotalBytes: 8 << 30, MemAvailBytes: 6 << 30,
				DiskTotalBytes: 47 << 30, DiskUsedBytes: 26 << 30,
				ReportedAt: now - 400, // 400s > 3×60s → stale
			},
		},
		Services: []ServiceRow{
			{BackendID: "claude-1", IP: "192.168.1.10", Version: "v1.5.0", CgroupMemBytes: 14 << 20, ReportedAt: now},
			{BackendID: "miniagent-1", IP: "192.168.1.10", Version: "v1.5.0", CgroupMemBytes: 0, ReportedAt: now},
			{BackendID: "status-1", IP: "192.168.1.5", Version: "v1.4.0", CgroupMemBytes: 8 << 20, ReportedAt: now},
		},
	})
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	md, m := cardBody(t, card)
	for _, want := range []string{
		"▸ 主机", "▸ 进程",
		"**192.168.1.5**", "**192.168.1.10**",
		"load  0.95 / 0.38 / 0.33",
		"2G / 8G (25%)", "12G / 50G (24%)",
		"**claude-1** · 14M",
		"192.168.1.10 · v1.5.0",
		"—",       // miniagent cgroup 0
		"🔴 版本漂移",  // status-1 behind the dominant v1.5.0
		"(stale)", // 192.168.1.5 host row
	} {
		if !strings.Contains(md, want) {
			t.Errorf("body missing %q; body=%q", want, md)
		}
	}
	// Drift flips the header template to orange.
	header, _ := m["header"].(map[string]any)
	if header["template"] != "orange" {
		t.Errorf("template = %v, want orange", header["template"])
	}
	// Hosts sorted by IP lexicographically: ".10" precedes ".5" (per design).
	if strings.Index(md, "192.168.1.10") > strings.Index(md, "192.168.1.5") {
		t.Errorf("hosts not sorted by IP; body=%q", md)
	}
	// Grouped layout: stale mark stays on the host identity line (no wrap
	// ambiguity), and each metric sits on its own indented line.
	if !strings.Contains(md, "**192.168.1.5**  (stale)\n　　load") {
		t.Errorf("host group layout broken; body=%q", md)
	}
}

func TestStatusReport_ServiceGroupLayout(t *testing.T) {
	const now = 1700000000
	card, err := StatusReport(StatusReportInput{
		Footer: FooterInfo{BackendType: "status-monitor"}, GeneratedAt: now, IntervalS: 60,
		Backends: []string{"a"},
		Services: []ServiceRow{
			{BackendID: "a", IP: "10.0.0.1", Version: "v1.4.0-2-g5fcc235", CgroupMemBytes: 12 << 20, ReportedAt: now},
		},
	})
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	md, _ := cardBody(t, card)
	// Full (unclipped) version on the detail line, mem on the identity line.
	if !strings.Contains(md, "**a** · 12M\n　　10.0.0.1 · v1.4.0-2-g5fcc235") {
		t.Errorf("service group layout broken; body=%q", md)
	}
}

func TestStatusReport_NoDriftWhenUniform(t *testing.T) {
	const now = 1700000000
	card, err := StatusReport(StatusReportInput{
		Footer: FooterInfo{BackendType: "status-monitor"}, GeneratedAt: now, IntervalS: 60,
		Backends: []string{"a", "b"},
		Services: []ServiceRow{
			{BackendID: "a", Version: "v1.5.0", ReportedAt: now},
			{BackendID: "b", Version: "v1.5.0", ReportedAt: now},
			{BackendID: "c", Version: "", ReportedAt: now}, // unknown: excluded, not drifted
		},
	})
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	md, m := cardBody(t, card)
	if strings.Contains(md, "版本漂移") {
		t.Errorf("unexpected drift mark; body=%q", md)
	}
	if !strings.Contains(md, "unknown") {
		t.Errorf("empty version not rendered as unknown; body=%q", md)
	}
	header, _ := m["header"].(map[string]any)
	if header["template"] != "blue" {
		t.Errorf("template = %v, want blue", header["template"])
	}
}

func TestVersionDrift(t *testing.T) {
	// Single backend: no drift possible.
	if _, drifted := versionDrift([]ServiceRow{{BackendID: "a", Version: "v1"}}); len(drifted) != 0 {
		t.Errorf("single backend drifted: %v", drifted)
	}
	// Tie breaks to the lexicographically smallest version (deterministic).
	dominant, drifted := versionDrift([]ServiceRow{
		{BackendID: "a", Version: "v2"},
		{BackendID: "b", Version: "v1"},
	})
	if dominant != "v1" || !drifted["a"] || drifted["b"] {
		t.Errorf("tie: dominant=%q drifted=%v", dominant, drifted)
	}
}

func TestStaleMark(t *testing.T) {
	if got := staleMark(1000, 1000+3*60+1, 60); got == "" {
		t.Errorf("want stale beyond 3×interval")
	}
	if got := staleMark(1000, 1000+3*60, 60); got != "" {
		t.Errorf("boundary should not be stale: %q", got)
	}
	if got := staleMark(0, 999999, 60); got != "" {
		t.Errorf("zero ReportedAt should not be stale: %q", got)
	}
	if got := staleMark(1000, 999999, 0); got != "" {
		t.Errorf("zero interval should not be stale: %q", got)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[uint64]string{
		0:               "0B",
		512:             "512B",
		14 << 20:        "14M",
		3<<20 + 102<<10: "3.1M",
		1700 << 20:      "1.7G",
		16 << 30:        "16G",
		47 << 30:        "47G",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestStatusReport_SummaryTimeFormat locks the "更新" line to the full
// yyyy-mm-dd HH:MM:ss form (it was HH:MM:SS only), computed via the same
// time.Unix().Format call the renderer uses so the assertion is TZ-independent.
func TestStatusReport_SummaryTimeFormat(t *testing.T) {
	const generatedAt = 1700000000
	card, err := StatusReport(StatusReportInput{
		Footer: FooterInfo{BackendType: "status-monitor"}, GeneratedAt: generatedAt, IntervalS: 60,
		Backends: []string{"a"},
	})
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	md, _ := cardBody(t, card)
	want := "更新 " + time.Unix(generatedAt, 0).Format("2006-01-02 15:04:05")
	if !strings.Contains(md, want) {
		t.Errorf("summary time %q missing; body=%q", want, md)
	}
	// The old HH:MM:SS-only form must not appear bare (it would if the format
	// string regressed). The full form contains it as a substring, so check the
	// update prefix specifically.
	if strings.Contains(md, "更新 "+time.Unix(generatedAt, 0).Format("15:04:05")+"\n") {
		t.Errorf("summary regressed to time-only format; body=%q", md)
	}
}

// TestStatusReport_HostSectionNATDisambiguation locks the display enhancement:
// two hosts sharing one IP (the NAT case) each get a " · <hostname>" suffix on
// the identity line, while a unique-IP host renders the bare IP as before.
func TestStatusReport_HostSectionNATDisambiguation(t *testing.T) {
	card, err := StatusReport(StatusReportInput{
		Footer: FooterInfo{BackendType: "status-monitor"}, GeneratedAt: 1700000000, IntervalS: 60,
		Backends: []string{"a"},
		Hosts: []HostRow{
			{IP: "203.0.113.1", Hostname: "host-A", MemTotalBytes: 8 << 30, MemAvailBytes: 4 << 30},
			{IP: "203.0.113.1", Hostname: "host-B", MemTotalBytes: 8 << 30, MemAvailBytes: 4 << 30},
			{IP: "10.0.0.9", Hostname: "lonely", MemTotalBytes: 8 << 30, MemAvailBytes: 4 << 30},
		},
	})
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	md, _ := cardBody(t, card)
	if !strings.Contains(md, "**203.0.113.1 · host-A**") {
		t.Errorf("NAT host A identity missing hostname suffix; body=%q", md)
	}
	if !strings.Contains(md, "**203.0.113.1 · host-B**") {
		t.Errorf("NAT host B identity missing hostname suffix; body=%q", md)
	}
	// Unique-IP host renders the bare IP, no hostname suffix.
	if !strings.Contains(md, "**10.0.0.9**") {
		t.Errorf("unique-IP identity should be bare; body=%q", md)
	}
	if strings.Contains(md, "10.0.0.9 · lonely") {
		t.Errorf("unique-IP host should not carry hostname; body=%q", md)
	}
}
