package feishufront

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/feishufront/renderer"
	"github.com/justphantom/lark-bridge/internal/log"
)

// noticeSendTimeout bounds a backend online/offline notification's Feishu send.
// A stalled API call cannot wedge the notify goroutine indefinitely.
const noticeSendTimeout = 10 * time.Second

// cardPatchDelayDefault is how long handleBackendChoice waits after a click
// before PATCHing the picker card. Feishu reverts an immediate PATCH within
// its click-handling window (~3-5s); waiting past it lets the PATCH persist.
// Overridable via config (timeouts.card_patch_delay) and SetCardPatchDelay.
const cardPatchDelayDefault = 5 * time.Second

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
//
// generation is a cancel token for the debounce timer's callback: each
// OnBackendOnline that cancels a pending timer bumps it, so a callback that
// already fired (timer.Stop returned false) and is now waiting on flapMu
// notices its armedGen is stale and returns without sending the offline
// card. Without this, a backend that blips offline→online within the window
// could still see its offline notice posted AFTER the online recovery.
type flapState struct {
	timer           *time.Timer
	pendingType     string
	notifiedOffline bool
	generation      int
}

// OnBackendOffline arms a debounce timer rather than posting immediately: a
// reconnect within offlineNoticeDebounce cancels it (see OnBackendOnline), so a
// flapping backend produces no notice. Only when the timer fires
// (fireOfflineNotice) does the offline card reach every bound chat — and only
// then are the backend's stranded turns reclaimed: a backend that stays down
// for the whole window has lost its in-flight goroutines, so its turns can
// never receive a terminal control and would otherwise wedge
// /v1/deploy-preflight forever. A brief blip reclaims nothing.
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
	// Arm (or re-arm) the debounce timer with a fresh generation token each
	// time. A previously-fired timer's callback may be in flight, waiting on
	// flapMu; bumping generation makes it see a stale armedGen and
	// self-suppress, so only this latest arm's callback can post. Stopping
	// and recreating (not Reset) is required: Reset reuses the original
	// callback and its captured armedGen, which would still match
	// st.generation on the re-fire and post a DUPLICATE offline card plus a
	// duplicate reclaimStrandedTurns (the original M11 bug).
	st.generation++
	armedGen := st.generation
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(d.offlineNoticeDebounce, func() {
		goSafe(func() { d.fireOfflineNotice(backendID, armedGen) })
	})
	d.flapMu.Unlock()
}

// fireOfflineNotice runs in the debounce timer's goroutine once an offline
// event has persisted for the whole window. It flips the backend to
// offline-presented and posts the offline card to every bound chat. armedGen
// is the generation token captured when the timer was armed; if it no longer
// matches st.generation the timer was cancelled (backend recovered) while the
// callback was waiting on flapMu, and the notice is suppressed.
func (d *Dispatcher) fireOfflineNotice(backendID string, armedGen int) {
	d.flapMu.Lock()
	st := d.flap[backendID]
	if st == nil || st.generation != armedGen {
		d.flapMu.Unlock()
		return
	}
	typ := st.pendingType
	st.notifiedOffline = true
	st.timer = nil
	d.flapMu.Unlock()
	d.sendOfflineNotices(backendID, typ)
	// Now that the offline is confirmed persistent, reclaim the backend's
	// stranded turns: their goroutines died with the backend process, so they
	// can never emit a terminal control. Flipping each progress card to a
	// failure state and releasing the turn lets /v1/deploy-preflight converge
	// instead of deadlocking.
	d.reclaimStrandedTurns(backendID)
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
	// it silently — no offline card, no recovery card. Deleting the entry (not
	// just bumping the generation) makes a timer callback that already fired
	// (Stop returned false) and is waiting on flapMu see st==nil and suppress
	// the notice — and it keeps flap bounded to backends with LIVE flap state
	// instead of accumulating one entry per backend ever seen (low-2).
	if st.timer != nil {
		st.timer.Stop()
		delete(d.flap, backendID)
		d.flapMu.Unlock()
		return
	}
	// Only send a recovery if we previously showed an offline card.
	if !st.notifiedOffline {
		delete(d.flap, backendID) // clean state: nothing pending, nothing shown
		d.flapMu.Unlock()
		return
	}
	// Recovery path: clear the state under the lock (so a concurrent offline
	// re-arms a fresh entry) before posting the recovery card.
	delete(d.flap, backendID)
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
			"backend "+backendID+" 已断开。该后端的进行中任务已被自动结束（见下方「会话已失效」卡片）；要继续对话请用 /backend 切换到其他在线后端。", "", "", "")
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

// reclaimStrandedTurns finishes every in-flight turn owned by backendID and
// flips each turn's progress card to a "会话已失效" failure card in place. It
// runs once a backend has been offline for the whole notice-debounce window
// (fireOfflineNotice): by then the backend process and its in-flight goroutines
// are gone, so those turns can never receive a terminal control. Releasing them
// lets /v1/deploy-preflight converge instead of deadlocking every later deploy,
// and tells the user which prompt died rather than leaving a "处理中" card
// frozen forever. A turn with no progress card (messageID empty) falls back to
// a fresh standalone notice in its chat.
func (d *Dispatcher) reclaimStrandedTurns(backendID string) {
	reclaimed := d.turns.ReclaimBackend(backendID)
	// Mirror the reap into the registry's runningTurns so the two in-flight
	// views cannot disagree: TurnManager stops tracking the turns, but without
	// this the registry would keep reporting them (RunningTurns / metrics
	// consumers) until the backend reconnects and pushes a fresh snapshot.
	if d.registry != nil {
		d.registry.ReclaimTurns(backendID)
	}
	for _, turn := range reclaimed {
		d.invalidateTurnCard(turn)
	}
}

// invalidateTurnCard finalises one reclaimed turn: it patches the progress
// card to a failure notice (so the user sees the prompt ended), drops the
// progress state + finalized marker, and finalises any linked interactive
// cards. A withdrawn progress card or an empty messageID falls back to a fresh
// standalone notice so the failure is never silently dropped.
func (d *Dispatcher) invalidateTurnCard(turn Turn) {
	ctx, cancel := context.WithTimeout(context.Background(), noticeSendTimeout)
	defer cancel()
	backendType := ""
	if d.registry != nil {
		backendType = d.registry.BackendType(turn.BackendID)
	}
	footer := cardkit.FooterInfo{
		BackendID: turn.BackendID, BackendType: backendType,
		Model: turn.Model, SessionID: turn.SessionID,
		Status: "失效", Elapsed: cardkit.FormatElapsed(time.Since(turn.StartedAt)), Time: time.Now(),
	}
	card, err := cardkit.Notice(footer, "error", "会话已失效",
		"后端已离线，该任务无法继续。请重新发送你的问题，或用 /backend 切换到其他在线后端。", "", "", "")
	if err != nil {
		// Rendering failed: still release the progress slot so it does not leak.
		d.cleanupProgress(turn.PromptID, turn.MessageID)
		return
	}
	delivered := false
	if turn.MessageID != "" {
		if uerr := d.bot.UpdateCard(ctx, turn.MessageID, turn.CardID, card); uerr == nil {
			d.markFinalized(turn.MessageID)
			delivered = true
		}
	}
	if !delivered {
		// No progress card to patch, or it was withdrawn: send a fresh notice.
		if _, serr := d.bot.SendCard(ctx, turn.ChatID, card, ""); serr != nil {
			if l := d.logger.Load(); l != nil {
				l.Warn("reclaim: failed to deliver stranded-turn notice",
					log.FieldChatID, turn.ChatID, log.FieldMessageID, turn.MessageID, log.FieldError, serr.Error())
			}
		}
	}
	// Release progress state + finalized marker, and finalise any interactive
	// cards the turn owned (so they flip to a finalised form rather than grey).
	d.cleanupProgress(turn.PromptID, turn.MessageID)
	d.finalizeLinkedInteractive(ctx, turn.PromptID)
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
	ref, err := d.bot.SendCard(ctx, msg.ChatID, card, msg.MessageID)
	if err != nil {
		return err
	}
	messageID := ref.MessageID
	// Arm a TTL so a picker nobody clicks does not stay clickable forever.
	// Mirrors the interactive-card expiry: after cardkit.InteractiveTimeout
	// the card flips to a grey "已失效" state. Cancelled on the first click
	// (handleBackendChoice). Keyed by messageID — each /backend sends a
	// fresh card with its own id.
	d.armPickerExpiry(messageID, ref.CardID, card)
	return nil
}

// armPickerExpiry caches the picker card bytes and schedules the TTL flip.
// Guarded by cardMu alongside the interactive-card timer maps. cardID is the
// CardKit entity id ("" under legacy) the expiry/outcome update path needs.
func (d *Dispatcher) armPickerExpiry(messageID, cardID string, card []byte) {
	d.cardMu.Lock()
	defer d.cardMu.Unlock()
	d.pickerCards[messageID] = card
	d.pickerCardIDs[messageID] = cardID
	msgID := messageID
	d.pickerTimers[messageID] = time.AfterFunc(cardkit.InteractiveTimeout, func() {
		goSafe(func() { d.expirePicker(msgID) })
	})
}

// cancelPickerExpiry stops a pending TTL flip and drops the cached bytes.
// Called when the user clicks any backend button so a late expiry cannot
// overwrite the success/failure card the click produced. No-op when the
// picker already expired or was never armed.
func (d *Dispatcher) cancelPickerExpiry(messageID string) ([]byte, bool) {
	d.cardMu.Lock()
	defer d.cardMu.Unlock()
	t, ok := d.pickerTimers[messageID]
	if ok {
		t.Stop()
		delete(d.pickerTimers, messageID)
	}
	card, hadCard := d.pickerCards[messageID]
	delete(d.pickerCards, messageID)
	delete(d.pickerCardIDs, messageID)
	return card, hadCard
}

// pickerCardID returns the CardKit entity id cached for a picker card, or ""
// under the legacy engine / after the binding was dropped.
func (d *Dispatcher) pickerCardID(messageID string) string {
	d.cardMu.Lock()
	defer d.cardMu.Unlock()
	return d.pickerCardIDs[messageID]
}

// expirePicker runs in the TTL timer's goroutine. It flips the picker card to
// its expired form (grey + "已失效" footer) and clears the binding. A click
// racing this flip wins via cancelPickerExpiry (no cached bytes → no-op).
func (d *Dispatcher) expirePicker(messageID string) {
	d.cardMu.Lock()
	orig := d.pickerCards[messageID]
	cardID := d.pickerCardIDs[messageID]
	delete(d.pickerCards, messageID)
	delete(d.pickerCardIDs, messageID)
	delete(d.pickerTimers, messageID)
	d.cardMu.Unlock()
	if orig == nil {
		return
	}
	if expired, err := renderer.RenderInteractiveExpired(orig); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), noticeSendTimeout)
		defer cancel()
		_ = d.bot.UpdateCard(ctx, messageID, cardID, expired)
	}
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
	footer := cardkit.FooterInfo{Status: "待确认", Time: time.Now()}
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
// it binds the chat to the chosen backend and patches the picker card to its
// terminal state (green success / red failure) in place, so the whole switch
// produces only one message.
//
// The PATCH is delayed via a background goroutine because Feishu's card.
// action.trigger has a ~3-5s "click-handling window" during which any
// PatchMessage is silently reverted. Sleeping past the window lets the
// PATCH persist. The handler itself returns immediately so the WS ACK is
// not delayed (a delayed ACK triggers Feishu's 3-second timeout).
func (d *Dispatcher) handleBackendChoice(ctx context.Context, action *feishu.CardAction) error {
	// The user clicked; the picker can no longer expire. Cancel before any
	// return path so a late TTL flip cannot overwrite the outcome card.
	d.cancelPickerExpiry(action.MessageID)

	id, _ := action.Value["backendID"].(string)
	btype := d.registry.BackendType(id)
	var level, title, body string
	if btype == "" {
		level, title, body = "error", "后端离线", "backend "+id+" 已不在线。发送 /backend 重新选择。"
	} else if err := d.router.Set(action.ChatID, id); err != nil {
		level, title, body = "error", "切换失败", err.Error()
	} else {
		level, title, body = "success", "已切换后端", "当前后端: "+id+"（"+btype+"）"
	}
	card, err := d.renderBackendOutcome(action.ChatID, id, btype, level, title, body)
	if err != nil {
		return err
	}
	// Delayed PATCH: the click-handling window reverts an immediate PATCH,
	// so sleep past it before persisting the card. Background goroutine so
	// the ACK is not delayed.
	delay := d.cardPatchDelay
	if delay <= 0 {
		delay = cardPatchDelayDefault
	}
	msgID := action.MessageID
	cardID := d.pickerCardID(msgID)
	goSafe(func() {
		time.Sleep(delay)
		// WithoutCancel: this PATCH must outlive the click-handler request
		// (the 3-5s sleep crosses the handler's return). A request-scoped
		// ctx would already be canceled by the time UpdateCard runs, so we
		// detach the cancel signal while still inheriting request values
		// (tracing, ...) for observability. WithTimeout bounds the PATCH
		// itself so the goroutine never hangs.
		patchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeSendTimeout)
		defer cancel()
		if err := d.bot.UpdateCardVerified(patchCtx, msgID, cardID, card); err != nil {
			d.logger.Load().Warn("delayed picker UpdateCard failed",
				log.FieldMessageID, msgID,
				log.FieldError, err.Error())
		}
	})
	return nil
}

// renderBackendOutcome builds the terminal-state backend picker card in one
// of two colours: green (level="success") confirms a switch; red
// (level="error") reports the chosen backend went offline or the router
// rejected the binding. Every backend button is disabled so the card stays
// terminal; the chosen backend (still bound on the success path, or the
// unchanged prior binding on failure) is prefixed ✓.
func (d *Dispatcher) renderBackendOutcome(chatID, selectedID, selectedType, level, title, body string) ([]byte, error) {
	ids := d.registry.Registered()
	sort.Strings(ids)
	current, _ := d.router.Resolve(chatID)
	header := cardkit.HeaderInfo{Title: title, Template: backendOutcomeTemplate(level)}
	footer := cardkit.FooterInfo{BackendID: selectedID, BackendType: selectedType, Status: title, Time: time.Now()}
	actions := make([]cardkit.Action, 0, len(ids))
	for _, id := range ids {
		label := id + "（" + d.registry.BackendType(id) + "）"
		if id == current {
			label = "✓ " + label
		}
		actions = append(actions, cardkit.ButtonAction(label, "backend",
			map[string]any{"backendID": id}, id == current, true))
	}
	return cardkit.Card(header, footer, []cardkit.Element{cardkit.MarkdownElement(body)}, actions)
}

// backendOutcomeTemplate maps an outcome level to a header template colour.
func backendOutcomeTemplate(level string) string {
	if level == "success" {
		return "green"
	}
	return "red"
}
