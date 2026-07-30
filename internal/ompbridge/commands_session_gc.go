package ompbridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// cmdSessionGC runs `omp gc --apply --archive --json` to reconcile the session
// store and the history.db/FTS index. This is the correct cleanup path after
// /session-clean, which only removes .jsonl files and leaves the index to be
// reconciled here.
func (h *Handler) cmdSessionGC(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok {
		return commandResult{Body: "当前群尚无会话绑定。"}, nil
	}
	if b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 /cd 选择一个项目目录后再执行 GC。"}, nil
	}
	replyToID := bridgebase.ReplyToID(ctx)
	h.EmitAsync(replyToID, &protocol.Control{
		Type:     protocol.TypeProgress,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: "🗑️ 正在运行 omp gc（归档冷会话并同步索引）…"},
	})

	bridgebase.GoSafe(h.Logger, "session-gc:"+chatID, func() {
		res, err := h.agent.RunGC(h.AppCtx, omp.GCOptions{
			AgentDir:             "",
			ColdArchiveAfterDays: h.gcColdArchiveAfterDays,
			RetainNewestPerCwd:   h.gcRetainNewestPerCwd,
			Timeout:              h.gcTimeout,
		})
		if err != nil {
			h.EmitPromptNotice(chatID, replyToID, "error", "GC 失败", "omp gc 执行失败："+err.Error())
			return
		}
		h.Logger.Info("omp gc completed",
			log.FieldChatID, chatID,
			"agent_dir", res.AgentDir,
			"archived", res.Archived,
			"history_rows_deleted", res.HistoryRowsDeleted)
		body := formatGCResult(res)
		h.EmitPromptNotice(chatID, replyToID, "success", "GC 完成", body)
	})
	return commandResult{Handled: true}, nil
}

// formatGCResult renders the aggregate counts from omp gc --json into a
// human-readable summary. A non-empty Errors slice downgrades the tone in the
// caller (we keep it success-level unless there were runtime errors, because
// per-session archive errors are already partial work).
func formatGCResult(r omp.GCResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "扫描 %d 个会话，归档 %d 个。\n", r.Scanned, r.Archived)
	fmt.Fprintf(&sb, "保留最新（全局/目录）：%d / %d\n", r.KeptNewestGlobal, r.KeptNewestPerCwd)
	fmt.Fprintf(&sb, "history.db 删除行数：%d\n", r.HistoryRowsDeleted)
	if r.FTSRebuilt {
		sb.WriteString("FTS 索引：已重建\n")
	}
	if len(r.Errors) > 0 {
		fmt.Fprintf(&sb, "警告项（%d）：%s", len(r.Errors), strings.Join(r.Errors, "; "))
	}
	return strings.TrimSpace(sb.String())
}
