package bridgebase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

const (
	// AskWaitTimeout bounds how long a picker waits for a human answer before
	// the request is cancelled. Interactive cards on the frontend expire at
	// cardkit.InteractiveTimeout, so this only needs to outlast that window.
	AskWaitTimeout = 9 * time.Minute

	// emitNoticeTimeout bounds each Notice/Question emit a picker makes on its
	// own (outside the dispatcher). Long enough to ride out a transient IPC
	// blip, short enough that a stuck POST does not pin a goroutine.
	emitNoticeTimeout = 10 * time.Second

	// listFnTimeout bounds a backend's models/agent list subcommand invoked by
	// the picker. CLI startups are heavy: a provider/config load can take tens
	// of seconds, and a models catalog fetch over the network has been
	// measured at minutes. 300s covers the worst observed case with headroom
	// while still bounding a genuinely hung fork.
	listFnTimeout = 300 * time.Second

	// maxQuestionOptions caps the number of options shown in a single picker
	// card. Feishu rejects oversized cards with ErrCode 11310 "element
	// exceeds the limit" (observed ~270-entry renderer budget; 100 stays
	// well under with room for future renderer changes). Truncates in list
	// order; callers should surface common items first or expose a custom
	// input box (allowCustom=true) so omitted entries remain reachable.
	maxQuestionOptions = 100
)

// EmitFunc matches the bridges' Handler.emit signature: promptID scopes the
// control to an in-flight turn (empty for a standalone picker card).
type EmitFunc func(ctx context.Context, promptID string, ctrl *protocol.Control) error

// ErrPickerInFlight is returned by AskAndWait when a picker is already
// open in the same chat. The caller surfaces err.Error() as a notice, so the
// message is user-facing.
var ErrPickerInFlight = errors.New("本群已有一个选择进行中，请先完成或等待其失效")

// StaticOptions adapts a fixed option list to AskAndWait's listFn form, for
// backends whose picker values come from static config rather than a CLI
// subcommand.
func StaticOptions(options []string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return options, nil }
}

// AskAndWait runs the full interactive-selection loop for a setting picker:
// lists the available options via listFn, emits a Question card offering them
// (plus a custom-input box so a value not listed can still be typed), then
// blocks until the user answers or AskWaitTimeout elapses.
//
// Both the listFn call and the answer wait derive their ctx from appCtx, NOT
// from any caller ctx: a backend CLI may take tens of seconds to list models,
// far exceeding the dispatcher's cmdutil.Timeout (15s). Callers MUST run this
// in a background goroutine (the dispatcher returns immediately with a
// placeholder Notice and Handled=true). chatID satisfies protocol.Validate
// (Question controls require ChatID). kind/label tailor the card copy.
//
// Returns the chosen value (custom input takes priority over a listed pick),
// the Feishu message ID of the question card that was answered, and an error
// describing why no answer was obtained.
func AskAndWait(
	appCtx context.Context,
	answers *AnswerBroker,
	emit EmitFunc,
	chatID, replyToID, kind, label string,
	listFn func(context.Context) ([]string, error),
	allowCustom bool,
) (string, string, error) {
	// listTimeoutCtx bounds the (slow) list subcommand independently of any
	// caller deadline. It rides the process-lifetime appCtx so a shutdown
	// still cancels an in-flight fork.
	listCtx, listCancel := context.WithTimeout(appCtx, listFnTimeout)
	defer listCancel()
	options, err := listFn(listCtx)
	if err != nil {
		return "", "", fmt.Errorf("获取%s列表失败：%w", kind, err)
	}
	if len(options) == 0 {
		return "", "", fmt.Errorf("没有可用的%s", kind)
	}
	if len(options) > maxQuestionOptions {
		options = options[:maxQuestionOptions]
	}

	requestID, err := newRequestID()
	if err != nil {
		return "", "", fmt.Errorf("生成请求 ID 失败：%w", err)
	}
	ch, ok := answers.Register(requestID)
	if !ok {
		return "", "", fmt.Errorf("已有一个进行中的选择，请先完成或等待其失效")
	}

	q := &protocol.Control{
		Type:   protocol.TypeQuestion,
		ChatID: chatID,
		Question: &protocol.QuestionPayload{
			RequestID: requestID,
			Questions: []protocol.QuestionItem{{
				Label:   label,
				Options: options,
				Custom:  allowCustom,
			}},
			// AskAndWait serves only setting pickers, never mid-turn
			// permission gates, so every card it emits is a takeover
			// candidate. The takeover still requires replyToID to match an
			// open turn on the frontend; callers passing "" get a standalone
			// card exactly as before.
			TakeOverProgress: true,
		},
	}
	emitCtx, emitCancel := context.WithTimeout(appCtx, emitNoticeTimeout)
	defer emitCancel()
	if err := emit(emitCtx, replyToID, q); err != nil {
		answers.Cancel(requestID)
		return "", "", fmt.Errorf("发送选择卡片失败：%w", err)
	}

	// Waiting for a human answer is unbounded in practice; derive a fresh
	// deadline from the process-lifetime appCtx.
	waitCtx, waitCancel := context.WithTimeout(appCtx, AskWaitTimeout)
	defer waitCancel()

	select {
	case ans, ok := <-ch:
		if !ok {
			// Channel closed by Drain during shutdown.
			return "", "", errors.New("服务正在关闭，请稍后重试")
		}
		choice := PickAnswerValue(ans)
		if choice == "" {
			return "", "", fmt.Errorf("未选择任何%s", kind)
		}
		messageID := ""
		if ans != nil {
			messageID = ans.MessageID
		}
		return choice, messageID, nil
	case <-waitCtx.Done():
		answers.Cancel(requestID)
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return "", "", fmt.Errorf("选择超时（>%s），请重新发起", AskWaitTimeout)
		}
		return "", "", errors.New("等待选择被中断")
	}
}

// PickAnswerValue extracts the user's selection from an AnswerPayload. A
// custom-typed value wins over a listed pick (the user explicitly overrode
// the list); the Choices slice carries a single-select's value at index 0.
func PickAnswerValue(ans *protocol.AnswerPayload) string {
	if ans == nil {
		return ""
	}
	if ans.Custom != "" {
		return ans.Custom
	}
	if len(ans.Choices) > 0 {
		return ans.Choices[0]
	}
	return ""
}

// newRequestID returns a random hex string for an interactive card's
// requestID. crypto/rand keeps it unguessable so a stale card click after a
// timeout cannot collide with a fresh picker.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "q-" + hex.EncodeToString(b[:]), nil
}

// EmitNotice sends a Notice control on the picker's own lifecycle. Interactive
// pickers return Handled=true and bypass the dispatcher, so they cannot reuse
// the dispatcher's ctx (which expired during the wait). Deriving a fresh ctx
// from appCtx lets a confirmation or error Notice land after a multi-minute
// wait.
func EmitNotice(appCtx context.Context, emit EmitFunc, chatID, level, title, body string, extra ...string) error {
	return EmitCardUpdate(appCtx, emit, chatID, "", level, title, body, extra...)
}

// EmitCardUpdate sends a Notice control that patches an existing card
// (identified by updateMessageID) instead of posting a new standalone notice.
// An empty updateMessageID falls back to sending a new card, matching
// EmitNotice behaviour.
func EmitCardUpdate(appCtx context.Context, emit EmitFunc, chatID, updateMessageID, level, title, body string, extra ...string) error {
	ctx, cancel := context.WithTimeout(appCtx, emitNoticeTimeout)
	defer cancel()
	np := &protocol.NoticePayload{
		Level:           level,
		Title:           title,
		Message:         body,
		UpdateMessageID: updateMessageID,
	}
	// extra carries optional Field/Before/After in that order, matching the
	// ChangeResult shape the renderer expects for a before→after block.
	if len(extra) > 0 {
		np.Field = extra[0]
	}
	if len(extra) > 1 {
		np.Before = extra[1]
	}
	if len(extra) > 2 {
		np.After = extra[2]
	}
	return emit(ctx, "", &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: np,
	})
}

// AskCardUpdate refreshes an existing picker card in place for the next round
// of a multi-round picker (today only /send's directory browser). It is the
// in-place counterpart to AskAndWait: instead of morphing the progress card or
// shipping a standalone card, it tells the frontend to PATCH updateMessageID
// (the message_id AskAndWait returned on round 1) with a fresh option list and
// a fresh requestID, then blocks for the answer exactly like AskAndWait.
//
// updateMessageID is what binds the whole browser to one card across rounds;
// passing "" degrades to a standalone card (callers should pass the round-1 id
// they captured). listFn/allowCustom mirror AskAndWait. Runs on appCtx, so the
// caller MUST be on a background goroutine (the dispatcher returns Handled=true
// immediately), exactly like AskAndWait.
func AskCardUpdate(
	appCtx context.Context,
	answers *AnswerBroker,
	emit EmitFunc,
	chatID, updateMessageID, kind, label string,
	listFn func(context.Context) ([]string, error),
	allowCustom bool,
) (string, string, error) {
	listCtx, listCancel := context.WithTimeout(appCtx, listFnTimeout)
	defer listCancel()
	options, err := listFn(listCtx)
	if err != nil {
		return "", "", fmt.Errorf("获取%s列表失败：%w", kind, err)
	}
	if len(options) == 0 {
		return "", "", fmt.Errorf("没有可用的%s", kind)
	}
	if len(options) > maxQuestionOptions {
		options = options[:maxQuestionOptions]
	}

	requestID, err := newRequestID()
	if err != nil {
		return "", "", fmt.Errorf("生成请求 ID 失败：%w", err)
	}
	ch, ok := answers.Register(requestID)
	if !ok {
		return "", "", fmt.Errorf("已有一个进行中的选择，请先完成或等待其失效")
	}

	q := &protocol.Control{
		Type:   protocol.TypeQuestion,
		ChatID: chatID,
		Question: &protocol.QuestionPayload{
			RequestID:       requestID,
			Questions:       []protocol.QuestionItem{{Label: label, Options: options, Custom: allowCustom}},
			UpdateMessageID: updateMessageID,
		},
	}
	emitCtx, emitCancel := context.WithTimeout(appCtx, emitNoticeTimeout)
	defer emitCancel()
	if err := emit(emitCtx, "", q); err != nil {
		answers.Cancel(requestID)
		return "", "", fmt.Errorf("刷新选择卡片失败：%w", err)
	}

	waitCtx, waitCancel := context.WithTimeout(appCtx, AskWaitTimeout)
	defer waitCancel()
	select {
	case ans, ok := <-ch:
		if !ok {
			return "", "", errors.New("服务正在关闭，请稍后重试")
		}
		choice := PickAnswerValue(ans)
		if choice == "" {
			return "", "", fmt.Errorf("未选择任何%s", kind)
		}
		// The card patched in place is updateMessageID; an answer carries the
		// message_id the user actually clicked, which is the same card. Prefer
		// the carried id when present (it survives a frontend fallback to a
		// fresh standalone card) and fall back to updateMessageID otherwise.
		messageID := updateMessageID
		if ans != nil && ans.MessageID != "" {
			messageID = ans.MessageID
		}
		return choice, messageID, nil
	case <-waitCtx.Done():
		answers.Cancel(requestID)
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return "", "", fmt.Errorf("选择超时（>%s），请重新发起", AskWaitTimeout)
		}
		return "", "", errors.New("等待选择被中断")
	}
}
