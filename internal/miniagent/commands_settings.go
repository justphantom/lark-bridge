package miniagent

import (
	"context"
	"fmt"
	"os"
	"strconv"
)

// settableModes is the set of -mode values /mode accepts (v3 default|auto).
var settableModes = map[string]struct{}{
	"default": {},
	"auto":    {},
}

// settableThinkingLevels is the set of -thinking values /thinking accepts.
var settableThinkingLevels = map[string]struct{}{
	"off": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// cmdMode pins/clears/shows the per-chat permission mode (-mode):
//
//	/mode              → show current effective mode
//	/mode clear        → clear pin (fall back to global default)
//	/mode default|auto → pin for this chat
func (h *Handler) cmdMode(_ context.Context, chatID, arg string) (level, title, body string) {
	if arg == "" {
		return "info", "当前权限模式", fmt.Sprintf("权限模式：%s\n可选：default | auto", h.activeMode(chatID))
	}
	if arg == "clear" {
		h.ensureBinding(chatID)
		h.router.SetMode(chatID, "")
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义权限模式，将使用全局默认 %s。", h.clientDefaultMode())
	}
	if _, ok := settableModes[arg]; !ok {
		return "error", "权限模式", "可选 default | auto，收到 " + arg
	}
	h.ensureBinding(chatID)
	h.router.SetMode(chatID, arg)
	return "success", "已切换权限模式", "已切换到 " + arg + "（下次提问生效）。"
}

// cmdThinking pins/clears/shows the per-chat reasoning effort (-thinking):
//
//	/thinking        → show current effective level
//	/thinking clear  → clear pin (fall back to global default)
//	/thinking <lvl>  → pin (off|minimal|low|medium|high|xhigh|max)
func (h *Handler) cmdThinking(_ context.Context, chatID, arg string) (level, title, body string) {
	if arg == "" {
		return "info", "当前思考级别", fmt.Sprintf("思考级别：%s\n可选：off | minimal | low | medium | high | xhigh | max", h.activeThinking(chatID))
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

// cmdNew deletes this chat's session jsonl so the next prompt starts a fresh
// conversation (R2). It runs inside the per-chat turn slot (handleSessionCommand
// → startTurn), so it cannot race an in-flight runTurn on the same chat — the
// busy-then-drop that serialises prompts also guards /new. A missing file is
// a no-op (first prompt ever, or already cleared); sessionRoot unset (stateDir
// empty, e.g. some tests) → warning, since there is nothing to forget.
func (h *Handler) cmdNew(_ context.Context, chatID, _ string) (level, title, body string) {
	p := h.sessionPath(chatID)
	if p == "" {
		return "warning", "清除会话", "会话目录未配置，无历史可清。"
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return "error", "清除会话", fmt.Sprintf("删除会话文件失败：%v", err)
	}
	return "success", "已清除会话", "本轮对话历史已清空，下次提问开始新会话。"
}
