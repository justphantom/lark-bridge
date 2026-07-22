package opencodeservebridge

import (
	"context"
	"fmt"
	"strings"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/log"
)

// cmdDeleteIdleSessions deletes all sessions that are both unbound (no chatID
// maps to them) and idle (not busy). The serve server scopes sessions by
// project directory, so the listing iterates every bound directory and merges
// by session ID; sessions under directories no chat is bound to are out of
// scope (serve has no global list API).
func (h *Handler) cmdDeleteIdleSessions(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	// Get all bindings to determine which sessions are bound.
	bindings := h.Router.AllBindings()

	// Build a set of bound session IDs and the deduped directory set.
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

	// List sessions per bound directory and merge by session ID.
	seen := make(map[string]struct{})
	var sessions []oc.SessionInfo
	for dir := range dirs {
		list, err := h.agent.ListSessions(ctx, dir)
		if err != nil {
			return commandResult{Body: fmt.Sprintf("列出会话失败：%v", err)}, err
		}
		for _, sess := range list {
			if _, dup := seen[sess.ID]; dup {
				continue
			}
			seen[sess.ID] = struct{}{}
			sessions = append(sessions, sess)
		}
	}

	// Get session statuses to check which are busy.
	statuses, err := h.agent.SessionStatuses(ctx)
	if err != nil {
		return commandResult{Body: fmt.Sprintf("获取会话状态失败：%v", err)}, err
	}

	// Find and delete unbound, idle sessions.
	var deleted []string
	var skippedBusy []string
	for _, sess := range sessions {
		// Skip if this session is bound to a chat.
		if _, bound := boundSessionIDs[sess.ID]; bound {
			continue
		}

		// Skip if this session is busy.
		if st, ok := statuses[sess.ID]; ok && st.Type == "busy" {
			skippedBusy = append(skippedBusy, sess.ID)
			continue
		}

		// Try to delete the session (only if idle).
		if err := h.agent.DeleteSessionIfIdle(ctx, sess.ID); err != nil {
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

	return commandResult{Body: sb.String()}, nil
}
