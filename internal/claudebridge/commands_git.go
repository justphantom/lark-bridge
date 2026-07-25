package claudebridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// cmdPull runs `git pull --ff-only` in the chat's bound working directory.
// --ff-only refuses to create a merge commit on divergence, leaving the
// tree clean instead of dropping the user into a conflicted state.
func (h *Handler) cmdPull(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	return h.runGit(chatID, bridgebase.ReplyToID(ctx), []string{"pull", "--ff-only"}, "拉取")
}

// cmdPush runs `git push` in the chat's bound working directory.
func (h *Handler) cmdPush(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	return h.runGit(chatID, bridgebase.ReplyToID(ctx), []string{"push"}, "推送")
}

// runGit resolves the chat's bound directory and hands the job to Core.Git.
// The runner owns per-chat single-flight and async execution. On accept the
// handler emits a non-terminal progress banner bound to replyToID (so the job
// is visible on the command's own progress card without spawning a second
// "triggered" card); the runner's terminal callback fires a notice ALSO bound
// to replyToID, so the frontend patches that same card in place and finalises
// the turn. Without the promptID bind the turn would orphan (stuck "处理中"
// card + an inflated /v1/status InFlight that can block deploy.sh).
// Returns Handled=true so the slash-command dispatcher does not emit its own
// notice.
func (h *Handler) runGit(chatID, replyToID string, args []string, label string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	if b.Directory == "" {
		return cmdutil.ErrorResult("尚未设置工作目录。发送 `/cd` 选择一个项目目录后再执行 git 操作。")
	}
	accepted := h.Git.AcquireAndRun(chatID, b.Directory, args, label, func(level, title, body string) {
		h.emitPromptNotice(chatID, replyToID, level, title, body)
	})
	if accepted {
		h.EmitAsync(replyToID, &protocol.Control{
			Type:     protocol.TypeProgress,
			ChatID:   chatID,
			Progress: &protocol.ProgressPayload{Description: "⏳ " + label + "执行中…"},
		})
	}
	return commandResult{Handled: true}, nil
}
