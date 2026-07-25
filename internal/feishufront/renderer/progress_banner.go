package renderer

// This file holds the progress-card banner: a one-line status slot at the top
// of the card that surfaces EITHER a transient loading notice (grey, sourced
// from ProgressPayload.Description) OR a blocking interactive-gate marker
// (⏸ waiting / ✓ answered / ✗ denied). The renderer owns presentation (icon +
// verb + tone); the bridge sends only the bare Summary via protocol.GateInfo.
//
// A gate takes precedence over a loading notice when both are set — a blocking
// gate is the one thing the user must see.

// bannerLine renders the top banner: a gate (blocking) takes precedence over a
// transient loading notice. Returns "" when neither is set so Render inserts no
// empty zone. The icon + verb convey state at a glance (⏸ waiting / ✓ answered
// / ✗ denied / • loading).
func bannerLine(loading string, g GateInfo) string {
	if g.State != "" {
		return gateLine(g)
	}
	if loading != "" {
		return "• " + loading
	}
	return ""
}

// gateLine formats one gate banner. verb defaults to "等待授权" (permission)
// and switches to "等待回答" for a question gate; an answered gate reports the
// choice, a denied gate reports the reject/timeout.
func gateLine(g GateInfo) string {
	verb := "等待授权"
	if g.Kind == "question" {
		verb = "等待回答"
	}
	icon := "•"
	switch g.State {
	case "waiting":
		icon = "⏸"
	case "answered":
		icon = "✓"
		verb = "已应答"
	case "denied":
		icon = "✗"
		verb = "已拒绝/超时"
	}
	line := icon + " " + verb
	if g.Summary != "" {
		line += "：" + g.Summary
	}
	return line
}
