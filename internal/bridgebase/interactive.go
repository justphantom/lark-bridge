package bridgebase

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// EmitFunc matches the bridges' Handler.emit signature: promptID scopes the
// control to an in-flight turn (empty for a standalone picker card).
type EmitFunc func(ctx context.Context, promptID string, ctrl *protocol.Control) error

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
