package claudebridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// cmdSessionClean deletes Claude sessions under the chat's bound directory.
// Forms (both require a prior /cd):
//   - /clean             → delete every session under dir EXCEPT the
//     currently bound one (the active conversation)
//   - /clean <sessionID> → delete only the given session by id
//
// Always confirmation-gated: a permission card (确认 / 取消) pops up first,
// and only after the user confirms does deletion proceed. Deletion is a
// filesystem op (the CLI has no per-session delete subcommand; `claude
// project purge` is project-wide and too coarse). The currently bound session
// is ALWAYS protected — deleting it would make the next --resume hit
// IsStaleSession (event.go:109-114). Confirmation uses AskPermission, which
// blocks for the user's pick, so it runs in a goroutine to keep the
// dispatcher ctx free.
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

	// Direct-delete path: caller supplied an id. Skip the list scan and go
	// straight to confirmation. The current session is protected even via the
	// explicit path (deleting it corrupts the in-flight binding).
	if len(args) > 0 {
		target := args[0]
		if target == curSession {
			return commandResult{Body: "不能删除当前绑定的会话；如需重置请用 /new。"}, nil
		}
		bridgebase.GoSafe(h.Logger, "session-clean-confirm:"+chatID, func() {
			h.runSessionCleanConfirm(chatID, replyToID, dir, []string{target}, false)
		})
		return commandResult{Handled: true}, nil
	}

	// Batch path: list, drop the current session, confirm, delete. ListSessions
	// is a sub-ms filesystem scan, so run it inline (no loading banner needed)
	// before entering the blocking confirmation goroutine.
	sessions, err := h.agent.ListSessions(h.AppCtx, dir)
	if err != nil {
		return commandResult{Body: "获取会话列表失败：" + err.Error()}, nil //nolint:nilerr // 用户输入错误以提示文案返回；非内部错误，不上报 error 级别
	}
	targets := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s.ID == curSession {
			continue
		}
		targets = append(targets, s.ID)
	}
	if len(targets) == 0 {
		return commandResult{Body: "当前目录下没有可清理的会话（当前绑定的会话已保留）。"}, nil
	}
	bridgebase.GoSafe(h.Logger, "session-clean-confirm:"+chatID, func() {
		h.runSessionCleanConfirm(chatID, replyToID, dir, targets, true)
	})
	return commandResult{Handled: true}, nil
}

// runSessionCleanConfirm emits a permission card, waits for the user's pick,
// then either deletes the targets (确认) or reports the cancel (取消). The
// card patches in place via messageID so the flow stays on one surface. batch
// true means the targets came from a list filter (current session already
// excluded); false means a single explicit id.
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
// need a retry.
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

// cmdSessionUse switches the chat's binding to another session of the same
// working directory. Forms:
//   - /use      → pop a selection card of the directory's sessions
//     (excluding the currently bound one — switching to itself is a no-op)
//   - /use <n>  → switch directly to the n-th session of the sorted
//     list (1-based, same numbering as /session)
//
// Resume is cwd-bound (verified): every candidate lives under the bound
// directory, so repointing the binding + letting the next --resume run is
// sufficient — no new CLI flag. The current chat's in-flight turn is aborted
// before the binding is repointed so the old session is not resumed mid-turn.
func (h *Handler) cmdSessionUse(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	if b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 /cd 选择一个项目目录后再切换会话。"}, nil
	}
	replyToID := bridgebase.ReplyToID(ctx)
	dir := b.Directory
	curSession := b.SessionID
	sessions, err := h.agent.ListSessions(h.AppCtx, dir)
	if err != nil {
		return commandResult{Body: "获取会话列表失败：" + err.Error()}, nil //nolint:nilerr // 用户输入错误以提示文案返回；非内部错误，不上报 error 级别
	}
	if len(sessions) == 0 {
		return commandResult{Body: "当前目录下没有任何会话。"}, nil
	}
	sorted := sortSessionsByUpdated(sessions)

	// Picker form: list the directory's sessions and let the user choose.
	if len(args) == 0 {
		return h.runSessionUsePicker(chatID, replyToID, sorted, curSession), nil
	}

	n, err := strconv.Atoi(args[0])
	if err != nil {
		//nolint:nilerr // 用户输入错误以提示文案返回；非内部错误，不上报 error 级别
		return commandResult{Body: fmt.Sprintf("会话序号必须是数字：%q", args[0])}, nil
	}
	if n < 1 || n > len(sorted) {
		return commandResult{Body: fmt.Sprintf("会话序号 %d 越界，有效范围 1-%d。", n, len(sorted))}, nil
	}
	h.applySessionSwitch(chatID, replyToID, sorted[n-1], curSession)
	return commandResult{Handled: true}, nil
}

// runSessionUsePicker drives the interactive session selection. The directory
// scan is synchronous (sub-ms); the picker itself (AskAndWait) blocks for the
// user's pick, so it runs in a goroutine to keep the dispatcher ctx free. The
// chosen label maps back to a session via a candidates map (each label is
// unique because of the 1-based number prefix).
func (h *Handler) runSessionUsePicker(chatID, replyToID string, sorted []claude.Session, curSession string) commandResult {
	bridgebase.GoSafe(h.Logger, "session-use-picker:"+chatID, func() {
		options := make([]string, 0, len(sorted))
		candidates := make(map[string]claude.Session, len(sorted))
		for i, s := range sorted {
			title := s.Title
			if title == "" {
				title = "(未命名会话)"
			}
			label := fmt.Sprintf("%d. %s · %s", i+1, title, claude.FormatSessionTime(s.Updated))
			if s.ID == curSession {
				label = "★ " + label
			}
			options = append(options, label)
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
			h.EmitPromptNotice(chatID, replyToID, "error", "切换失败", "选项已失效，请重新发起 /use。")
			return
		}
		h.applySessionSwitch(chatID, messageID, sess, curSession)
	})
	return commandResult{Handled: true}
}

// applySessionSwitch repoints the chat's binding to sess and emits the result
// notice. messageID is the card to patch: replyToID for the synchronous path
// (no picker card morphed), messageID for the picker path (patch the picker
// card in place). Switching to the session already bound is a no-op.
func (h *Handler) applySessionSwitch(chatID, messageID string, sess claude.Session, curSession string) {
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
		fmt.Sprintf("已切换到会话「%s」。旧会话保留，可用 /use 切回（/clean 会清理未绑定的会话）。", title))
}

// sortSessionsByUpdated returns a copy of sessions sorted most-recent-first.
// /session (formatSessionList) and /use share this ordering so
// the 1-based numbering the user reads off /session matches the index
// they pass to /use <n>.
func sortSessionsByUpdated(sessions []claude.Session) []claude.Session {
	sorted := make([]claude.Session, len(sessions))
	copy(sorted, sessions)
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1].Updated < sorted[i].Updated {
			// insertion-sort is fine: session counts per directory are small.
			for j := i; j > 0 && sorted[j-1].Updated < sorted[j].Updated; j-- {
				sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
			}
		}
	}
	return sorted
}

// formatSessionList renders the listing for /session. Sorted by updated
// desc; the currently bound session (if any) is marked with ★ so the user
// sees which row /clean will keep.
func formatSessionList(sessions []claude.Session, currentID string) string {
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
		fmt.Fprintf(&sb, "     ⏱️ %s\n", claude.FormatSessionTime(s.Updated))
	}
	return sb.String()
}
