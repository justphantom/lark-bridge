package renderer

import (
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{1023, "1023"},
		{1024, "1.000k"},
		{1536, "1.500k"},
		{1048575, "1023.999k"}, // just under 1m
		{1048576, "1.000m"},    // 1024k exactly
		{1572864, "1.500m"},    // 1.5m
	}
	for _, c := range cases {
		if got := formatTokens(c.n); got != c.want {
			t.Errorf("formatTokens(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestRenderResult_CumulativeTokens(t *testing.T) {
	hdr := cardkit.HeaderInfo{}
	ftr := cardkit.FooterInfo{}
	cases := []struct {
		name        string
		tokens      int
		totalTokens int
		wantSubstr  string
		notWant     string
	}{
		{
			name:        "no cumulative shows bare count",
			tokens:      1109,
			totalTokens: 0,
			wantSubstr:  "📊 1.083k tokens",
			notWant:     "/",
		},
		{
			name:        "cumulative equals current shows bare count",
			tokens:      1109,
			totalTokens: 1109,
			wantSubstr:  "📊 1.083k tokens",
			notWant:     "/",
		},
		{
			name:        "cumulative exceeds current shows both",
			tokens:      1109,
			totalTokens: 519875,
			wantSubstr:  "📊 1.083k / 507.690k tokens",
		},
		{
			name:        "large cumulative in m unit",
			tokens:      50000,
			totalTokens: 3000000,
			wantSubstr:  "📊 48.828k / 2.861m tokens",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctrl := &protocol.Control{Result: &protocol.ResultPayload{
				Text:        "ok",
				Tokens:      c.tokens,
				TotalTokens: c.totalTokens,
			}}
			b, err := RenderResult(ctrl, hdr, ftr, "")
			if err != nil {
				t.Fatalf("RenderResult: %v", err)
			}
			s := string(b)
			if !strings.Contains(s, c.wantSubstr) {
				t.Errorf("want substring %q in card, got: %s", c.wantSubstr, s)
			}
			if c.notWant != "" && strings.Contains(s, c.notWant) {
				t.Errorf("did not want %q in card, got: %s", c.notWant, s)
			}
		})
	}
}

// TestRenderResult_CumulativeCost pins the cost line mirrors tokens' "本次 /
// 累计" form when TotalCost exceeds Cost, and collapses to the bare turn cost
// otherwise.
func TestRenderResult_CumulativeCost(t *testing.T) {
	hdr := cardkit.HeaderInfo{}
	ftr := cardkit.FooterInfo{}
	cases := []struct {
		name      string
		cost      float64
		totalCost float64
		want      string
	}{
		{"no cumulative shows bare cost", 0.0123, 0, "💰 $0.0123"},
		{"cumulative equals current shows bare", 0.0123, 0.0123, "💰 $0.0123"},
		{"cumulative exceeds current shows both", 0.0123, 0.5000, "💰 $0.0123 / $0.5000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctrl := &protocol.Control{Result: &protocol.ResultPayload{Text: "ok", Cost: c.cost, TotalCost: c.totalCost}}
			b, err := RenderResult(ctrl, hdr, ftr, "")
			if err != nil {
				t.Fatalf("RenderResult: %v", err)
			}
			if !strings.Contains(string(b), c.want) {
				t.Errorf("want %q, got: %s", c.want, b)
			}
		})
	}
}

// TestRenderResult_ReasoningTokens pins the 🧠 reasoning line appears only for
// a reasoning turn (ReasoningTokens > 0) and is omitted for a plain turn.
func TestRenderResult_ReasoningTokens(t *testing.T) {
	hdr := cardkit.HeaderInfo{}
	ftr := cardkit.FooterInfo{}

	with := &protocol.Control{Result: &protocol.ResultPayload{Text: "ok", ReasoningTokens: 2048}}
	b, err := RenderResult(with, hdr, ftr, "")
	if err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	if !strings.Contains(string(b), "🧠 2.000k reasoning") {
		t.Errorf("reasoning turn should show 🧠 line: %s", b)
	}

	without := &protocol.Control{Result: &protocol.ResultPayload{Text: "ok", ReasoningTokens: 0}}
	b2, err := RenderResult(without, hdr, ftr, "")
	if err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	if strings.Contains(string(b2), "🧠") {
		t.Errorf("non-reasoning turn must not show 🧠 line: %s", b2)
	}
}
