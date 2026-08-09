// Package cardkit is the single source of truth for Feishu card JSON
// produced by the frontend. Every renderer MUST route through these
// constructors; no renderer may json.Marshal a top-level card object
// directly (R7).
//
// Card schema is 1.0 by default (not 2.0): root holds schema + config + header +
// elements. elements[] carries content, an action container wrapping any
// buttons ({"tag":"action","actions":[]}), and the footer line as the last
// element. v1 was kept because the legacy im PATCH path had unreliable v2-card
// persistence (see Card doc). The CardKit 实体 migration adds schema 2.0 behind
// a process-wide switch (SetSchemaV2): v2 moves elements under body.elements,
// rejects the v1 "action" container (buttons ride a column_set instead), and
// is served by the CardKit card-entity APIs, where the v1 click-window revert
// problem does not exist (PoC: docs/feishu-cardkit-migration-assessment.md).
// All cards share the same header/footer structure (R1–R3) so there is no
// visual drift across event types or backends.
package cardkit

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// MaxBodyRunes caps the markdown body of interactive/notice cards. These card
// types carry buttons/form/options JSON on top of the body, so the body budget
// is smaller than progress (6000) / result (8000) to stay well under Feishu's
// ≈28 KiB card content limit.
const MaxBodyRunes = 4000

// MaxCardElements is Feishu's per-card element hard cap (~50). Card fails
// rendering server-side (230025 or similar) above it, so Card() refuses to
// build such a card up front rather than letting SendCard/UpdateCard surface
// the rejection as an opaque error. This is the LAST line of defence;
// renderers are expected to bound their own output well under it.
const MaxCardElements = 50

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

// schema 1.0：button 必须包在
// {"tag":"action","actions":[...]} 容器里，root 顶层用 elements（不是
// body.elements）。退回 v1 的原因：飞书 PATCH 接口对 schemaV2 卡的持久化
// 在某些客户端组合下不稳定——picker 点击后翻绿一瞬间又恢复旧卡（实测）。
// schemaV2 卡不能改成 schemaV1（飞书 230099/200830），所以 SendCard 也必须
// 用 v1 保持一致。代价：失去 v2 新特性（streaming_mode 等我们没用上）；
// 收益：所有客户端版本一致渲染 + PATCH 链路可靠。footer 作为最后一个 element。
//
// schema 2.0（SetSchemaV2 之后）：root 顶层用 body.elements（不再用 elements），
// button 不能再用 action 容器（v2 拒绝 tag:"action"），改挂 column_set 里
// 每列一个按钮。CardKit 卡片实体 API 只接 v2，且不存在 v1 的点击窗口回退
// 问题（PoC: docs/feishu-cardkit-migration-assessment.md），所以 cardkit
// 引擎走 v2。布局按 schema 分派，同一份 elements/actions 输入。
func Card(header HeaderInfo, footer FooterInfo, elements []Element, actions []Action) ([]byte, error) {
	if elements == nil {
		elements = []Element{}
	}
	if schemaV2.Load() {
		return cardV2(header, footer, elements, actions)
	}
	return cardV1(header, footer, elements, actions)
}

// schemaV2 is the process-wide card-JSON schema switch. false (default) keeps
// every Card/Notice/StatusReport emitting schema 1.0 + top-level elements for
// the legacy im PATCH path; SetSchemaV2(true) flips them to schema 2.0 +
// body.elements + column_set buttons for the CardKit card-entity APIs. Set
// once at startup from config before the first card render — flipping it at
// runtime would let a progress card send as v2 then update as v1 (rejected:
// schemaV2 卡不能改回 v1, 飞书 230099/200830).
var schemaV2 atomic.Bool

// SetSchemaV2 flips the process-wide card schema. See schemaV2.
func SetSchemaV2(on bool) { schemaV2.Store(on) }

// cardV1 is the legacy layout: schema 1.0, root elements, action container.
func cardV1(header HeaderInfo, footer FooterInfo, elements []Element, actions []Action) ([]byte, error) {
	all := make([]Element, 0, len(elements)+2)
	all = append(all, elements...)
	if len(actions) > 0 {
		all = append(all, Element{"tag": "action", "actions": actions})
	}
	all = append(all, Footer(footer))
	if len(all) > MaxCardElements {
		return nil, fmt.Errorf("cardkit: card has %d elements, exceeds Feishu hard limit %d", len(all), MaxCardElements)
	}
	card := map[string]any{
		"schema":   "1.0",
		"config":   map[string]any{"update_multi": true},
		"header":   Header(header),
		"elements": all,
	}
	return json.Marshal(card)
}

// cardV2 is the CardKit layout: schema 2.0, body.elements, buttons ride a
// column_set (one button per column) because v2 rejects the v1 action
// container. buttonColumnCount caps columns at Feishu's per-column_set limit
// so a crowded permission card never overflows the component.
func cardV2(header HeaderInfo, footer FooterInfo, elements []Element, actions []Action) ([]byte, error) {
	all := make([]Element, 0, len(elements)+2)
	all = append(all, elements...)
	if len(actions) > 0 {
		all = append(all, actionColumnSet(actions))
	}
	all = append(all, Footer(footer))
	if len(all) > MaxCardElements {
		return nil, fmt.Errorf("cardkit: card has %d elements, exceeds Feishu hard limit %d", len(all), MaxCardElements)
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": Header(header),
		"body":   map[string]any{"elements": all},
	}
	return json.Marshal(card)
}

// actionColumnSet lays buttons out horizontally in a column_set for schema
// 2.0. Each button sits in its own column (weight 1, vertical_align top) so
// they size evenly; more than maxActionColumns actions wrap into additional
// rows (one column_set per row) appended via the caller's elements slice.
func actionColumnSet(actions []Action) Element {
	const maxActionColumns = 5
	row := actions
	if len(row) > maxActionColumns {
		row = row[:maxActionColumns]
	}
	columns := make([]any, 0, len(row))
	for _, a := range row {
		columns = append(columns, map[string]any{
			"tag":           "column",
			"width":         "weighted",
			"weight":        1,
			"vertical_align": "top",
			"elements":      []any{a},
		})
	}
	return Element{
		"tag":     "column_set",
		"columns": columns,
	}
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
