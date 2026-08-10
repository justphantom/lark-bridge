package statusmonitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

type fakeStatus struct {
	snap *protocol.StatusSnapshot
	err  error
}

func (f *fakeStatus) Status(context.Context) (*protocol.StatusSnapshot, error) {
	return f.snap, f.err
}

type fakeCtrl struct {
	mu       sync.Mutex
	controls []*protocol.Control
}

func (f *fakeCtrl) SendControl(_ context.Context, c *protocol.Control) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controls = append(f.controls, c)
	return nil
}

func (c *fakeCtrl) latest() *protocol.Control {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.controls) == 0 {
		return nil
	}
	return c.controls[len(c.controls)-1]
}

func TestTick_EmitsStatusReport(t *testing.T) {
	snap := &protocol.StatusSnapshot{
		InFlight: 2,
		Backends: []string{"claude-1", "status-1"},
		Turns: []protocol.TurnInfo{
			{BackendID: "claude-1", ChatID: "oc_abc", ElapsedS: 90},
		},
	}
	st := &fakeStatus{snap: snap}
	ctrl := &fakeCtrl{}
	now := time.Unix(1700000000, 0)
	h := New(Config{Interval: 60 * time.Second}, ctrl, st, "status-1", nil)
	h.now = func() time.Time { return now }

	h.tick(context.Background())

	c := ctrl.latest()
	if c == nil {
		t.Fatal("no control sent")
	}
	if c.Type != protocol.TypeStatusReport {
		t.Errorf("type = %q, want %q", c.Type, protocol.TypeStatusReport)
	}
	if c.StatusReport.Key != "status-1" {
		t.Errorf("key = %q", c.StatusReport.Key)
	}
	if c.StatusReport.GeneratedAt != now.Unix() {
		t.Errorf("generatedAt = %d, want %d", c.StatusReport.GeneratedAt, now.Unix())
	}
	if c.StatusReport.InFlight != 2 || len(c.StatusReport.Turns) != 1 || c.StatusReport.IntervalS != 60 {
		t.Errorf("payload snapshot mismatch: %+v", c.StatusReport)
	}
}

func TestTick_StatusErrorSkips(t *testing.T) {
	st := &fakeStatus{err: errors.New("boom")}
	ctrl := &fakeCtrl{}
	h := New(Config{}, ctrl, st, "status-1", nil)
	h.tick(context.Background())
	if len(ctrl.controls) != 0 {
		t.Errorf("status error should skip the tick, got %d controls", len(ctrl.controls))
	}
}

func TestHandleEvent_PromptRepliesNotice(t *testing.T) {
	ctrl := &fakeCtrl{}
	h := New(Config{Interval: 60 * time.Second}, ctrl, &fakeStatus{}, "status-1", nil)
	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "p1",
		Prompt:   &protocol.PromptPayload{ChatID: "oc_x", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	c := ctrl.latest()
	if c == nil || c.Type != protocol.TypeNotice {
		t.Fatalf("want 1 notice, got %+v", ctrl.controls)
	}
}

func TestHandleEvent_NonPromptIgnored(t *testing.T) {
	ctrl := &fakeCtrl{}
	h := New(Config{}, ctrl, &fakeStatus{}, "status-1", nil)
	// A non-prompt, non-ping event (e.g. Abort) must be silently ignored.
	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypeAbort}); err != nil {
		t.Fatalf("HandleEvent(abort): %v", err)
	}
	if len(ctrl.controls) != 0 {
		t.Errorf("abort should be ignored, got %d controls", len(ctrl.controls))
	}
}

func TestHandleEvent_PingAnswersPong(t *testing.T) {
	ctrl := &fakeCtrl{}
	h := New(Config{}, ctrl, &fakeStatus{}, "status-1", nil)
	// The frontend's C2 health probe must be answered with a TypePong,
	// otherwise the backend is evicted after maxMissedPongs.
	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePing}); err != nil {
		t.Fatalf("HandleEvent(ping): %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := ctrl.latest(); got != nil {
			if got.Type != protocol.TypePong || got.Pong == nil {
				t.Fatalf("expected TypePong, got %+v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no pong control emitted")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNew_DefaultsIntervalAndTitle(t *testing.T) {
	h := New(Config{}, &fakeCtrl{}, &fakeStatus{}, "b", nil)
	if h.cfg.Interval != 60*time.Second {
		t.Errorf("default interval = %v", h.cfg.Interval)
	}
	if h.cfg.Title != defaultTitle {
		t.Errorf("default title = %q", h.cfg.Title)
	}
}
