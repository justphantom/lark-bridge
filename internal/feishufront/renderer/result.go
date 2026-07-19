package renderer

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// maxResultRunes caps the result body so a very long reply (e.g. a whole file
// dumped by the agent) does not produce a card JSON that Feishu rejects
// (≈28 KiB card content limit). The stats line is appended after capping.
const maxResultRunes = 8000

// formatDuration renders a duration in a human-friendly way.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

// formatTokens renders a token count in 1024-base units: the raw number below
// 1024, "a.bcd k" from 1024 up, "a.bcd m" from 1024k (1,048,576) up. Three
// decimal places keep sub-unit precision visible without overflowing the card
// stats line.
func formatTokens(n int) string {
	if n < 1024 {
		return strconv.Itoa(n)
	}
	f := float64(n)
	if f < 1024*1024 {
		return fmt.Sprintf("%.3fk", f/1024)
	}
	return fmt.Sprintf("%.3fm", f/(1024*1024))
}

// RenderResult builds the terminal reply card: green template, the result
// text as markdown, an optional one-line execution summary (reads/execs/
// subagents), and a tokens/duration stat line when present. No actions.
// summary is "" when no tools ran (e.g. a pure chat reply).
func RenderResult(ctrl *protocol.Control, header cardkit.HeaderInfo, footer cardkit.FooterInfo, summary string) ([]byte, error) {
	header.Template = "green"
	if header.Title == "" {
		header.Title = "已完成"
	}
	body := truncateRunes(ctrl.Result.Text, maxResultRunes)
	// Sections below the body are joined with markdown thematic breaks so each
	// reads as its own line; collect them in order then append to the body.
	var sections []string
	if summary != "" {
		sections = append(sections, summary)
	}
	// Build stats line: tokens · duration · steps · cost
	var stats []string
	if ctrl.Result.Tokens > 0 {
		// Cumulative total covers this session's every turn (including this
		// one). Show "本次 / 累计" only when history exists and exceeds the
		// current turn; otherwise the bare current count reads cleaner.
		if ctrl.Result.TotalTokens > ctrl.Result.Tokens {
			stats = append(stats, fmt.Sprintf("📊 %s / %s tokens", formatTokens(ctrl.Result.Tokens), formatTokens(ctrl.Result.TotalTokens)))
		} else {
			stats = append(stats, fmt.Sprintf("📊 %s tokens", formatTokens(ctrl.Result.Tokens)))
		}
	}
	if ctrl.Result.Duration > 0 {
		stats = append(stats, fmt.Sprintf("⏱ %s", formatDuration(ctrl.Result.Duration)))
	}
	if ctrl.Result.Steps > 0 {
		stats = append(stats, fmt.Sprintf("🔄 %d 轮", ctrl.Result.Steps))
	}
	if ctrl.Result.Cost > 0 {
		stats = append(stats, fmt.Sprintf("💰 $%.4f", ctrl.Result.Cost))
	}
	if len(stats) > 0 {
		sections = append(sections, strings.Join(stats, " · "))
	}
	for _, sec := range sections {
		if body == "" {
			body = sec
		} else {
			body = body + "\n\n---\n" + sec
		}
	}
	if footer.Model == "" && ctrl.Result.Model != "" {
		footer.Model = ctrl.Result.Model
	}
	if footer.SessionID == "" && ctrl.Result.SessionID != "" {
		footer.SessionID = ctrl.Result.SessionID
	}
	return cardkit.Card(header, footer, []cardkit.Element{cardkit.MarkdownElement(body)}, nil)
}
