package feishufront

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/feishufront/renderer"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// DispatchControl routes a backend Control to the right card update path.
// Terminal controls (result/error/notice) are de-duplicated per PromptID via
// the terminals set so a replayed stream cannot flip a finalised card twice.
func (d *Dispatcher) DispatchControl(ctx context.Context, rc RoutedControl) error {
	ctrl := rc.Control
	backendType := d.registry.BackendType(rc.BackendID)
	switch ctrl.Type {
	case protocol.TypeSessionInit:
		if si := ctrl.SessionInit; si != nil {
			d.turns.SetSession(ctrl.PromptID, si.SessionID, si.Model)
		}
		return d.updateProgress(ctx, ctrl, backendType)
	case protocol.TypeToolUse, protocol.TypeToolResult, protocol.TypeProgress, protocol.TypeTodo, protocol.TypeThinking:
		return d.updateProgress(ctx, ctrl, backendType)
	case protocol.TypeText:
		// 文本预览不再展示,忽略(完整回复由终态结果卡承载)
		return nil
	case protocol.TypeResult, protocol.TypeError, protocol.TypeNotice:
		// Terminals dedup keyed by PromptID: the FIRST terminal of any kind
		// (Result / Error / Notice) wins; subsequent ones for the same
		// PromptID are dropped. This intentionally couples Notice and Result
		// under one key — semantic invariant: a turn that has emitted any
		// terminal frame (a result card OR a final error/notice) is
		// considered finalized, so a late-arriving duplicate (e.g. an
		// already-handled error followed by a redundant Result) does not
		// stack a second card. If a future protocol change ever allows a
		// Notice to precede a still-desired Result for the same prompt,
		// switch the key to PromptID+Type (per-type bucketing).
		if ctrl.PromptID != "" && !d.terminals.Add(ctrl.PromptID) {
			return nil
		}
		if ctrl.Type == protocol.TypeResult {
			return d.sendResult(ctx, ctrl, backendType)
		}
		return d.sendNoticeControl(ctx, ctrl, backendType)
	case protocol.TypeStatusReport:
		// Periodic broadcast from the status-monitor backend. Deliberately NOT
		// routed through the terminal dedup set (it is keyed by PromptID and
		// would swallow every tick after the first) nor the card debouncer
		// (which coalesces high-frequency progress frames, not whole-card
		// periodic replacements).
		return d.sendStatusReport(ctx, rc)
	case protocol.TypeQuestion, protocol.TypePermission:
		return d.sendInteractive(ctx, ctrl, backendType)
	default:
		return fmt.Errorf("dispatcher: unknown control type %q", ctrl.Type)
	}
}

// resolveFooter returns the Turn snapshot, effective chatID, and a pre-filled
// FooterInfo. ok reports whether a turn exists; when false the returned turn
// is the zero value and chatID falls back to ctrl.ChatID. The footer's Elapsed
// is filled from the turn's start time; the caller sets Status per card type.
func (d *Dispatcher) resolveFooter(ctrl *protocol.Control, backendType string) (turn Turn, ok bool, chatID string, footer cardkit.FooterInfo) {
	turn, ok = d.turns.Get(ctrl.PromptID)
	chatID = ctrl.ChatID
	footer = cardkit.FooterInfo{BackendType: backendType}
	if ok {
		chatID = turn.ChatID
		footer.BackendID = turn.BackendID
		footer.Model = turn.Model
		footer.SessionID = turn.SessionID
		footer.Elapsed = cardkit.FormatElapsed(time.Since(turn.StartedAt))
	}
	if chatID == "" {
		chatID = ctrl.ChatID
	}
	return turn, ok, chatID, footer
}

func (d *Dispatcher) updateProgress(ctx context.Context, ctrl *protocol.Control, backendType string) error {
	turn, ok := d.turns.Get(ctrl.PromptID)
	if !ok {
		return nil
	}
	d.progressMu.Lock()
	state, exists := d.progress[ctrl.PromptID]
	if !exists {
		state = renderer.NewProgressState()
		d.progress[ctrl.PromptID] = state
	}
	switch ctrl.Type {
	case protocol.TypeSessionInit:
		// No state mutation; just re-render so footer picks up session/model.
	case protocol.TypeToolUse:
		// A non-nil Subagent (claude local_agent task_started/task_progress;
		// opencode has no running-phase event) routes to the dedicated
		// subagent zone instead of the leaf-tool-row path. Nil Subagent
		// keeps the legacy row (local_bash, or older backends) — the
		// IsSubagent flag still marks it for category counting.
		if ctrl.ToolUse.Subagent != nil {
			state.AddSubagentUse(toRendererSubagent(ctrl.ToolUse.Subagent))
		} else {
			state.AddToolUse(ctrl.ToolUse.Name, ctrl.ToolUse.Input, ctrl.ToolUse.IsSubagent, ctrl.ToolUse.TaskID)
		}
	case protocol.TypeToolResult:
		if ctrl.ToolResult.Subagent != nil {
			state.AddSubagentResult(toRendererSubagent(ctrl.ToolResult.Subagent))
		} else {
			state.AddToolResult(ctrl.ToolResult.Name, ctrl.ToolResult.Input, ctrl.ToolResult.Output, ctrl.ToolResult.IsError, ctrl.ToolResult.IsSubagent, ctrl.ToolResult.TaskID)
		}
	case protocol.TypeProgress:
		state.AddProgress()
		// Description/Gate render through the same banner slot. A gate
		// (blocking) wins over a loading notice; SetGate/SetLoading keep
		// the precedence in the renderer. Conversion at the boundary keeps
		// the renderer free of a protocol import (same convention as Todo).
		if g := ctrl.Progress.Gate; g != nil {
			state.SetGate(renderer.GateInfo{State: g.State, Kind: g.Kind, Summary: g.Summary})
		} else if d := ctrl.Progress.Description; d != "" {
			state.SetLoading(d)
		}
	case protocol.TypeTodo:
		state.AddTodo(toRendererTodos(ctrl.Todo.Todos))
	case protocol.TypeThinking:
		state.SetThinking(ctrl.Thinking.Delta, ctrl.Thinking.Replace)
	}
	// Clone under the lock so the expensive Render+Marshal runs outside
	// progressMu — otherwise concurrent turns serialise on each render.
	snapshot := state.Clone()
	d.progressMu.Unlock()

	header := cardkit.HeaderInfo{BackendType: backendType, Title: "处理中", Template: "blue"}
	footer := cardkit.FooterInfo{BackendID: turn.BackendID, BackendType: backendType, Model: turn.Model, SessionID: turn.SessionID, Status: "处理中", Elapsed: cardkit.FormatElapsed(time.Since(turn.StartedAt))}
	card, err := snapshot.Render(header, footer)
	if err != nil {
		return err
	}
	return d.updateCard(ctx, turn.MessageID, card)
}

// toRendererTodos converts protocol.TodoItem → renderer.TodoItem at the package
// boundary so the renderer never imports protocol. Field-for-field copy; the
// slices are disjoint so the renderer's overwrite cannot leak back into the
// protocol control.
func toRendererTodos(items []protocol.TodoItem) []renderer.TodoItem {
	out := make([]renderer.TodoItem, len(items))
	for i, it := range items {
		out[i] = renderer.TodoItem{Content: it.Content, Status: it.Status, Priority: it.Priority}
	}
	return out
}

// toRendererSubagent converts protocol.SubagentSummary → renderer.SubagentInfo
// at the package boundary, same convention as toRendererTodos. The renderer
// keeps its own type so a protocol field rename or addition does not ripple
// into the renderer package; conversion is the single touchpoint.
func toRendererSubagent(s *protocol.SubagentSummary) renderer.SubagentInfo {
	return renderer.SubagentInfo{
		Status:       s.Status,
		TaskType:     s.TaskType,
		Type:         s.Type,
		Title:        s.Title,
		Description:  s.Description,
		ChildSession: s.ChildSession,
		Model:        s.Model,
		DurationMs:   s.DurationMs,
		ToolUses:     s.ToolUses,
		LastToolName: s.LastToolName,
		TotalTokens:  s.TotalTokens,
		Preview:      s.Preview,
		OutputBytes:  s.OutputBytes,
		Truncated:    s.Truncated,
	}
}

// sendTerminalCard ships a terminal card (result or notice) and unconditionally
// releases the turn/progress slots bound to promptID, whether the send
// succeeded or not. It tries to update the existing progress card in place
// first (so a terminal reply replaces the "starting" placeholder), falling
// back to a fresh card. finalizeInteractive also closes a linked interactive
// card on success (the result path); the notice path passes false.
func (d *Dispatcher) sendTerminalCard(ctx context.Context, promptID, chatID, messageID string, card []byte, finalizeInteractive bool) error {
	if messageID != "" {
		if uerr := d.bot.UpdateCard(ctx, messageID, card); uerr == nil {
			if finalizeInteractive {
				d.finalizeLinkedInteractive(ctx, promptID)
			}
			d.turns.Finish(promptID)
			d.cleanupProgress(promptID, messageID)
			return nil
		}
	}
	_, err := d.bot.SendCard(ctx, chatID, card, "")
	if err == nil {
		if finalizeInteractive {
			d.finalizeLinkedInteractive(ctx, promptID)
		}
		d.turns.Finish(promptID)
		d.cleanupProgress(promptID, messageID)
	} else {
		// Both the in-place UpdateCard and the fresh SendCard failed: still
		// release the turn/progress slots so the promptID does not leak.
		d.turns.Finish(promptID)
		d.cleanupProgress(promptID, messageID)
	}
	return err
}

func (d *Dispatcher) sendResult(ctx context.Context, ctrl *protocol.Control, backendType string) error {
	turn, ok, chatID, footer := d.resolveFooter(ctrl, backendType)
	messageID := ""
	if ok {
		messageID = turn.MessageID
	}
	// Snapshot the execution summary before cleanupProgress drops the state.
	// Reads under progressMu to race against concurrent updateProgress writes.
	summary := ""
	d.progressMu.Lock()
	if st := d.progress[ctrl.PromptID]; st != nil {
		summary = st.Summary()
	}
	d.progressMu.Unlock()
	footer.Status = "已完成"
	header := cardkit.HeaderInfo{BackendType: backendType}
	d.logger.Load().Debug("sendResult",
		"prompt_id", ctrl.PromptID,
		"text_len", len(ctrl.Result.Text),
		"summary_len", len(summary))
	card, err := renderer.RenderResult(ctrl, header, footer, summary)
	if err != nil {
		// Drop the in-memory turn/progress so a render failure does not leak
		// the promptID across the maps for the process lifetime.
		d.turns.Finish(ctrl.PromptID)
		d.cleanupProgress(ctrl.PromptID, messageID)
		return err
	}
	// Flush pending debounced updates so the progress card freezes at its
	// last frame before the standalone result card ships.
	if d.debouncer != nil {
		d.debouncer.flush()
	}
	// Mark finalized so a straggler progress update for the same messageID
	// cannot enqueue a stale frame that the next debouncer tick would flush
	// over the frozen progress card.
	d.markFinalized(messageID)
	// TypeResult ships a fresh standalone card (replyToID=""): the in-flight
	// progress card stays frozen at its last frame so the user can review
	// the process alongside the final reply. Linked interactive cards are
	// still finalised so they don't sit grey forever.
	_, err = d.bot.SendCard(ctx, chatID, card, "")
	if err == nil {
		d.finalizeLinkedInteractive(ctx, ctrl.PromptID)
	} else if errors.Is(err, feishu.ErrCardContentRejected) && ctrl.Result.Text != "" {
		// Feishu rejected the card's CONTENT (too many tables/elements, body
		// too large, …) — not a transient failure. Deliver the reply as plain
		// text so it is not lost. The reply is capped to the text-message
		// budget; markdown tables render as raw "| … |" text (ugly but
		// readable, and crucially delivered). err is replaced: success clears
		// the rejection; a SendText failure is the new error to surface.
		err = d.sendResultTextFallback(ctx, chatID, ctrl.Result.Text)
		if err == nil {
			d.finalizeLinkedInteractive(ctx, ctrl.PromptID)
		}
	}
	// Release turn/progress slots whether the send succeeded or not, so a
	// transient Feishu error does not leak the promptID across the maps.
	d.turns.Finish(ctrl.PromptID)
	d.cleanupProgress(ctrl.PromptID, messageID)
	return err
}

// sendResultTextFallback delivers reply as a plain-text message after the
// result CARD was rejected (e.g. ErrCode 11310 — too many tables). text is
// byte-capped (rune-boundary-safe) to fit a Feishu text message, with a
// trailing truncation marker when cut. Returns the SendText error so the
// caller can decide whether to surface it.
func (d *Dispatcher) sendResultTextFallback(ctx context.Context, chatID, reply string) error {
	body := strutil.Truncate(reply, feishu.MaxTextBodyBytes-40)
	if len(body) < len(reply) {
		body += "\n\n…（内容过长，已截断）"
	}
	d.logger.Load().Info("result card rejected; falling back to plain text",
		log.FieldChatID, chatID,
		"reply_len", len(reply),
		"text_body_len", len(body))
	if _, err := d.bot.SendText(ctx, chatID, body, ""); err != nil {
		return fmt.Errorf("feishu: result text fallback: %w", err)
	}
	return nil
}

func (d *Dispatcher) sendNoticeControl(ctx context.Context, ctrl *protocol.Control, backendType string) error {
	// A notice carrying UpdateMessageID patches an existing standalone card
	// (e.g. a submitted question card) instead of sending a new one.
	if n := ctrl.Notice; n != nil && n.UpdateMessageID != "" {
		footer := cardkit.FooterInfo{BackendType: backendType, Status: noticeFooterStatus(n.Level, n.Title)}
		card, err := cardkit.Notice(footer, n.Level, n.Title, n.Message, n.Field, n.Before, n.After)
		if err != nil {
			return err
		}
		return d.bot.UpdateCard(ctx, n.UpdateMessageID, card)
	}

	d.cleanupProgress(ctrl.PromptID, "")
	turn, ok, chatID, footer := d.resolveFooter(ctrl, backendType)
	messageID := ""
	if ok {
		messageID = turn.MessageID
	}
	level, title, msg := "info", "提示", ""
	field, before, after := "", "", ""
	if n := ctrl.Notice; n != nil {
		level = n.Level
		if level == "" {
			level = "info"
		}
		title = n.Title
		if title == "" {
			title = "提示"
		}
		msg = n.Message
		field, before, after = n.Field, n.Before, n.After
	} else if e := ctrl.Error; e != nil {
		level, title, msg = "error", "错误", e.Message
	}
	footer.Status = noticeFooterStatus(level, title)
	card, err := cardkit.Notice(footer, level, title, msg, field, before, after)
	if err != nil {
		d.turns.Finish(ctrl.PromptID)
		return err
	}
	// Update the existing progress card in place when there is one, so a
	// slash command (whose reply arrives as a TypeNotice) replaces the
	// "starting" placeholder instead of leaving it orphaned next to a new
	// notice card. Fall back to a fresh card only when no progress card exists
	// or the update fails.
	if d.debouncer != nil {
		d.debouncer.flush()
	}
	// Mark finalized so a straggler progress frame cannot overwrite this notice.
	d.markFinalized(messageID)
	return d.sendTerminalCard(ctx, ctrl.PromptID, chatID, messageID, card, false)
}

// sendStatusReport broadcasts the standing overview card to every chat bound
// to the status-monitor backend that sent rc. The frontend, not the backend,
// owns the per-chat messageID bookkeeping: each (chatID, reportKey) pair maps
// to one card PATCHed in place every tick; a card the user withdrew
// (feishu.IsCardGone on PATCH) is dropped from the map and re-sent fresh.
func (d *Dispatcher) sendStatusReport(ctx context.Context, rc RoutedControl) error {
	p := rc.Control.StatusReport
	if p == nil || p.Key == "" {
		return nil
	}
	chats := d.router.ChatsOf(rc.BackendID)
	if len(chats) == 0 {
		// No bound chat yet: nothing to refresh. Common until a user /backend-binds.
		return nil
	}
	footer := cardkit.FooterInfo{BackendID: rc.BackendID, BackendType: "status-monitor", Status: "总览"}
	rows := make([]cardkit.TurnRow, len(p.Turns))
	for i, t := range p.Turns {
		rows[i] = cardkit.TurnRow{BackendID: t.BackendID, ChatID: t.ChatID, ElapsedS: t.ElapsedS}
	}
	card, err := cardkit.StatusReport(footer, p.Title, p.GeneratedAt, p.IntervalS, p.InFlight, p.Backends, rows)
	if err != nil {
		return err
	}
	for _, chatID := range chats {
		d.patchOrCreateStatusCard(ctx, chatID, p.Key, card)
		// Refresh lastAccess so the 14-day sweeper does not drop a chat whose
		// only traffic is this periodic card (ChatsOf alone never touches it).
		d.router.Touch(chatID)
	}
	return nil
}

// patchOrCreateStatusCard PATCHes the cached card for (chatID, key), or
// SendCards a new one when none is cached or the prior one was withdrawn. A
// transient (non-gone) PATCH error leaves the cached messageID in place so the
// next tick retries the same card instead of stacking a duplicate.
func (d *Dispatcher) patchOrCreateStatusCard(ctx context.Context, chatID, key string, card []byte) {
	mapKey := chatID + "\x00" + key
	d.statusMu.Lock()
	msgID := d.statusCards[mapKey]
	d.statusMu.Unlock()

	if msgID != "" {
		err := d.bot.UpdateCard(ctx, msgID, card)
		if err == nil {
			return
		}
		if !feishu.IsCardGone(err) {
			// Network/rate-limit class: keep the cached id, retry next tick.
			d.logger.Load().Warn("status card update failed",
				log.FieldChatID, chatID, log.FieldMessageID, msgID, log.FieldError, err)
			return
		}
		// Card withdrawn: drop the stale id and fall through to re-send.
		d.statusMu.Lock()
		delete(d.statusCards, mapKey)
		d.statusMu.Unlock()
	}

	newID, err := d.bot.SendCard(ctx, chatID, card, "")
	if err != nil {
		d.logger.Load().Warn("status card send failed",
			log.FieldChatID, chatID, log.FieldError, err)
		return
	}
	d.statusMu.Lock()
	d.statusCards[mapKey] = newID
	d.statusMu.Unlock()
}

func (d *Dispatcher) notice(ctx context.Context, chatID, level, title, message string) error {
	card, err := cardkit.Notice(cardkit.FooterInfo{Status: "通知", Time: time.Now()}, level, title, message, "", "", "")
	if err != nil {
		return err
	}
	_, err = d.bot.SendCard(ctx, chatID, card, "")
	return err
}

// cleanupProgress removes the progress state for a finished prompt and clears
// its finalized marker so the messageID slot does not leak.
func (d *Dispatcher) cleanupProgress(promptID, messageID string) {
	d.progressMu.Lock()
	delete(d.progress, promptID)
	if messageID != "" {
		delete(d.finalized, messageID)
	}
	d.progressMu.Unlock()
}

// markFinalized records that messageID's terminal card has been sent, so any
// later progress update for it is dropped at updateCard instead of overwriting
// the final card via the debouncer.
func (d *Dispatcher) markFinalized(messageID string) {
	if messageID == "" {
		return
	}
	d.progressMu.Lock()
	d.finalized[messageID] = struct{}{}
	d.progressMu.Unlock()
}

// noticeFooterStatus picks the footer state word for a notice/error card. A
// cancellation (level info with a "取消"/"超时" title, emitted by emitTerminal)
// reads as 已取消/超时; errors read as 错误; everything else is a plain 通知.
func noticeFooterStatus(level, title string) string {
	if level == "error" {
		return "错误"
	}
	switch title {
	case "已取消", "请求超时":
		return title
	}
	return "通知"
}
