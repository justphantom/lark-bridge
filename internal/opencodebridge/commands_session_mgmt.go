package opencodebridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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
	h.EmitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取会话列表，请稍候（约半分钟）…"},
	})
	dir := b.Directory
	curSession := b.SessionID
	bridgebase.GoSafe(h.Logger, "session-list:"+chatID, func() {
		sessions, err := h.agent.ListSessions(h.AppCtx, dir)
		if err != nil {
			h.EmitPromptNotice(chatID, replyToID, "error", "获取失败", "获取会话列表失败："+err.Error())
			return
		}
		level := "info"
		title := "会话列表"
		if len(sessions) == 0 {
			level = "info"
			title = "无会话"
		}
		h.EmitPromptNotice(chatID, replyToID, level, title, formatSessionList(sessions, curSession))
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
	h.EmitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取会话列表，请稍候（约半分钟）…"},
	})
	bridgebase.GoSafe(h.Logger, "session-clean-list:"+chatID, func() {
		sessions, err := h.agent.ListSessions(h.AppCtx, dir)
		if err != nil {
			h.EmitPromptNotice(chatID, replyToID, "error", "获取失败", "获取会话列表失败："+err.Error())
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
			h.EmitPromptNotice(chatID, replyToID, "info", "无可清理会话",
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
		h.EmitPromptNotice(chatID, replyToID, "error", "清理失败", err.Error())
		return
	}
	if choice != "confirm" {
		h.EmitCardUpdateLogged(chatID, messageID, "info", "已取消", "已取消清理，没有任何会话被删除。")
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
	h.EmitCardUpdateLogged(chatID, messageID, level, "清理完成", body)
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
	sorted := sortSessionsByUpdated(sessions)

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

// sortSessionsByUpdated returns a copy of sessions sorted most-recent-first.
// /session-list (formatSessionList) and /session-use share this ordering so
// the 1-based numbering the user reads off /session-list matches the index
// they pass to /session-use <n>.
func sortSessionsByUpdated(sessions []opencode.Session) []opencode.Session {
	sorted := make([]opencode.Session, len(sessions))
	copy(sorted, sessions)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Updated > sorted[j].Updated })
	return sorted
}

// cmdSessionUse switches the chat's binding to another session of the same
// working directory. Forms:
//   - /session-use      → pop a selection card of the directory's sessions
//   - /session-use <n>  → switch directly to the n-th session of the sorted
//     list (1-based, same numbering as /session-list)
//
// CLI mode does not have a real-time busy-status RPC (the serve backend did
// via SessionStatuses), so a target session is always considered switchable;
// if another chat is mid-turn on the same session, that turn keeps running
// until it finishes or its chat issues /session-abort. The current chat's own
// in-flight turn is aborted before the binding is repointed.
func (h *Handler) cmdSessionUse(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	if b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 /cd 选择一个项目目录后再切换会话。"}, nil
	}
	if len(args) == 0 {
		return h.runSessionUsePicker(ctx, chatID), nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		//nolint:nilerr // 用户输入错误以提示文案返回；非内部错误，不上报 error 级别
		return commandResult{Body: fmt.Sprintf("会话序号必须是数字：%q", args[0])}, nil
	}
	replyToID := bridgebase.ReplyToID(ctx)
	dir := b.Directory
	curSession := b.SessionID
	// Loading banner on the command's progress card.
	h.EmitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取会话列表，请稍候（约半分钟）…"},
	})
	bridgebase.GoSafe(h.Logger, "session-use:"+chatID, func() {
		sessions, err := h.agent.ListSessions(h.AppCtx, dir)
		if err != nil {
			h.EmitPromptNotice(chatID, replyToID, "error", "切换失败", "获取会话列表失败："+err.Error())
			return
		}
		sorted := sortSessionsByUpdated(sessions)
		if len(sorted) == 0 {
			h.EmitPromptNotice(chatID, replyToID, "info", "无会话", "当前目录下没有任何会话。")
			return
		}
		if n < 1 || n > len(sorted) {
			h.EmitPromptNotice(chatID, replyToID, "error", "切换失败",
				fmt.Sprintf("会话序号 %d 越界，有效范围 1-%d。", n, len(sorted)))
			return
		}
		h.applySessionSwitch(chatID, replyToID, sorted[n-1], curSession, "")
	})
	return commandResult{Handled: true}, nil
}

// runSessionUsePicker drives the interactive session selection. ListSessions
// is slow (CLI forks ~15-30s), so the command returns immediately and the
// goroutine emits a Question card once the listing lands. The chosen label
// maps back to a session via a candidates map (each label is unique because
// of the 1-based number prefix).
func (h *Handler) runSessionUsePicker(ctx context.Context, chatID string) commandResult {
	replyToID := bridgebase.ReplyToID(ctx)
	h.EmitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取当前目录的会话，请稍候…"},
	})
	bridgebase.GoSafe(h.Logger, "session-use-picker:"+chatID, func() {
		b, ok := h.Router.Lookup(chatID)
		if !ok || b.Directory == "" {
			h.EmitPromptNotice(chatID, replyToID, "error", "切换失败", "尚未设置工作目录。")
			return
		}
		sessions, err := h.agent.ListSessions(h.AppCtx, b.Directory)
		if err != nil {
			h.EmitPromptNotice(chatID, replyToID, "error", "切换失败", "获取会话列表失败："+err.Error())
			return
		}
		sorted := sortSessionsByUpdated(sessions)
		if len(sorted) == 0 {
			h.EmitPromptNotice(chatID, replyToID, "info", "无会话", "当前目录下没有任何会话。")
			return
		}
		// Number prefix keeps every label unique so the choice maps to one
		// session; ★ marks the current binding so the user sees what they
		// would switch away from.
		options := make([]string, len(sorted))
		candidates := make(map[string]opencode.Session, len(sorted))
		for i, s := range sorted {
			title := s.Title
			if title == "" {
				title = "(未命名会话)"
			}
			label := fmt.Sprintf("%d. %s · %s", i+1, title, formatSessionTime(s.Updated))
			if s.ID == b.SessionID {
				label = "★ " + label
			}
			options[i] = label
			candidates[label] = s
		}
		choice, messageID, err := h.AskAndWait(chatID, replyToID, "会话", "选择要切换的会话",
			bridgebase.StaticOptions(options), false)
		if err != nil {
			h.EmitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
			return
		}
		sess, ok := candidates[choice]
		if !ok {
			h.EmitPromptNotice(chatID, replyToID, "error", "切换失败", "选项已失效，请重新发起 /session-use。")
			return
		}
		h.applySessionSwitch(chatID, messageID, sess, b.SessionID, "")
	})
	return commandResult{Handled: true}
}

// applySessionSwitch repoints the chat's binding to sess and emits the result
// notice. messageID is the card to patch: replyToID for the synchronous path
// (no picker card morphed), messageID for the picker path (patch the picker
// card in place).
//
// Switching to the session already bound is a no-op (binding untouched). Any
// in-flight turn on this chat is aborted first so the old session is not
// resumed mid-turn when the next prompt arrives.
func (h *Handler) applySessionSwitch(chatID, messageID string, sess opencode.Session, curSession, _ string) {
	if sess.ID == curSession {
		title := sess.Title
		if title == "" {
			title = "(未命名会话)"
		}
		h.EmitCardUpdateLogged(chatID, messageID, "info", "会话未切换",
			fmt.Sprintf("已是当前会话「%s」。", title))
		return
	}
	h.AbortChat(chatID)
	h.Router.SetSessionID(chatID, sess.ID)
	h.Logger.Info("session switched",
		log.FieldChatID, chatID,
		log.FieldSessionID, sess.ID)
	title := sess.Title
	if title == "" {
		title = "(未命名会话)"
	}
	h.EmitCardUpdateLogged(chatID, messageID, "success", "已切换会话",
		fmt.Sprintf("已切换到会话「%s」。旧会话保留，可用 /session-use 切回（/session-clean 会清理未绑定的会话）。", title))
}

// formatSessionTime renders a millisecond timestamp as a relative string.
// Bands follow the opencode-serve bridge's historical formatTime so former
// users see the same shape after the migration to CLI mode.
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
