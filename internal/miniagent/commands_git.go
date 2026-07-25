package miniagent

import (
	"context"
)

// cmdPull runs `git pull --ff-only` in the chat's active workdir (per-chat
// /cd pin or the global workspace_root fallback). --ff-only refuses to
// create a merge commit on divergence, leaving the tree clean instead of
// dropping the user into a conflicted state.
func (h *Handler) cmdPull(_ context.Context, chatID, _ string) (level, title, body string) {
	return h.runGit(chatID, []string{"pull", "--ff-only"}, "拉取")
}

// cmdPush runs `git push` in the chat's active workdir.
func (h *Handler) cmdPush(_ context.Context, chatID, _ string) (level, title, body string) {
	return h.runGit(chatID, []string{"push"}, "推送")
}

// runGit resolves the chat's active workdir and hands the job to the
// GitRunner. On accept the handler emits a non-terminal progress banner bound
// to the dispatcher-stamped promptID (so the job shows on the command's own
// progress card without a second "triggered" card); the runner's terminal
// callback fires a notice bound to the same promptID, patching that card in
// place. The "async" sentinel level tells the dispatcher this command has
// handled its own notices and must not emit another.
func (h *Handler) runGit(chatID string, args []string, label string) (level, title, body string) {
	dir := h.activeDir(chatID)
	if dir == "" {
		return "warning", "未设置目录", "尚未配置工作目录（WORKSPACE_ROOT 为空），无法执行 git 操作。"
	}
	// PromptIDForPickers carries the promptID the dispatcher stamped for
	// this chat (see handleSessionCommand) so the terminal notice morphs
	// the in-progress card in place rather than spawning a new one.
	promptID := h.PromptIDForPickers(chatID)
	accepted := h.git.AcquireAndRun(chatID, dir, args, label, func(level, title, body string) {
		h.notifyWithPromptID(chatID, promptID, level, title, body)
	})
	if accepted {
		h.notifyProgressWithPromptID(chatID, promptID, "⏳ "+label+"执行中…")
	}
	return "async", "", ""
}
