package ompbridge

import (
	"context"
	"testing"

	"github.com/justphantom/lark-bridge/internal/omp"
)

// TestStreamRun_IdleVsCancel asserts streamRun distinguishes an idle-timeout
// cancellation (context.Cause == errIdleTimeout → IsIdleTimeout) from any other
// cancellation (user abort / prompt timeout → IsCancelled). The two share the
// exact same ctx.Err() path; only context.Cause separates them, and the
// terminal control the user sees depends on it: idle surfaces a "响应超时"
// notice while a user cancel surfaces "已取消". This mirrors opencodebridge's
// idle-vs-cancel contract (opencode/stream_loop_test.go) which OMP lacked.
//
// The events channel is pre-closed so the for-range exits without consuming an
// event and the post-loop ctx.Err() branch (the same one the live idle/cancel
// unwind hits) decides the PromptResult flags.
func TestStreamRun_IdleVsCancel(t *testing.T) {
	h, _ := newTestHandler(t)

	cases := []struct {
		name     string
		cause    error
		wantIdle bool
		wantCanc bool
	}{
		{"idle timeout", errIdleTimeout, true, false},
		{"user cancel", context.Canceled, false, true},
		{"prompt timeout", context.DeadlineExceeded, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(tc.cause)

			events := make(chan omp.Event)
			close(events)

			r := h.streamRun(ctx, "chat-1", "reply-1", events, "", nil)
			if r.IsIdleTimeout != tc.wantIdle {
				t.Errorf("IsIdleTimeout = %v, want %v", r.IsIdleTimeout, tc.wantIdle)
			}
			if r.IsCancelled != tc.wantCanc {
				t.Errorf("IsCancelled = %v, want %v", r.IsCancelled, tc.wantCanc)
			}
			if r.Err == nil {
				t.Error("PromptResult.Err = nil, want the cancelled ctx error")
			}
		})
	}
}
