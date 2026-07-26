package bridgebase

import (
	"context"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// This file holds two Core helpers that used to be duplicated across the
// opencode / claude bridges: a promptID-bound notice emit (every picker /
// git command used a byte-identical copy) and the /pull /push spine
// (single-flight + banner + terminal-notice lifecycle). Centralising them
// here keeps the heavy logic in one place; bridges keep only the
// bridge-specific bits (ensureBinding).

// EmitPromptNotice emits a Notice bound to promptID on a fresh 10s context
// derived from AppCtx (NOT the dispatcher's ctx, which expires mid-picker).
// The frontend patches the progress card for that promptID in place and
// finalises the turn, so a picker failure does not leave the "处理中"
// placeholder hanging.
func (c *Core) EmitPromptNotice(chatID, promptID, level, title, body string) {
	ctx, cancel := context.WithTimeout(c.AppCtx, 10*time.Second)
	defer cancel()
	c.EmitLogged(ctx, promptID, chatID, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: body},
	})
}

// RunGitJob is the shared spine of /pull and /push: per-chat single-flight via
// Git.AcquireAndRun, a non-terminal "⏳ <label>执行中…" banner bound to
// replyToID on accept, and the terminal notice (success/error/busy) likewise
// bound so the command's progress card is patched in place. Bridges supply
// the resolved working dir (from their bridge-specific binding) and call this.
func (c *Core) RunGitJob(chatID, replyToID, dir string, args []string, label string) {
	accepted := c.Git.AcquireAndRun(chatID, dir, args, label, func(level, title, body string) {
		c.EmitPromptNotice(chatID, replyToID, level, title, body)
	})
	if accepted {
		c.EmitAsync(replyToID, &protocol.Control{
			Type:     protocol.TypeProgress,
			ChatID:   chatID,
			Progress: &protocol.ProgressPayload{Description: "⏳ " + label + "执行中…"},
		})
	}
}
