// Package cardkit is the single source of truth for Feishu card JSON
// produced by the frontend. Every renderer MUST route through these
// constructors; no renderer may json.Marshal a top-level card object
// directly (R7).
//
// Card schema follows the Feishu card v2 layout: the root holds only
// schema + header + body. body.elements[] carries content, button rows
// ({"tag":"action","actions":[]}), and the footer line as the last
// element. All cards share the same header/footer structure (R1–R3) so
// there is no visual drift across event types or backends.
package cardkit

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxBodyRunes caps the markdown body of interactive/notice cards. These card
// types carry buttons/form/options JSON on top of the body, so the body budget
// is smaller than progress (6000) / result (8000) to stay well under Feishu's
// ≈28 KiB card content limit.
const MaxBodyRunes = 4000

// InteractiveTimeout bounds how long an unresponded interactive card waits for
// user action. Kept here so the renderer and the turn manager share one value.
const InteractiveTimeout = 10 * time.Minute

// truncateRunes caps s to max runes, appending "…" if truncated. Duplicated
// from renderer.truncateRunes rather than exported, because renderer is an
// internal sibling and only two call sites exist.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// HeaderInfo describes a card header: backendType prefixes the title,
// template sets the colour band.
type HeaderInfo struct {
	BackendType string // "claude" | "opencode"
	Title       string
	Template    string // blue | green | orange | red | grey
}

// FooterInfo describes a card footer. Status leads the line so a card can be
// scanned by state at a glance (处理中 / 已完成 / 已取消 / 待确认 …), colour-blind
// redundant with the header template. Elapsed is the live running time of the
// turn (omitted on cards without one); Time is the absolute timestamp used as a
// fallback when Elapsed is empty (e.g. standalone notice cards with no turn).
type FooterInfo struct {
	BackendID   string
	BackendType string
	Model       string
	SessionID   string
	Status      string
	Elapsed     string
	Time        time.Time
}

// Element is one card body element (markdown / div / hr / action etc.).
type Element map[string]any

// Action is one button/select action inside an actions element.
type Action map[string]any

// Card builds the unified card: root holds only schema + header + body.
// Buttons are placed directly in body.elements (card JSON 2.0 deprecated
// the {"tag":"action"} interaction module); the footer line is appended
// as the last body element.
func Card(header HeaderInfo, footer FooterInfo, elements []Element, actions []Action) ([]byte, error) {
	if elements == nil {
		elements = []Element{}
	}
	all := make([]Element, 0, len(elements)+len(actions)+1)
	all = append(all, elements...)
	for _, a := range actions {
		all = append(all, Element(a))
	}
	all = append(all, Footer(footer))
	card := map[string]any{
		"schema": "2.0",
		"header": Header(header),
		"body":   map[string]any{"elements": all},
	}
	return json.Marshal(card)
}

// Header builds the top-level header object (R2): title is
// "[{backendType}] {title}", template sets the colour. template defaults
// to blue when unset.
func Header(info HeaderInfo) map[string]any {
	template := info.Template
	if template == "" {
		template = "blue"
	}
	title := info.Title
	if info.BackendType != "" {
		title = fmt.Sprintf("[%s] %s", info.BackendType, info.Title)
	}
	return map[string]any{
		"title":    map[string]any{"tag": "plain_text", "content": title},
		"template": template,
	}
}

// Footer builds a body element carrying the footer line. The text is
// "{status} · {backendType} · {model} · {elapsed|time} · {session前缀}", omitting
// any empty segment. Elapsed (live, e.g. "45s") is preferred over the absolute
// timestamp so a progress card reads as a running timer; cards without a turn
// (standalone notices) pass Elapsed empty and may supply Time instead.
func Footer(info FooterInfo) Element {
	var parts []string
	if info.Status != "" {
		parts = append(parts, info.Status)
	}
	if info.BackendType != "" {
		parts = append(parts, info.BackendType)
	}
	if info.Model != "" {
		parts = append(parts, info.Model)
	}
	if info.Elapsed != "" {
		parts = append(parts, info.Elapsed)
	} else if !info.Time.IsZero() {
		parts = append(parts, info.Time.Format("2006-01-02 15:04:05"))
	}
	if info.SessionID != "" {
		prefix := info.SessionID
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		parts = append(parts, prefix)
	}
	text := strings.Join(parts, " · ")
	return Element{
		"tag":  "div",
		"text": map[string]any{"tag": "plain_text", "content": text},
	}
}

// FormatElapsed renders a duration as a compact running-time label for the
// progress/result footer: "45s", "1m23s", "1h02m". Kept integer-grained because
// the progress card refreshes every 500 ms — sub-second precision would flicker.
func FormatElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// Notice builds a notice card (R6): error/warning/info share one template,
// only the level (→ template + title colour) differs. body is the markdown
// message. field/before/after, when field is non-empty, render a before→after
// change block above the body so a setting-change command confirms what moved
// (e.g. "/perm plan" shows "~default~ → **plan**") instead of only the new
// value. before empty (first-time set) collapses to just the new value.
func Notice(footer FooterInfo, level, title, body, field, before, after string) ([]byte, error) {
	info := HeaderInfo{
		BackendType: footer.BackendType,
		Title:       title,
		Template:    noticeTemplate(level),
	}
	md := body
	if field != "" {
		change := "**" + field + "**\n"
		if before != "" {
			change += "~" + before + "~ → **" + after + "**"
		} else {
			change += "→ **" + after + "**"
		}
		if md != "" {
			md = change + "\n\n" + md
		} else {
			md = change
		}
	}
	md = truncateRunes(md, MaxBodyRunes)
	elements := []Element{MarkdownElement(md)}
	return Card(info, footer, elements, nil)
}

// noticeTemplate maps a notice level to a header template colour.
func noticeTemplate(level string) string {
	switch level {
	case "error":
		return "red"
	case "warning":
		return "orange"
	case "success":
		return "green"
	default:
		return "grey"
	}
}
