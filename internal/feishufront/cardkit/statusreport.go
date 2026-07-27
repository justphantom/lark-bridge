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

// TurnRow is the cardkit view of one in-flight turn for StatusReport. It
// mirrors protocol.TurnInfo's display fields without importing protocol, so
// cardkit stays a pure rendering layer (same convention as Notice taking only
// primitive args).
type TurnRow struct {
	BackendID string
	ChatID    string // full Feishu chat id; rendered via ShortID
	ElapsedS  int64
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
// backend: a summary line (updated time · period · online backends · in-flight
// count) followed by per-backend turn groups (short chat id + elapsed). When
// there are no in-flight turns the body collapses to a one-line idle notice
// plus the online-backend list. schema 1.0 via Card, same as every other card.
func StatusReport(footer FooterInfo, title string, generatedAt int64, intervalS, inflight int, backends []string, turns []TurnRow) ([]byte, error) {
	info := HeaderInfo{BackendType: footer.BackendType, Title: title, Template: "blue"}
	if info.Title == "" {
		info.Title = "状态总览"
	}

	var b strings.Builder
	// Summary line.
	fmt.Fprintf(&b, "更新 %s", time.Unix(generatedAt, 0).Format("15:04:05"))
	if intervalS > 0 {
		fmt.Fprintf(&b, " · 周期 %s", formatPeriod(intervalS))
	}
	fmt.Fprintf(&b, " · 在线后端 %d · 会话 %d", len(backends), inflight)

	if inflight == 0 || len(turns) == 0 {
		b.WriteString("\n\n当前没有运行中的会话。")
		if len(backends) > 0 {
			b.WriteString("\n\n在线后端：" + strings.Join(backends, " · "))
		}
	} else {
		groups := groupTurns(backends, turns)
		for _, g := range groups {
			fmt.Fprintf(&b, "\n\n▸ %s  %d 个会话", g.backendID, len(g.rows))
			shown := g.rows
			tail := 0
			if len(shown) > maxTurnsPerBackend {
				shown = shown[:maxTurnsPerBackend]
				tail = len(g.rows) - maxTurnsPerBackend
			}
			for _, r := range shown {
				fmt.Fprintf(&b, "\n　　· %s  已运行 %s", ShortID(r.ChatID), FormatElapsed(time.Duration(r.ElapsedS)*time.Second))
			}
			if tail > 0 {
				fmt.Fprintf(&b, "\n　　· …另 %d 条", tail)
			}
		}
	}

	md := truncateRunes(b.String(), MaxBodyRunes)
	elements := []Element{MarkdownElement(md)}
	return Card(info, footer, elements, nil)
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
