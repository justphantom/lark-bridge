package bridgebase

import (
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestAnswerBroker_RegisterDeliver verifies the happy path: a registered
// slot receives its answer and is removed.
func TestAnswerBroker_RegisterDeliver(t *testing.T) {
	b := NewAnswerBroker()
	ch, ok := b.Register("req-1")
	if !ok {
		t.Fatal("first Register should succeed")
	}
	if !b.Deliver("req-1", &protocol.AnswerPayload{Choices: []string{"a"}}) {
		t.Fatal("Deliver to a registered slot should return true")
	}
	select {
	case ans := <-ch:
		if len(ans.Choices) != 1 || ans.Choices[0] != "a" {
			t.Errorf("got choices=%v, want [a]", ans.Choices)
		}
	case <-time.After(time.Second):
		t.Fatal("answer not delivered within 1s")
	}
	// Slot is gone after Deliver.
	if b.Deliver("req-1", &protocol.AnswerPayload{}) {
		t.Error("second Deliver should return false (slot drained)")
	}
}

// TestAnswerBroker_DoubleRegisterFails verifies a duplicate requestID is
// rejected so two concurrent pickers cannot collide on one id.
func TestAnswerBroker_DoubleRegisterFails(t *testing.T) {
	b := NewAnswerBroker()
	if _, ok := b.Register("dup"); !ok {
		t.Fatal("first Register should succeed")
	}
	if _, ok := b.Register("dup"); ok {
		t.Error("second Register with same id should fail")
	}
}

// TestAnswerBroker_DeliverUnknown verifies delivering to a slot that was
// never registered (or already cancelled/timed out) returns false and
// discards silently.
func TestAnswerBroker_DeliverUnknown(t *testing.T) {
	b := NewAnswerBroker()
	if b.Deliver("nope", &protocol.AnswerPayload{}) {
		t.Error("Deliver to unknown id should return false")
	}
}

// TestAnswerBroker_Cancel verifies Cancel removes a slot; a subsequent
// Deliver returns false and the channel never fires.
func TestAnswerBroker_Cancel(t *testing.T) {
	b := NewAnswerBroker()
	ch, _ := b.Register("c1")
	b.Cancel("c1")
	if b.Deliver("c1", &protocol.AnswerPayload{}) {
		t.Error("Deliver after Cancel should return false")
	}
	select {
	case ans, ok := <-ch:
		if ok || ans != nil {
			t.Errorf("channel should stay open and empty, got ans=%v ok=%v", ans, ok)
		}
	case <-time.After(50 * time.Millisecond):
		// Expected: channel never fires because Deliver was a no-op.
	}
}

// TestAnswerBroker_Drain verifies Drain closes every pending channel so
// blocked picker goroutines return immediately at shutdown. This is the
// guard against "Close hangs forever waiting on a picker".
func TestAnswerBroker_Drain(t *testing.T) {
	b := NewAnswerBroker()
	ch1, _ := b.Register("d1")
	ch2, _ := b.Register("d2")
	b.Drain()
	for i, ch := range []<-chan *protocol.AnswerPayload{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("slot %d: channel should be closed", i)
			}
		case <-time.After(time.Second):
			t.Errorf("slot %d: channel not closed within 1s", i)
		}
	}
	if got := b.PendingIDs(); len(got) != 0 {
		t.Errorf("after Drain, pending=%v, want empty", got)
	}
}

// TestAnswerBroker_DoubleDeliverKeepsFirst verifies a second Deliver to the
// same id (e.g. a double-clicked card) does not overwrite the first answer
// in the buffered channel.
func TestAnswerBroker_DoubleDeliverKeepsFirst(t *testing.T) {
	b := NewAnswerBroker()
	ch, _ := b.Register("dd")
	first := &protocol.AnswerPayload{Choices: []string{"first"}}
	second := &protocol.AnswerPayload{Choices: []string{"second"}}
	if !b.Deliver("dd", first) {
		t.Fatal("first Deliver should succeed")
	}
	if b.Deliver("dd", second) {
		t.Error("second Deliver should return false (slot drained)")
	}
	select {
	case ans := <-ch:
		if ans.Choices[0] != "first" {
			t.Errorf("got %q, want first answer preserved", ans.Choices[0])
		}
	case <-time.After(time.Second):
		t.Fatal("first answer never delivered")
	}
}

// TestAnswerBroker_PendingIDs verifies PendingIDs reflects current slots
// (used by tests that need to discover a picker's id).
func TestAnswerBroker_PendingIDs(t *testing.T) {
	b := NewAnswerBroker()
	b.Register("a")
	b.Register("b")
	b.Register("c")
	got := b.PendingIDs()
	if len(got) != 3 {
		t.Fatalf("pending=%v, want 3 ids", got)
	}
}
