package feishufront

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/log"
)

// noticeSendTimeout bounds a backend online/offline notification's Feishu send.
// A stalled API call cannot wedge the notify goroutine indefinitely.
const noticeSendTimeout = 10 * time.Second

func (d *Dispatcher) OnBackendOffline(backendID, backendType string) {
	// A disconnecting backend never sends the terminal control that would
	// Finish its turns. Per project policy a turn ends ONLY when the user
	// sends /session-abort, so we do NOT release them here — the stranded
	// turns stay in the in-flight set, visible by name via GET /v1/status's
	// turns list, until the user explicitly aborts or the frontend restarts.
	// (Interactive/card bindings are still reclaimed by their own TTL sweep.)
	if d.router == nil {
		return
	}
	chats := d.router.ChatsOf(backendID)
	for _, chatID := range chats {
		footer := cardkit.FooterInfo{BackendID: backendID, BackendType: backendType, Status: "离线", Time: time.Now()}
		card, err := cardkit.Notice(footer, "warning", "后端离线",
			"backend "+backendID+" 已断开。该后端的进行中任务不会被自动结束，如需结束请发送 /session-abort；要继续对话请用 /backend 切换到其他在线后端。", "", "", "")
		if err != nil {
			continue
		}
		d.notifyBackendChat(chatID, "offline", card)
	}
}

func (d *Dispatcher) OnBackendOnline(backendID, backendType string) {
	if d.router == nil {
		return
	}
	chats := d.router.ChatsOf(backendID)
	if len(chats) == 0 {
		return // no chats bound to this backend; nothing to notify
	}
	for _, chatID := range chats {
		footer := cardkit.FooterInfo{BackendID: backendID, BackendType: backendType, Status: "已恢复", Time: time.Now()}
		card, err := cardkit.Notice(footer, "success", "后端已恢复",
			"backend "+backendID+" 已重新连接，可以继续对话。", "", "", "")
		if err != nil {
			continue
		}
		d.notifyBackendChat(chatID, "online", card)
	}
}

// notifyBackendChat sends a backend online/offline notice to one chat. A
// bounded context prevents a stalled Feishu API from wedging the notify loop;
// failures are logged rather than ignored so a transient outage is observable.
func (d *Dispatcher) notifyBackendChat(chatID, kind string, card []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), noticeSendTimeout)
	defer cancel()
	if _, err := d.bot.SendCard(ctx, chatID, card, ""); err != nil {
		d.logger.Load().Warn("notify backend online/offline",
			log.FieldChatID, chatID,
			"notice", kind,
			log.FieldError, err)
	}
}

func parseBackendCommand(prompt string) (cmd, rest string) {
	// Match "/backend" only as a complete token (followed by space or end),
	// so "/backendfoo list" is not mistaken for a backend command.
	if prompt != "/backend" && !strings.HasPrefix(prompt, "/backend ") {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimSpace(prompt), " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

// handleBackendCommand serves every form of /backend: it pops an interactive
// picker card whose buttons are the currently-online backends. args is ignored
// — there is no free-form /backend use {id}; a backend can only be picked from
// the online list.
func (d *Dispatcher) handleBackendCommand(ctx context.Context, msg *feishu.IncomingMessage, args string) error {
	if d.router == nil {
		return d.notice(ctx, msg.ChatID, "error", "路由未就绪", "前端路由尚未初始化")
	}
	if len(d.registry.Registered()) == 0 {
		return d.notice(ctx, msg.ChatID, "warning", "无在线后端", "当前没有后端连接，请稍后再试。")
	}
	card, err := d.renderBackendPicker(msg.ChatID)
	if err != nil {
		return err
	}
	_, err = d.bot.SendCard(ctx, msg.ChatID, card, msg.MessageID)
	return err
}

// renderBackendPicker builds an interactive card listing every online backend
// as a button. The chat's currently-bound backend (if any) is prefixed ✓ and
// disabled so it cannot be re-selected. Each button carries kind="backend" +
// backendID, which DispatchCardAction reads to route the click to the
// frontend's handleBackendChoice instead of forwarding it to a backend.
func (d *Dispatcher) renderBackendPicker(chatID string) ([]byte, error) {
	ids := d.registry.Registered()
	sort.Strings(ids)
	current, _ := d.router.Resolve(chatID)
	header := cardkit.HeaderInfo{Title: "选择后端", Template: "blue"}
	footer := cardkit.FooterInfo{Status: "选择后端", Time: time.Now()}
	actions := make([]cardkit.Action, 0, len(ids))
	for _, id := range ids {
		label := id + "（" + d.registry.BackendType(id) + "）"
		if id == current {
			label = "✓ " + label
		}
		actions = append(actions, cardkit.ButtonAction(label, "backend",
			map[string]any{"backendID": id}, false, id == current))
	}
	body := "点击按钮切换当前群的后端（仅在线可选）。"
	return cardkit.Card(header, footer, []cardkit.Element{cardkit.MarkdownElement(body)}, actions)
}

// handleBackendChoice is the frontend-side consumer of a backend-picker click:
// it binds the chat to the chosen backend and refreshes the card so the new
// selection shows ✓/disabled. Unlike backend-driven Question cards this never
// round-trips to a backend — the frontend owns the Layer-1 route.
func (d *Dispatcher) handleBackendChoice(ctx context.Context, action *feishu.CardAction) error {
	id, _ := action.Value["backendID"].(string)
	btype := d.registry.BackendType(id)
	if btype == "" {
		return d.notice(ctx, action.ChatID, "warning", "后端离线",
			"backend "+id+" 已不在线。发送 /backend 重新选择。")
	}
	if err := d.router.Set(action.ChatID, id); err != nil {
		return d.notice(ctx, action.ChatID, "error", "切换失败", err.Error())
	}
	// Refresh the picker card so the new selection shows ✓/disabled, then
	// always send a confirmation notice. The card refresh alone (button
	// going grey with a ✓) is too subtle — without an explicit notice users
	// report "no feedback", unlike the other pickers which all confirm via a
	// separate success card.
	if card, err := d.renderBackendPicker(action.ChatID); err == nil {
		_ = d.bot.UpdateCard(ctx, action.MessageID, card)
	}
	return d.notice(ctx, action.ChatID, "success", "已切换后端", "当前后端: "+id+"（"+btype+"）")
}
