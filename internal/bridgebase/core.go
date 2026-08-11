package bridgebase

import (
	"context"
	"time"
)

// PromptCancel is the cancel entry of one in-flight prompt, registered under
// its chatID so /abort and Close can cancel exactly one chat's run
// without disturbing others.
type PromptCancel struct {
	Cancel    context.CancelFunc
	StartTime time.Time
	ChatID    string
	PromptID  string // turn identity; used for running-session reports
}
