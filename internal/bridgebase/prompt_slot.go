package bridgebase

import (
	"context"
	"time"
)

// startPrompt/endPrompt/abortChat manage the per-chat prompt slot derived
// from Core's CancelByChat map. They lived as byte-identical copies in both
// bridges' handler_prompt.go; lifted here so each bridge's Handler gets them
// for free via the embedded *Core.

// StartPrompt reserves the per-chat prompt slot derived from AppCtx. Returns
// (ctx, mine, ok=false) when the chat already has an in-flight prompt
// (busy-then-drop). On success the caller owns the slot until EndPrompt.
func (c *Core) StartPrompt(_ context.Context, chatID string) (ctx context.Context, mine *PromptCancel, ok bool) {
	c.CancelMu.Lock()
	defer c.CancelMu.Unlock()
	if _, busy := c.CancelByChat[chatID]; busy {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(c.AppCtx)
	mine = &PromptCancel{
		Cancel:    cancel,
		StartTime: time.Now(),
		ChatID:    chatID,
	}
	c.CancelByChat[chatID] = mine
	return ctx, mine, true
}

// EndPrompt releases the per-chat slot only if it still points at mine (a
// later /session-del + new prompt may have replaced it). nil mine is a no-op.
func (c *Core) EndPrompt(chatID string, mine *PromptCancel) {
	if mine == nil {
		return
	}
	c.CancelMu.Lock()
	defer c.CancelMu.Unlock()
	if cur, ok := c.CancelByChat[chatID]; ok && cur == mine {
		delete(c.CancelByChat, chatID)
	}
}

// AbortChat cancels the in-flight prompt for chatID, if any. Returns whether
// a prompt was running.
func (c *Core) AbortChat(chatID string) bool {
	c.CancelMu.Lock()
	defer c.CancelMu.Unlock()
	if pc, ok := c.CancelByChat[chatID]; ok {
		pc.Cancel()
		return true
	}
	return false
}
