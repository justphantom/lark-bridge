package opencodeservebridge

import (
	"context"
	"fmt"
	"strconv"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
)

// cmdSessionUse switches the chat's binding to another session of the same
// working directory. Forms:
//   - /session-use      → pop a selection card of the directory's sessions
//   - /session-use <n>  → switch directly to the n-th session of the sorted
//     list (1-based, same numbering as /session-list)
//
// Like /session-list the guard uses Lookup (not ensureBinding) so the
// command never creates a binding as a side effect.
func (h *Handler) cmdSessionUse(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok || b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 `/cd` 选择一个项目目录后再切换会话。"}, nil
	}

	if len(args) == 0 {
		return h.runSessionPicker(chatID), nil
	}

	n, err := strconv.Atoi(args[0])
	if err != nil {
		return commandResult{Body: fmt.Sprintf("会话序号必须是数字：%q", args[0])}, nil
	}
	sessions, err := h.agent.ListSessions(ctx, b.Directory)
	if err != nil {
		return commandResult{Body: fmt.Sprintf("获取会话列表失败：%v", err)}, err
	}
	sorted := sortedSessions(sessions)
	if len(sorted) == 0 {
		return commandResult{Body: "当前目录下没有任何会话。"}, nil
	}
	if n < 1 || n > len(sorted) {
		return commandResult{Body: fmt.Sprintf("会话序号 %d 越界，有效范围 1-%d。", n, len(sorted))}, nil
	}
	body, _, err := h.switchSession(ctx, chatID, sorted[n-1])
	return commandResult{Body: body}, err
}

// runSessionPicker drives the interactive session selection in a background
// goroutine, mirroring runModelPicker/runCleanConfirm: AskAndWait blocks far
// beyond the dispatcher's command timeout, so the command returns
// immediately with a placeholder Notice (Handled=true) and the goroutine
// emits the selection card and the result card update itself. The binding is
// re-read inside the goroutine so a /cd issued after the placeholder does
// not switch into a stale directory's session.
func (h *Handler) runSessionPicker(chatID string) commandResult {
	h.emitNoticeLogged(chatID, "info", "正在加载会话列表", "正在获取当前目录的会话，请稍候…")
	bridgebase.GoSafe(h.Logger, "session-use:"+chatID, func() {
		b, ok := h.Router.Lookup(chatID)
		if !ok || b.Directory == "" {
			h.emitNoticeLogged(chatID, "error", "切换失败", "尚未设置工作目录。")
			return
		}
		sessions, err := h.agent.ListSessions(h.AppCtx, b.Directory)
		if err != nil {
			h.emitNoticeLogged(chatID, "error", "切换失败", fmt.Sprintf("获取会话列表失败：%v", err))
			return
		}
		sorted := sortedSessions(sessions)
		if len(sorted) == 0 {
			h.emitNoticeLogged(chatID, "info", "无会话", "当前目录下没有任何会话。")
			return
		}
		statuses, err := h.agent.SessionStatuses(h.AppCtx)
		if err != nil {
			h.emitNoticeLogged(chatID, "error", "切换失败", fmt.Sprintf("获取会话状态失败：%v", err))
			return
		}

		// Numbering follows the full sorted list (same as /session-list);
		// busy sessions keep their number but are not switchable options.
		// The number prefix also keeps every label unique, so the chosen
		// label maps back to exactly one session.
		var options []string
		candidates := make(map[string]oc.SessionInfo, len(sorted))
		busy := 0
		for i, sess := range sorted {
			if st, ok := statuses[sess.ID]; ok && st.Type == "busy" {
				busy++
				continue
			}
			label := fmt.Sprintf("%d. %s · %s", i+1, sessTitle(sess.Title), formatTime(sess.Time.Updated))
			if sess.ID == b.SessionID {
				label = "→ " + label
			}
			options = append(options, label)
			candidates[label] = sess
		}
		if len(options) == 0 {
			h.emitNoticeLogged(chatID, "info", "无会话可切换", fmt.Sprintf("%d 个会话正在执行，不可切换。", busy))
			return
		}

		label := "选择要切换的会话"
		if busy > 0 {
			label = fmt.Sprintf("%s（%d 个会话正在执行，不可切换）", label, busy)
		}
		choice, messageID, err := h.AskAndWait(chatID, "", "会话", label, bridgebase.StaticOptions(options), false)
		if err != nil {
			h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
			return
		}
		sess, ok := candidates[choice]
		if !ok {
			h.emitNoticeLogged(chatID, "error", "切换失败", "选项已失效，请重新发起 /session-use。")
			return
		}

		body, switched, err := h.switchSession(h.AppCtx, chatID, sess)
		if err != nil {
			h.emitNoticeLogged(chatID, "error", "切换失败", err.Error())
			return
		}
		level, title := "info", "会话未切换"
		if switched {
			level, title = "success", "已切换会话"
		}
		h.emitCardUpdateLogged(chatID, messageID, level, title, body)
	})
	return commandResult{Handled: true}
}

// switchSession applies the guarded switch to sess, shared by the direct and
// picker paths. The order is fixed:
//  1. already bound to sess → no-op reply, binding untouched;
//  2. SessionStatuses re-check → a busy session is refused (it may have
//     started running after the picker listed it);
//  3. AbortChat cancels any in-flight turn so the old session is not
//     resumed mid-turn, then the binding is repointed.
//
// switched reports whether step 3 ran, so the picker can pick a card level.
func (h *Handler) switchSession(ctx context.Context, chatID string, sess oc.SessionInfo) (body string, switched bool, err error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok || b.Directory == "" {
		return "尚未设置工作目录。发送 `/cd` 选择一个项目目录后再切换会话。", false, nil
	}
	if b.SessionID == sess.ID {
		return fmt.Sprintf("已是当前会话「%s」。", sessTitle(sess.Title)), false, nil
	}
	statuses, err := h.agent.SessionStatuses(ctx)
	if err != nil {
		return "", false, fmt.Errorf("获取会话状态失败：%w", err)
	}
	if st, ok := statuses[sess.ID]; ok && st.Type == "busy" {
		return fmt.Sprintf("会话「%s」正在执行，暂时不可切换。", sessTitle(sess.Title)), false, nil
	}
	h.AbortChat(chatID)
	h.Router.SetSessionID(chatID, sess.ID)
	h.Logger.Info("session switched", log.FieldChatID, chatID, log.FieldSessionID, sess.ID)
	return fmt.Sprintf("已切换到会话「%s」。旧会话保留，可用 /session-use 切回（/session-clean 会清理未绑定空闲会话）。",
		sessTitle(sess.Title)), true, nil
}
