package ompbridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// cmdPull runs `git pull --ff-only` in the chat's bound working directory.
// --ff-only refuses to create a merge commit on divergence, leaving the tree
// clean instead of dropping the user into a conflicted state.
func (h *Handler) cmdPull(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	return h.runGit(chatID, bridgebase.ReplyToID(ctx), []string{"pull", "--ff-only"}, "拉取")
}

// cmdPush runs `git push` in the chat's bound working directory.
func (h *Handler) cmdPush(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	return h.runGit(chatID, bridgebase.ReplyToID(ctx), []string{"push"}, "推送")
}

// runGit resolves the chat's bound directory and delegates the single-flight
// + banner + terminal-notice lifecycle to Core.RunGitJob (shared across
// bridges — the heavy logic lives once in bridgebase). Only the
// bridge-specific ensureBinding stays here.
func (h *Handler) runGit(chatID, replyToID string, args []string, label string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	if b.Directory == "" {
		return cmdutil.ErrorResult("尚未设置工作目录。发送 `/cd` 选择一个项目目录后再执行 git 操作。")
	}
	h.RunGitJob(chatID, replyToID, b.Directory, args, label)
	return commandResult{Handled: true}, nil
}
