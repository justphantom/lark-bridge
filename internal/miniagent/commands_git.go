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
// GitRunner. The runner emits "已触发" inline and the terminal notice on
// completion; the "async" sentinel level tells the dispatcher this
// command has handled its own notices and must not emit another.
func (h *Handler) runGit(chatID string, args []string, label string) (level, title, body string) {
	dir := h.activeDir(chatID)
	if dir == "" {
		return "warning", "未设置目录", "尚未配置工作目录（WORKSPACE_ROOT 为空），无法执行 git 操作。"
	}
	// PromptIDForPickers carries the promptID the dispatcher stamped for
	// this chat (see handleSessionCommand) so the terminal notice morphs
	// the in-progress card in place rather than spawning a new one.
	h.git.AcquireAndRun(chatID, dir, args, label, func(level, title, body string) {
		h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), level, title, body)
	})
	return "async", "", ""
}
