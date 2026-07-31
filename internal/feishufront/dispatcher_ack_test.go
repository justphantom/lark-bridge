package feishufront

import (
	"context"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestDispatchTerminal_SendsACK verifies the terminal-delivery ACK contract:
// after a terminal control (Result/Error/Notice) is dispatched, the frontend
// sends an ACK back to the backend over the SSE event channel so the backend's
// EmitTerminal retry loop stops. The ACK is keyed by PromptID (the backend's
// wait key) and echoes the control type for diagnostics.
func TestDispatchTerminal_SendsACK(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctrl *protocol.Control
	}{
		{
			name: "result",
			ctrl: &protocol.Control{Type: protocol.TypeResult, PromptID: "p-ack-r", ChatID: "c1", Result: &protocol.ResultPayload{Text: "done"}},
		},
		{
			name: "error",
			ctrl: &protocol.Control{Type: protocol.TypeError, PromptID: "p-ack-e", ChatID: "c1", Error: &protocol.ErrorPayload{Message: "boom"}},
		},
		{
			name: "notice",
			ctrl: &protocol.Control{Type: protocol.TypeNotice, PromptID: "p-ack-n", ChatID: "c1", Notice: &protocol.NoticePayload{Level: "info", Message: "hi"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			reg := NewBackendRegistry()
			d := NewDispatcher(sink, reg, NewTurnManager(), nil)
			conn := reg.Register("omp-1", "omp") // SendEvent needs a live conn
			defer conn.Close()
			d.turns.Start(tc.ctrl.PromptID, "c1", "om_progress", "omp-1")

			if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "omp-1", Control: tc.ctrl}); err != nil {
				t.Fatalf("DispatchControl: %v", err)
			}

			// The ACK rides the backend's SSE event channel. Drain it with a
			// short deadline; the dispatcher sends non-blocking.
			select {
			case ev := <-conn.eventCh:
				if ev.Type != protocol.TypeAck {
					t.Fatalf("event type = %q, want %q", ev.Type, protocol.TypeAck)
				}
				if ev.PromptID != tc.ctrl.PromptID {
					t.Errorf("ACK PromptID = %q, want %q", ev.PromptID, tc.ctrl.PromptID)
				}
				if ev.Ack == nil || ev.Ack.ControlType != tc.ctrl.Type {
					got := "<nil>"
					if ev.Ack != nil {
						got = ev.Ack.ControlType
					}
					t.Errorf("ACK ControlType = %q, want %q", got, tc.ctrl.Type)
				}
			case <-time.After(time.Second):
				t.Fatal("no ACK received on backend event channel within 1s")
			}
		})
	}
}

// TestDispatchTerminal_DuplicateStillACKs verifies a duplicate terminal (the
// backend is retrying because its first ACK was lost) still gets an ACK so the
// backend stops re-sending, even though the duplicate is dropped before render
// (the terminals dedup). Without this, a lost-ACK retry would loop forever.
func TestDispatchTerminal_DuplicateStillACKs(t *testing.T) {
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	d := NewDispatcher(sink, reg, NewTurnManager(), nil)
	conn := reg.Register("claude-1", "claude")
	defer conn.Close()
	const promptID = "p-dup-ack"
	d.turns.Start(promptID, "c1", "om_progress", "claude-1")

	mk := func() *protocol.Control {
		return &protocol.Control{Type: protocol.TypeResult, PromptID: promptID, ChatID: "c1", Result: &protocol.ResultPayload{Text: "done"}}
	}
	// First dispatch renders + ACKs.
	_ = d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: mk()})
	<-conn.eventCh // drain first ACK

	// Duplicate dispatch is dropped (dedup) but MUST still ACK.
	_ = d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: mk()})
	select {
	case ev := <-conn.eventCh:
		if ev.Type != protocol.TypeAck || ev.PromptID != promptID {
			t.Fatalf("duplicate ACK = %+v, want TypeAck/%s", ev, promptID)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate terminal did not produce an ACK; backend would retry forever")
	}
	// The duplicate must NOT have re-rendered (no extra send/update).
	sends, updates := sink.counts()
	if sends != 1 {
		t.Errorf("sends = %d, want 1 (duplicate must not re-render)", sends)
	}
	if updates != 0 {
		t.Errorf("updates = %d, want 0", updates)
	}
}
