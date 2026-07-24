package opencodeservebridge

import (
	"context"
	"fmt"
	"strings"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
)

// cmdDeleteIdleSessions deletes all sessions that are both unbound (no chatID
// maps to them) and idle (not busy). Deletion is destructive, so candidates
// are collected first and a confirmation card is popped; only "确认清理"
// runs the deletion. With no candidates it replies with plain text and pops
// no card.
func (h *Handler) cmdDeleteIdleSessions(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	candidates, skippedBusy, err := h.collectIdleSessions(ctx)
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	if len(candidates) == 0 {
		return commandResult{Body: "没有可清理的未绑定空闲会话。"}, nil
	}
	return h.runCleanConfirm(ctx, chatID, candidates, skippedBusy), nil
}

// collectIdleSessions gathers the deletion candidates: unbound, idle
// sessions merged across every bound directory. The serve server scopes
// sessions by project directory, so the listing iterates every bound
// directory and merges by session ID; sessions under directories no chat is
// bound to are out of scope (serve has no global list API). Pure query: it
// never deletes. The second return value carries IDs skipped because busy,
// for the final result message.
func (h *Handler) collectIdleSessions(ctx context.Context) ([]oc.SessionInfo, []string, error) {
	bindings := h.Router.AllBindings()

	boundSessionIDs := make(map[string]struct{}, len(bindings))
	dirs := make(map[string]struct{}, len(bindings))
	for _, b := range bindings {
		if b.SessionID != "" {
			boundSessionIDs[b.SessionID] = struct{}{}
		}
		if b.Directory != "" {
			dirs[b.Directory] = struct{}{}
		}
	}

	seen := make(map[string]struct{})
	var unbound []oc.SessionInfo
	for dir := range dirs {
		list, err := h.agent.ListSessions(ctx, dir)
		if err != nil {
			return nil, nil, fmt.Errorf("列出会话失败：%w", err)
		}
		for _, sess := range list {
			if _, dup := seen[sess.ID]; dup {
				continue
			}
			seen[sess.ID] = struct{}{}
			if _, bound := boundSessionIDs[sess.ID]; bound {
				continue
			}
			unbound = append(unbound, sess)
		}
	}

	statuses, err := h.agent.SessionStatuses(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("获取会话状态失败：%w", err)
	}

	var idle []oc.SessionInfo
	var skippedBusy []string
	for _, sess := range unbound {
		if st, ok := statuses[sess.ID]; ok && st.Type == "busy" {
			skippedBusy = append(skippedBusy, sess.ID)
			continue
		}
		idle = append(idle, sess)
	}
	return idle, skippedBusy, nil
}

// runCleanConfirm pops the confirmation card and runs the deletion in the
// background. AskAndWait blocks up to AskWaitTimeout for a human answer, far
// beyond the dispatcher's command timeout, so (like runModelPicker) it
// returns Handled immediately; the picker Question (TakeOverProgress) morphs
// the command's progress card, and the result patches that same card via
// UpdateMessageID. replyToID keeps the whole flow on one card; a wait error
// binds back to the progress card via emitPromptNotice so no standalone card
// is left dangling.
func (h *Handler) runCleanConfirm(ctx context.Context, chatID string, candidates []oc.SessionInfo, skippedBusy []string) commandResult {
	replyToID := bridgebase.ReplyToID(ctx)
	bridgebase.GoSafe(h.Logger, "session-clean:"+chatID, func() {
		choice, messageID, err := h.AskAndWait(chatID, replyToID, "清理", cleanConfirmLabel(candidates),
			bridgebase.StaticOptions([]string{"确认清理", "取消"}), false)
		if err != nil {
			h.emitPromptNotice(chatID, replyToID, "info", "清理未执行", err.Error())
			return
		}
		if choice != "确认清理" {
			h.emitCardUpdateLogged(chatID, messageID, "info", "已取消清理", "未删除任何会话。")
			return
		}

		// DeleteSessionIfIdle re-checks busy server-side at delete time, so a
		// session that started working after the listing is still spared.
		var deleted []string
		for _, sess := range candidates {
			if err := h.agent.DeleteSessionIfIdle(h.AppCtx, sess.ID); err != nil {
				h.Logger.Warn("failed to delete idle session",
					log.FieldSessionID, sess.ID,
					log.FieldError, err)
				continue
			}
			deleted = append(deleted, sess.ID)
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "已删除 %d 个未绑定且空闲的会话。\n", len(deleted))
		if len(skippedBusy) > 0 {
			fmt.Fprintf(&sb, "跳过 %d 个未绑定但正在执行的会话。\n", len(skippedBusy))
		}
		if len(deleted) > 0 {
			sb.WriteString("\n已删除的会话ID：\n")
			for _, id := range deleted {
				fmt.Fprintf(&sb, "  %s\n", id)
			}
		}

		h.Logger.Info("deleted idle sessions",
			log.FieldChatID, chatID,
			"deleted_count", len(deleted),
			"skipped_busy_count", len(skippedBusy))
		h.emitCardUpdateLogged(chatID, messageID, "success", "已清理空闲会话", sb.String())
	})
	return commandResult{Handled: true}
}

// cleanConfirmLabel renders the confirmation card's label: the total count
// plus the first maxShown sessions (title + ID, mirroring /session-list
// fields). Longer lists are truncated so the card stays small.
func cleanConfirmLabel(candidates []oc.SessionInfo) string {
	const maxShown = 10
	shown := candidates
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "将删除以下 %d 个未绑定且空闲的会话：\n", len(candidates))
	for _, sess := range shown {
		fmt.Fprintf(&sb, "%s（`%s`）\n", sessTitle(sess.Title), sess.ID)
	}
	if len(candidates) > maxShown {
		fmt.Fprintf(&sb, "…等 %d 个", len(candidates))
	}
	return sb.String()
}
