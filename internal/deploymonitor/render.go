package deploymonitor

import (
	"fmt"
	"strings"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// This file holds the formatting helpers shared by the job lifecycle and the
// /running query: command-line labels, /running turn-list rendering, and the
// rune+line-aware output tail. Kept out of handler.go so that file holds only
// the dispatch + job-lifecycle state machine.

// jobLabel renders the command line ("name args...") for logs and notices.
func jobLabel(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// renderTurns formats the in-flight snapshot as a scannable notice body. The
// trailing abort hint reinforces the policy: turns are never ended automatically.
func renderTurns(snap *protocol.StatusSnapshot) string {
	if len(snap.Turns) == 0 {
		return "当前没有运行中的会话。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "运行中会话（%d）：\n", len(snap.Turns))
	for _, t := range snap.Turns {
		fmt.Fprintf(&sb, "- %s · %s · %s\n", t.BackendID, shortID(t.ChatID), formatElapsed(t.ElapsedS))
	}
	sb.WriteString("\n会话不会自动结束，如需结束请发送 /session-abort。")
	return sb.String()
}

// shortID shortens a Feishu ID (oc_ + 32 hex) to its last 8 chars so the turn
// list stays scannable while remaining identifiable.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return "…" + id[len(id)-8:]
}

// formatElapsed turns seconds into a compact duration label.
func formatElapsed(s int64) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
	}
}

// tailOutput returns the last ~maxRunes runes of out, advanced to the next
// line boundary. The deploy script emits substantial progress text; only the
// tail is useful in a chat notice. The budget is in RUNES, not bytes, so a
// multi-byte log (Chinese progress lines, 3 bytes/char) is not split mid-rune;
// advancing to the next newline avoids opening on a half-line fragment.
func tailOutput(out []byte, maxRunes int) string {
	s := strings.TrimSpace(string(out))
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	cut := string(r[len(r)-maxRunes:])
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "…" + cut
}
