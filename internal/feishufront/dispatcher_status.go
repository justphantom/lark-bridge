package feishufront

import (
	"context"
	"runtime/debug"

	"github.com/justphantom/lark-bridge/internal/log"
)

// statusSendSem bounds concurrent status-report broadcasts. sendStatusReport
// does O(bound chats) synchronous Feishu PATCH calls per tick; without a cap
// a slow Feishu API would head-of-line block the serial control pump. Mirrors
// fileSendSem (which bounds TypeFile delivery).
var statusSendSem = make(chan struct{}, 4)

// sendStatusReportAsync runs a TypeStatusReport broadcast off the control
// pump's goroutine. The pump is a single serial loop; sendStatusReport does
// O(chats) synchronous Feishu PATCH calls, so running it inline lets one slow
// API head-of-line block every chat's card updates. Status is periodic,
// unordered, and safely droppable — ideal for async + bounded concurrency.
func (d *Dispatcher) sendStatusReportAsync(ctx context.Context, rc RoutedControl) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if l := d.logger.Load(); l != nil {
					l.Error("panic in status report",
						"backend_id", rc.BackendID,
						log.FieldPanic, r,
						log.FieldStack, string(debug.Stack()))
				}
			}
		}()
		statusSendSem <- struct{}{}
		defer func() { <-statusSendSem }()
		if err := d.sendStatusReport(ctx, rc); err != nil {
			if l := d.logger.Load(); l != nil {
				l.Warn("status report send", "backend_id", rc.BackendID, log.FieldError, err)
			}
		}
	}()
}
