package opencodebridge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencode"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// cmdSessionList lists every opencode session created under the chat's bound
// directory. Async: the CLI takes 25–50s to start (provider/config load
// before the subcommand runs), exceeding the dispatcher's 15s budget; the
// command returns immediately and the result lands on the command's progress
// card once the listing returns.
//
// The CLI's session store is cwd-bound, so the chat's directory MUST be set
// (via /cd) — a chat with no binding or no directory pin has nothing to list.
func (h *Handler) cmdSessionList(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	if b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 /cd 选择一个项目目录后再查看会话。"}, nil
	}
	replyToID := bridgebase.ReplyToID(ctx)
	h.emitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取会话列表，请稍候（约半分钟）…"},
	})
	dir := b.Directory
	curSession := b.SessionID
	bridgebase.GoSafe(h.Logger, "session-list:"+chatID, func() {
		sessions, err := h.agent.ListSessions(h.AppCtx, dir)
		if err != nil {
			h.emitPromptNotice(chatID, replyToID, "error", "获取失败", "获取会话列表失败："+err.Error())
			return
		}
		level := "info"
		title := "会话列表"
		if len(sessions) == 0 {
			level = "info"
			title = "无会话"
		}
		h.emitPromptNotice(chatID, replyToID, level, title, formatSessionList(sessions, curSession))
	})
	return commandResult{Handled: true}, nil
}

// cmdSessionClean deletes opencode sessions under the chat's bound directory.
// Forms (both require a prior /cd):
//   - /session-clean             → delete every session under dir EXCEPT the
//     currently bound one (the active conversation)
//   - /session-clean <sessionID> → delete only the given session by id
//
// Always async + confirmation: a permission card (确认 / 取消) pops up first,
// and only after the user confirms does deletion proceed. The CLI's slow
// startup (25–50s per fork) means N sessions take ~N×30s sequentially;
// parallel forks would multiply the heavy provider load for no throughput
// gain, so deletes run one at a time.
func (h *Handler) cmdSessionClean(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok {
		return commandResult{Body: "当前群尚无会话绑定。"}, nil
	}
	if b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 /cd 选择一个项目目录后再清理。"}, nil
	}
	replyToID := bridgebase.ReplyToID(ctx)
	dir := b.Directory
	curSession := b.SessionID

	// Direct-delete path: caller supplied an id. Skip the list fork and go
	// straight to confirmation. AskPermission blocks waiting for the user,
	// so it must run in its own goroutine to keep the dispatcher ctx free.
	if len(args) > 0 {
		target := args[0]
		// The current session is protected even via the explicit path:
		// deleting it would corrupt the in-flight binding (the next /current
		// or /session-new would point at a session the CLI no longer has).
		if target == curSession {
			return commandResult{Body: "不能删除当前绑定的会话；如需重置请用 /session-new。"}, nil
		}
		bridgebase.GoSafe(h.Logger, "session-clean-confirm:"+chatID, func() {
			h.runSessionCleanConfirm(chatID, replyToID, dir, []string{target}, false)
		})
		return commandResult{Handled: true}, nil
	}

	// Batch path: list, drop the current session, confirm, delete.
	h.emitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取会话列表，请稍候（约半分钟）…"},
	})
	bridgebase.GoSafe(h.Logger, "session-clean-list:"+chatID, func() {
		sessions, err := h.agent.ListSessions(h.AppCtx, dir)
		if err != nil {
			h.emitPromptNotice(chatID, replyToID, "error", "获取失败", "获取会话列表失败："+err.Error())
			return
		}
		targets := make([]string, 0, len(sessions))
		for _, s := range sessions {
			if s.ID == curSession {
				continue
			}
			targets = append(targets, s.ID)
		}
		if len(targets) == 0 {
			h.emitPromptNotice(chatID, replyToID, "info", "无可清理会话",
				"当前目录下没有可清理的会话（当前绑定的会话已保留）。")
			return
		}
		h.runSessionCleanConfirm(chatID, replyToID, dir, targets, true)
	})
	return commandResult{Handled: true}, nil
}

// runSessionCleanConfirm emits a permission card, waits for the user's pick,
// then either deletes the targets sequentially (确认) or reports the cancel
// (取消). The card patches in place via messageID so the flow stays on one
// surface. batch true means the targets came from a list filter (current
// session already excluded); false means a single explicit id.
func (h *Handler) runSessionCleanConfirm(chatID, replyToID, dir string, targets []string, batch bool) {
	count := len(targets)
	var msg string
	if batch {
		msg = fmt.Sprintf("即将删除当前目录下 %d 个会话（当前绑定的会话已保留）。确认继续？", count)
	} else {
		msg = fmt.Sprintf("即将删除会话 `%s`。确认继续？", targets[0])
	}
	opts := []protocol.PermissionOption{
		{Label: "确认删除", Value: "confirm"},
		{Label: "取消", Value: "cancel"},
	}
	choice, messageID, err := h.AskPermission(chatID, replyToID, "", "清理会话",
		protocol.PermissionMessage{Message: msg}, opts, true)
	if err != nil {
		h.emitPromptNotice(chatID, replyToID, "error", "清理失败", err.Error())
		return
	}
	if choice != "confirm" {
		h.emitCardUpdateLogged(chatID, messageID, "info", "已取消", "已取消清理，没有任何会话被删除。")
		return
	}
	var failed []string
	for _, id := range targets {
		if err := h.agent.DeleteSession(h.AppCtx, dir, id); err != nil {
			failed = append(failed, id)
			h.Logger.Warn("session delete failed",
				log.FieldChatID, chatID,
				log.FieldSessionID, id,
				log.FieldError, err)
		}
	}
	body, level := summarizeClean(count, failed)
	h.emitCardUpdateLogged(chatID, messageID, level, "清理完成", body)
}

// summarizeClean renders the post-deletion summary line. Full success is
// success-level; any failure drops to warning so the user notices which ids
// need a retry (typically "Session not found" because someone deleted them
// out-of-band between the list and the confirm).
func summarizeClean(count int, failed []string) (body, level string) {
	switch {
	case len(failed) == 0:
		return fmt.Sprintf("已删除 %d 个会话。", count), "success"
	case len(failed) < count:
		return fmt.Sprintf("已删除 %d/%d 个会话；失败 %d 个：%s。",
			count-len(failed), count, len(failed), strings.Join(failed, ", ")), "warning"
	default:
		return fmt.Sprintf("全部 %d 个会话删除失败（可能已不存在或被外部修改）。", count), "warning"
	}
}

// formatSessionList renders the listing for /session-list. Sorted by updated
// desc; the currently bound session (if any) is marked with ★ so the user
// sees which row /session-clean will keep.
func formatSessionList(sessions []opencode.Session, currentID string) string {
	if len(sessions) == 0 {
		return "当前目录下没有任何会话。"
	}
	sorted := make([]opencode.Session, len(sessions))
	copy(sorted, sessions)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Updated > sorted[j].Updated })

	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 目录下会话（%d）\n\n", len(sorted))
	for i, s := range sorted {
		marker := "  "
		if s.ID == currentID {
			marker = "★ "
		}
		title := s.Title
		if title == "" {
			title = "(未命名会话)"
		}
		fmt.Fprintf(&sb, "%s%d. %s\n", marker, i+1, title)
		fmt.Fprintf(&sb, "     🆔 %s\n", s.ID)
		fmt.Fprintf(&sb, "     ⏱️ %s\n", formatSessionTime(s.Updated))
	}
	return sb.String()
}

// formatSessionTime renders a millisecond timestamp as a relative string,
// matching opencodeservebridge.formatTime's bands so the two bridges read
// alike for the same input.
func formatSessionTime(ms int64) string {
	if ms == 0 {
		return "(未知)"
	}
	t := time.UnixMilli(ms)
	diff := time.Since(t)
	switch {
	case diff < time.Minute:
		return "刚刚"
	case diff < time.Hour:
		return fmt.Sprintf("%d分钟前", int(diff.Minutes()))
	case diff < 24*time.Hour:
		hours := int(diff.Hours())
		if hours == 1 {
			return "1小时前"
		}
		return fmt.Sprintf("%d小时前", hours)
	case diff < 7*24*time.Hour:
		days := int(diff.Hours() / 24)
		if days == 1 {
			return "1天前"
		}
		return fmt.Sprintf("%d天前", days)
	default:
		return t.Format("2006-01-02 15:04")
	}
}
