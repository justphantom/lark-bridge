package feishufront

import (
	"context"
	"fmt"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/feishufront/renderer"
	"github.com/justphantom/lark-bridge/internal/protocol"
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
	case protocol.TypeToolUse, protocol.TypeToolResult, protocol.TypeProgress, protocol.TypeTodo:
		return d.updateProgress(ctx, ctrl, backendType)
	case protocol.TypeText:
		// 文本预览不再展示,忽略(完整回复由终态结果卡承载)
		return nil
	case protocol.TypeThinking:
		// thinking 不再展示,忽略
		return nil
	case protocol.TypeResult, protocol.TypeError, protocol.TypeNotice:
		if ctrl.PromptID != "" && !d.terminals.Add(ctrl.PromptID) {
			return nil
		}
		if ctrl.Type == protocol.TypeResult {
			return d.sendResult(ctx, ctrl, backendType)
		}
		return d.sendNoticeControl(ctx, ctrl, backendType)
	case protocol.TypeQuestion:
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
		state.AddToolUse(ctrl.ToolUse.Name, ctrl.ToolUse.Input, ctrl.ToolUse.IsSubagent, ctrl.ToolUse.TaskID)
	case protocol.TypeToolResult:
		state.AddToolResult(ctrl.ToolResult.Name, ctrl.ToolResult.Input, ctrl.ToolResult.Output, ctrl.ToolResult.IsError, ctrl.ToolResult.IsSubagent, ctrl.ToolResult.TaskID)
	case protocol.TypeProgress:
		state.AddProgress()
	case protocol.TypeTodo:
		state.AddTodo(toRendererTodos(ctrl.Todo.Todos))
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
	}
	// Release turn/progress slots whether the send succeeded or not, so a
	// transient Feishu error does not leak the promptID across the maps.
	d.turns.Finish(ctrl.PromptID)
	d.cleanupProgress(ctrl.PromptID, messageID)
	return err
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
