package ompbridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// cmdSessionList lists every omp session created under the chat's bound
// directory. Unlike opencode, this reads the filesystem directly (omp session
// headers are plain JSONL), so it completes synchronously without a progress
// banner.
//
// The chat's directory MUST be set (via /cd) — a chat with no binding or no
// directory pin has nothing to list.
func (h *Handler) cmdSessionList(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	if b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 /cd 选择一个项目目录后再查看会话。"}, nil
	}
	sessions, err := h.agent.ListSessions(ctx, b.Directory)
	if err != nil {
		return commandResult{Body: "获取会话列表失败：" + err.Error()}, err
	}
	return commandResult{Body: formatSessionList(sessions, b.SessionID)}, nil
}

// cmdSessionUse switches the chat's binding to another session of the same
// working directory. Forms:
//   - /session-use      → pop a selection card of the directory's sessions
//   - /session-use <n>  → switch directly to the n-th session of the sorted
//     list (1-based, same numbering as /session-list)
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
	h.EmitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取会话列表…"},
	})
	bridgebase.GoSafe(h.Logger, "session-use:"+chatID, func() {
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
		if n < 1 || n > len(sorted) {
			h.EmitPromptNotice(chatID, replyToID, "error", "切换失败",
				fmt.Sprintf("会话序号 %d 越界，有效范围 1-%d。", n, len(sorted)))
			return
		}
		h.applySessionSwitch(chatID, replyToID, sorted[n-1], b.SessionID)
	})
	return commandResult{Handled: true}, nil
}

// runSessionUsePicker drives the interactive session selection.
func (h *Handler) runSessionUsePicker(ctx context.Context, chatID string) commandResult {
	replyToID := bridgebase.ReplyToID(ctx)
	h.EmitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取当前目录的会话…"},
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
		options := make([]string, len(sorted))
		candidates := make(map[string]omp.Session, len(sorted))
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
		h.applySessionSwitch(chatID, messageID, sess, b.SessionID)
	})
	return commandResult{Handled: true}
}

// applySessionSwitch repoints the chat's binding to sess and emits the result
// notice.
func (h *Handler) applySessionSwitch(chatID, messageID string, sess omp.Session, curSession string) {
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
		fmt.Sprintf("已切换到会话「%s」。旧会话保留，可用 /session-use 切回。", title))
}

// formatSessionList renders the listing for /session-list. Sorted by updated
// desc; the currently bound session (if any) is marked with ★.
func formatSessionList(sessions []omp.Session, currentID string) string {
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
func sortSessionsByUpdated(sessions []omp.Session) []omp.Session {
	sorted := make([]omp.Session, len(sessions))
	copy(sorted, sessions)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Updated > sorted[j].Updated })
	return sorted
}

// formatSessionTime renders a millisecond timestamp as a relative string.
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

// cmdSessionClean deletes omp sessions in the chat's bound directory. Forms:
//   - /session-clean      → delete every session except the current binding
//   - /session-clean <id> → delete the specified session id
//
// Both forms require confirmation via a permission card.
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

	if len(args) > 0 {
		target := args[0]
		if target == curSession {
			return commandResult{Body: "不能删除当前绑定的会话；如需重置请用 /session-new。"}, nil
		}
		bridgebase.GoSafe(h.Logger, "session-clean-confirm:"+chatID, func() {
			h.runSessionCleanConfirm(chatID, replyToID, dir, []string{target}, false)
		})
		return commandResult{Handled: true}, nil
	}

	h.EmitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🔍 正在获取当前目录的会话…"},
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
// then deletes the targets. The card patches in place via messageID.
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

// summarizeClean renders the post-deletion summary line.
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

// cmdSessionNew resets the bound session id so the next prompt starts a fresh
// omp conversation. The working directory is preserved (files on disk
// remain); only the conversational context (the --resume id) is dropped. Any
// in-flight prompt is aborted first so the old session is not resumed
// mid-turn.
func (h *Handler) cmdSessionNew(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.Router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	h.AbortChat(chatID)
	h.Router.SetSessionID(chatID, "")
	h.Logger.Info("session reset (new conversation)", log.FieldChatID, chatID)
	return commandResult{Body: "已重置会话上下文。工作目录保留，发送消息即开始新对话。"}, nil
}

// cmdSessionDel removes the binding entirely; the next prompt recreates a
// fresh binding (new directory + new session). Use /session-new to keep the
// directory but reset the conversation.
func (h *Handler) cmdSessionDel(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.Router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无会话绑定。"}, nil
	}
	h.AbortChat(chatID)
	h.Router.Unbind(chatID)
	h.Logger.Info("binding deleted", log.FieldChatID, chatID)
	return commandResult{Body: "已删除会话绑定。下次提问将创建新会话与新目录。"}, nil
}

// cmdCurrent shows the current binding's directory, session id, model,
// approval mode and thinking level. If the chat has no binding yet (no
// conversation started), one is created lazily so the command reflects the
// pre-prompt configuration.
func (h *Handler) cmdCurrent(_ context.Context, chatID string, _ []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	var sb strings.Builder
	sb.WriteString("当前会话：\n")
	fmt.Fprintf(&sb, "  目录：%s\n", b.Directory)
	sid := b.SessionID
	if sid == "" {
		sid = "(未创建，首次提问后生成)"
	}
	fmt.Fprintf(&sb, "  会话ID：%s\n", sid)
	model := b.ModelSpec
	if model == "" {
		model = "(默认)"
	}
	fmt.Fprintf(&sb, "  模型：%s\n", model)
	perm := b.PermissionMode
	if perm == "" {
		perm = "默认 (" + h.PermissionDefault + ")"
	}
	fmt.Fprintf(&sb, "  审批模式：%s\n", perm)
	effort := b.EffortLevel
	if effort == "" {
		effort = "默认 (" + h.thinkingDefault + ")"
	}
	fmt.Fprintf(&sb, "  思考级别：%s\n", effort)
	return commandResult{Body: sb.String()}, nil
}

// cmdHelp returns the auto-generated command list.
func (*Handler) cmdHelp(_ context.Context, _ string, _ []string) (commandResult, error) {
	return commandResult{Body: renderCmdHelp()}, nil
}
