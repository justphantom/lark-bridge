package bridgebase

import (
	"github.com/justphantom/lark-bridge/internal/protocol"
)

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
