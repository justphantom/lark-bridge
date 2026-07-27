package cardkit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStatusReport_RenderAndGroups(t *testing.T) {
	footer := FooterInfo{BackendID: "status-1", BackendType: "status-monitor", Status: "总览"}
	turns := []TurnRow{
		{BackendID: "claude-1", ChatID: "oc_1111111111111111aaaaaaaaaaaaaaaa", ElapsedS: 30},
		{BackendID: "claude-1", ChatID: "oc_2222222222222222bbbbbbbbbbbbbbbb", ElapsedS: 745}, // 12m25s, longest first
		{BackendID: "opencode-1", ChatID: "oc_3333333333333333cccccccccccccccc", ElapsedS: 5},
	}
	card, err := StatusReport(footer, "总览", 1700000000, 60, 2,
		[]string{"claude-1", "opencode-1", "status-1"}, turns)
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(card, &m); err != nil {
		t.Fatalf("invalid card json: %v", err)
	}
	if m["schema"] != "1.0" {
		t.Errorf("schema = %v, want 1.0", m["schema"])
	}
	elems, _ := m["elements"].([]any)
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
	card, err := StatusReport(FooterInfo{BackendType: "status-monitor"}, "", 1700000000, 60, 0, []string{"claude-1"}, nil)
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	var m map[string]any
	json.Unmarshal(card, &m)
	elems, _ := m["elements"].([]any)
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
	card, err := StatusReport(FooterInfo{BackendType: "status-monitor"}, "t", 1, 60, len(turns), []string{"claude-1"}, turns)
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	var m map[string]any
	json.Unmarshal(card, &m)
	elems, _ := m["elements"].([]any)
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
