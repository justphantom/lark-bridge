package bridgebase

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// trackMax atomically updates *mx to cur when cur is larger. Avoids the
// read-modify-write race a naive "if cur > mx { mx = cur }" pattern
// introduces under -race.
func trackMax(mx *int32, cur int32) {
	for {
		old := atomic.LoadInt32(mx)
		if cur <= old {
			return
		}
		if atomic.CompareAndSwapInt32(mx, old, cur) {
			return
		}
	}
}

// TestSingleFlightJobRunner_GlobalRejectsConcurrent verifies a Global runner
// allows only one job at a time across all chatIDs.
func TestSingleFlightJobRunner_GlobalRejectsConcurrent(t *testing.T) {
	r := NewSingleFlightJobRunner(SingleFlightGlobal, log.Nop(), time.Second)

	var inFlight, maxInFlight int32
	var jobWg sync.WaitGroup
	job := func(_ context.Context) ([]byte, error) {
		defer jobWg.Done()
		cur := atomic.AddInt32(&inFlight, 1)
		trackMax(&maxInFlight, cur)
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return []byte("ok"), nil
	}
	var acquireWg sync.WaitGroup
	for i := range 5 {
		jobWg.Add(1)
		acquireWg.Add(1)
		go func(i int) {
			defer acquireWg.Done()
			// Acquire registers the job and, on reject, fires the 4th-arg
			// callback synchronously to balance jobWg — so the bool return
			// needs no extra handling here.
			_ = r.Acquire("chat-"+string(rune('a'+i)), "test", job, nil,
				func(level, title, body string) { jobWg.Done() })
		}(i)
	}
	acquireWg.Wait()
	jobWg.Wait()

	if maxInFlight > 1 {
		t.Errorf("maxInFlight = %d, want <= 1 (Global mode)", maxInFlight)
	}
}

// TestSingleFlightJobRunner_PerChatParallel verifies a PerChat runner lets
// different chats run in parallel while serialising jobs within one chat.
func TestSingleFlightJobRunner_PerChatParallel(t *testing.T) {
	r := NewSingleFlightJobRunner(SingleFlightPerChat, log.Nop(), time.Second)

	var inFlight, maxInFlight int32
	var jobWg sync.WaitGroup
	job := func(_ context.Context) ([]byte, error) {
		defer jobWg.Done()
		cur := atomic.AddInt32(&inFlight, 1)
		trackMax(&maxInFlight, cur)
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return nil, nil
	}
	var acquireWg sync.WaitGroup
	for _, chat := range []string{"c1", "c2", "c3"} {
		jobWg.Add(1)
		acquireWg.Add(1)
		go func(chat string) {
			defer acquireWg.Done()
			r.Acquire(chat, "test", job, nil, nil)
		}(chat)
	}
	acquireWg.Wait()
	jobWg.Wait()

	if maxInFlight < 2 {
		t.Errorf("maxInFlight = %d, want >= 2 (PerChat parallel)", maxInFlight)
	}
}

// TestSingleFlightJobRunner_PropagatesError verifies the terminal notice
// fires with level=error when the Job returns one.
func TestSingleFlightJobRunner_PropagatesError(t *testing.T) {
	r := NewSingleFlightJobRunner(SingleFlightGlobal, log.Nop(), time.Second)

	done := make(chan struct{})
	var gotLevel, gotTitle string
	job := func(_ context.Context) ([]byte, error) {
		return []byte("partial"), errors.New("boom")
	}
	r.Acquire("c1", "deploy", job,
		func(level, title, _ string) {
			gotLevel, gotTitle = level, title
			close(done)
		}, nil)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notice never fired")
	}
	if gotLevel != "error" {
		t.Errorf("level = %q, want error", gotLevel)
	}
	if gotTitle != "deploy失败" {
		t.Errorf("title = %q, want 'deploy失败'", gotTitle)
	}
}
