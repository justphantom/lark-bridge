package miniagent

import (
	"context"
	"fmt"
	"strings"
)

// Session management commands. After the stateless migration the only
// persistent per-chat state is the router binding (Directory + ModelSpec);
// every command below reads/writes it via h.Router. There is no session,
// memory, or permission concept anymore.
//
// Each command returns the Notice level/title/body the dispatcher emits.
// "async" as level is a sentinel meaning the command has already emitted its
// own controls (picker card) and the dispatcher must not emit a Notice.

// sessionCmds is the single source of truth for command names → handlers.
// Adding a command means adding one entry here (and the method); isSession
// recognition and dispatch both read this table.
//
// /running and /session-abort are NOT in this map: they are dispatched
// earlier in HandleEvent (before startTurn) because they must not occupy
// the per-chat turn slot.
var sessionCmds = map[string]func(h *Handler, ctx context.Context, chatID, arg string) (level, title, body string){
	"/current": (*Handler).cmdCurrent,
	"/model":   (*Handler).cmdModel,
	"/models":  (*Handler).cmdModels,
	"/cd":      (*Handler).cmdDirectory,
	"/pull":    (*Handler).cmdPull,
	"/push":    (*Handler).cmdPush,
	"/help":    (*Handler).cmdHelp,
}

// isSessionCommand reports whether prompt is one this handler owns. It never
// panics on a bare "/" — strings.Fields collapses that to nothing.
func isSessionCommand(prompt string) bool {
	if !strings.HasPrefix(prompt, "/") {
		return false
	}
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false
	}
	_, ok := sessionCmds[fields[0]]
	return ok
}

// handleSessionCommand reserves the per-chat turn slot (so a command cannot
// race with an in-flight runTurn over the router binding), runs the command,
// and replies via a Notice. A busy chat gets the same "处理中" notice a
// prompt would.
func (h *Handler) handleSessionCommand(ctx context.Context, chatID, promptID, prompt string) error {
	turnCtx, mine, ok := h.startTurn(ctx, chatID)
	_ = turnCtx
	if !ok {
		h.notifyWithPromptID(chatID, promptID, "warning", "处理中", "上一条消息还在处理，请等它结束后再发。")
		return nil
	}
	defer h.endTurn(chatID, mine)
	h.SetPromptIDForPickers(chatID, promptID)
	defer h.SetPromptIDForPickers(chatID, "")

	fields := strings.Fields(prompt)
	arg := ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	fn := sessionCmds[fields[0]]
	level, title, body := fn(h, ctx, chatID, arg)
	if level == "async" {
		return nil
	}
	h.notifyWithPromptID(chatID, promptID, level, title, body)
	return nil
}

// cmdCurrent reports the per-chat model + directory the next fork will use.
// Falls back to the global defaults when the chat has no pin.
func (h *Handler) cmdCurrent(_ context.Context, chatID, _ string) (level, title, body string) {
	cur := h.activeModel(chatID)
	dir := h.activeDir(chatID)
	return "info", "当前状态", fmt.Sprintf("模型：%s\n工作目录：%s", cur, dir)
}
