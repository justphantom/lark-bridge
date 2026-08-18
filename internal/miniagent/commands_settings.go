package miniagent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
)

// settableThinkingLevels is the set of -thinking values /effort accepts.
var settableThinkingLevels = map[string]struct{}{
	"off": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// effortOptions is the sorted list of selectable -thinking values offered by
// the /effort picker. Built once from settableThinkingLevels so the map stays
// the single source of truth for both validation and the picker card.
var effortOptions = func() []string {
	opts := make([]string, 0, len(settableThinkingLevels))
	for l := range settableThinkingLevels {
		opts = append(opts, l)
	}
	sort.Strings(opts)
	return opts
}()

// cmdEffort pins/clears/selects the per-chat reasoning effort (-thinking):
//
//	/effort           → interactive picker (off|minimal|low|medium|high|xhigh|max)
//	/effort clear     → clear pin (fall back to global default)
//	/effort <lvl>     → pin for this chat
//
// The picker reuses the command's progress card (see cmdModel): promptID +
// TakeOverProgress morph it into the picker, and the result patches the same
// card via UpdateMessageID.
func (h *Handler) cmdEffort(_ context.Context, chatID, arg string) (level, title, body string) {
	if arg == "" {
		// Interactive picker: askAndWait blocks for a human click; run off
		// the turn goroutine like /model and /cd.
		promptID := h.PromptIDForPickers(chatID)
		go func() { //nolint:gosec // G118: picker outlives the request ctx
			choice, messageID, err := h.askAndWait(context.Background(), chatID, promptID, "思考级别", effortOptions)
			if err != nil {
				h.notifyWithPromptID(chatID, promptID, "warning", "选择失败", err.Error())
				return
			}
			h.ensureBinding(chatID)
			h.router.SetThinking(chatID, choice)
			h.notifyWithCardUpdate(chatID, messageID, "success", "已切换思考级别", "已切换到 "+choice+"（下次提问生效）。")
		}()
		return "async", "", "" // sentinel: handleSessionCommand must not notify
	}
	if arg == "clear" {
		h.ensureBinding(chatID)
		h.router.SetThinking(chatID, "")
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义思考级别，将使用全局默认 %s。", h.clientDefaultThinking())
	}
	if _, ok := settableThinkingLevels[arg]; !ok {
		return "error", "思考级别", "可选 off | minimal | low | medium | high | xhigh | max，收到 " + arg
	}
	h.ensureBinding(chatID)
	h.router.SetThinking(chatID, arg)
	return "success", "已切换思考级别", "已切换到 " + arg + "（下次提问生效）。"
}

// cmdMaxIter pins/clears/shows the per-chat LLM-call cap (-max-iterations):
//
//	/maxiter          → show current effective cap
//	/maxiter clear    → clear pin (fall back to global default)
//	/maxiter <N>      → pin N (N >= 1, applies next prompt)
//
// <1 is rejected (use "clear" to reset) so the zero value stays unambiguous as
// "unset". ensureBinding runs only on the success paths, mirroring cmdMode, so a
// rejected value never creates a binding.
func (h *Handler) cmdMaxIter(_ context.Context, chatID, arg string) (level, title, body string) {
	if arg == "" {
		return "info", "当前迭代上限", "迭代上限：" + h.formatMaxIter(h.activeMaxIter(chatID))
	}
	if arg == "clear" {
		h.ensureBinding(chatID)
		h.router.SetMaxIterations(chatID, 0)
		return "success", "已恢复默认", "已清除自定义迭代上限，将使用全局默认 " + h.formatMaxIter(h.clientDefaultMaxIter()) + "。"
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 {
		return "error", "迭代上限", "需要正整数（≥1），或 clear 恢复默认；收到 " + arg
	}
	h.ensureBinding(chatID)
	h.router.SetMaxIterations(chatID, n)
	return "success", "已设置迭代上限", fmt.Sprintf("已设置为 %d（下次提问生效）。", n)
}

// formatMaxIter renders an iteration cap for display. 0 means "no flag passed"
// (the upstream CLI default of 20).
func (h *Handler) formatMaxIter(n int) string {
	if n > 0 {
		return strconv.Itoa(n)
	}
	return "默认（上游 CLI，约 20）"
}

// cmdNew deletes this chat's session-id mapping so the next prompt starts a
// fresh conversation (R2): the bridge forgets the chatID→id mapping, so the
// next turn passes -save-session and miniagent creates a new session. It runs
// inside the per-chat turn slot (handleSessionCommand → startTurn), so it
// cannot race an in-flight runTurn on the same chat — the busy-then-drop that
// serialises prompts also guards /new. A missing file is a no-op (first prompt
// ever, or already cleared); sessionRoot unset (stateDir empty, e.g. some
// tests) → warning, since there is nothing to forget.
//
// NOTE: the miniagent session jsonl under miniagent's own session.dir is NOT
// deleted here — the bridge does not configure session.dir and does not own
// those files. It becomes an orphan (harmless; miniagent manages its session
// store). Forgetting the mapping is enough to start fresh.
func (h *Handler) cmdNew(_ context.Context, chatID, _ string) (level, title, body string) {
	p := h.sessionIDFile(chatID)
	if p == "" {
		return "warning", "清除会话", "会话目录未配置，无历史可清。"
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return "error", "清除会话", fmt.Sprintf("删除会话映射失败：%v", err)
	}
	return "success", "已清除会话", "本轮对话历史已清空，下次提问开始新会话。"
}
