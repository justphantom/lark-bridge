package claudebridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// cmdPull runs `git pull --ff-only` in the chat's bound working directory.
// --ff-only refuses to create a merge commit on divergence, leaving the
// tree clean instead of dropping the user into a conflicted state.
func (h *Handler) cmdPull(_ context.Context, chatID string, _ []string) (commandResult, error) {
	return h.runGit(chatID, []string{"pull", "--ff-only"}, "拉取")
}

// cmdPush runs `git push` in the chat's bound working directory.
func (h *Handler) cmdPush(_ context.Context, chatID string, _ []string) (commandResult, error) {
	return h.runGit(chatID, []string{"push"}, "推送")
}

// runGit resolves the chat's bound directory and hands the job to Core.Git.
// The runner owns per-chat single-flight and async execution; this handler
// only validates a directory is pinned and wires the terminal-notice emit
// path. Returns Handled=true so the slash-command dispatcher does not also
// emit a notice (the runner emits "已触发" inline and the terminal notice
// on completion).
func (h *Handler) runGit(chatID string, args []string, label string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	if b.Directory == "" {
		return cmdutil.ErrorResult("尚未设置工作目录。发送 `/cd` 选择一个项目目录后再执行 git 操作。")
	}
	h.Git.AcquireAndRun(chatID, b.Directory, args, label, func(level, title, body string) {
		h.EmitNoticeLogged(chatID, level, title, body)
	})
	return commandResult{Handled: true}, nil
}
