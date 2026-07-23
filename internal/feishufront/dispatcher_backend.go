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

// offlineNoticeDebounce delays an offline notice so a flapping backend (rapid
// disconnect/reconnect) cannot spam every bound chat with offline→online card
// pairs. An offline event arms a timer; a reconnect before it fires cancels
// the pending notice silently. Only a backend that stays down for the whole
// window triggers a notice — and only a backend whose offline notice was
// actually shown triggers a matching "recovered" notice, so flapping produces
// zero cards.
const offlineNoticeDebounce = 30 * time.Second

// flapState is the per-backend debounce state for online/offline notices.
// timer != nil means an offline notice is pending confirmation; notifiedOffline
// is true once an offline card has actually been shown to users. Guarded by
// Dispatcher.flapMu.
type flapState struct {
	timer           *time.Timer
	pendingType     string
	notifiedOffline bool
}

// OnBackendOffline arms a debounce timer rather than posting immediately: a
// reconnect within offlineNoticeDebounce cancels it (see OnBackendOnline), so a
// flapping backend produces no notice. Only when the timer fires does the
// offline card reach every bound chat.
//
// Per project policy a turn ends ONLY when the user sends /session-abort, so
// turns are NOT released here — stranded turns stay in-flight, visible via GET
// /v1/status, until the user aborts or the frontend restarts.
func (d *Dispatcher) OnBackendOffline(backendID, backendType string) {
	if d.router == nil {
		return
	}
	d.flapMu.Lock()
	st := d.flap[backendID]
	if st == nil {
		st = &flapState{}
		d.flap[backendID] = st
	}
	// Already shown offline: no duplicate notice, nothing to arm.
	if st.notifiedOffline {
		d.flapMu.Unlock()
		return
	}
	st.pendingType = backendType
	if st.timer != nil {
		st.timer.Reset(d.offlineNoticeDebounce)
	} else {
		st.timer = time.AfterFunc(d.offlineNoticeDebounce, func() {
			d.fireOfflineNotice(backendID)
		})
	}
	d.flapMu.Unlock()
}

// fireOfflineNotice runs in the debounce timer's goroutine once an offline
// event has persisted for the whole window. It flips the backend to
// offline-presented and posts the offline card to every bound chat.
func (d *Dispatcher) fireOfflineNotice(backendID string) {
	d.flapMu.Lock()
	st := d.flap[backendID]
	if st == nil {
		d.flapMu.Unlock()
		return
	}
	typ := st.pendingType
	st.notifiedOffline = true
	st.timer = nil
	d.flapMu.Unlock()
	d.sendOfflineNotices(backendID, typ)
}

// OnBackendOnline either cancels a pending offline notice (the backend blipped
// and came back → silent) or, if an offline card was actually shown, posts the
// matching recovery card. A reconnect with no prior notice produces nothing.
func (d *Dispatcher) OnBackendOnline(backendID, backendType string) {
	if d.router == nil {
		return
	}
	d.flapMu.Lock()
	st := d.flap[backendID]
	if st == nil {
		d.flapMu.Unlock()
		return // never went offline-presented; nothing to recover
	}
	// A pending offline notice means the backend blipped and came back: cancel
	// it silently — no offline card, no recovery card.
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
		d.flapMu.Unlock()
		return
	}
	// Only send a recovery if we previously showed an offline card.
	if !st.notifiedOffline {
		d.flapMu.Unlock()
		return
	}
	st.notifiedOffline = false
	d.flapMu.Unlock()
	d.sendOnlineNotices(backendID, backendType)
}

// sendOfflineNotices posts the offline card to every chat bound to backendID.
func (d *Dispatcher) sendOfflineNotices(backendID, backendType string) {
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

// sendOnlineNotices posts the recovered card to every chat bound to backendID.
func (d *Dispatcher) sendOnlineNotices(backendID, backendType string) {
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
// it binds the chat to the chosen backend and updates the original picker card
// to a green result state (disabled buttons + confirmation) so the switch
// produces only one message.
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
	card, err := d.renderBackendResult(action.ChatID, id, btype)
	if err != nil {
		return d.notice(ctx, action.ChatID, "error", "切换失败", err.Error())
	}
	return d.bot.UpdateCard(ctx, action.MessageID, card)
}

// renderBackendResult builds the result-state backend picker card: green
// header, confirmation body, and every backend button disabled (the selected
// one prefixed ✓). This replaces the original picker card in place so /backend
// emits only one message.
func (d *Dispatcher) renderBackendResult(chatID, selectedID, selectedType string) ([]byte, error) {
	ids := d.registry.Registered()
	sort.Strings(ids)
	current, _ := d.router.Resolve(chatID)
	header := cardkit.HeaderInfo{Title: "已切换后端", Template: "green"}
	footer := cardkit.FooterInfo{BackendID: selectedID, BackendType: selectedType, Status: "已完成", Time: time.Now()}
	actions := make([]cardkit.Action, 0, len(ids))
	for _, id := range ids {
		label := id + "（" + d.registry.BackendType(id) + "）"
		if id == current {
			label = "✓ " + label
		}
		actions = append(actions, cardkit.ButtonAction(label, "backend",
			map[string]any{"backendID": id}, id == current, true))
	}
	body := "当前后端: " + selectedID + "（" + selectedType + "）"
	return cardkit.Card(header, footer, []cardkit.Element{cardkit.MarkdownElement(body)}, actions)
}
