package feishufront

import (
	"context"
	"sync"
	"time"
)

// finalFlushTimeout bounds the debouncer's shutdown flush. The normal flush
// reuses the debouncer's lifecycle ctx, which is already cancelled at shutdown;
// the final flush therefore takes a fresh ctx so pending progress frames reach
// Feishu, bounded so a stalled API cannot hang shutdown.
const finalFlushTimeout = 2 * time.Second

// cardDebouncer coalesces UpdateCard calls for the same messageID into one
// API call every flushInterval. The latest card wins; a background goroutine
// flushes pending updates.
type cardDebouncer struct {
	mu            sync.Mutex
	pending       map[string][]byte // messageID → latest card bytes
	bot           CardSink
	flushInterval time.Duration
	ctx           context.Context
}

func newCardDebouncer(ctx context.Context, bot CardSink, interval time.Duration) *cardDebouncer {
	d := &cardDebouncer{
		pending:       make(map[string][]byte),
		bot:           bot,
		flushInterval: interval,
		ctx:           ctx,
	}
	go d.flushLoop()
	return d
}

// enqueue stores the latest card for messageID. The flush goroutine sends it.
func (d *cardDebouncer) enqueue(messageID string, card []byte) {
	d.mu.Lock()
	d.pending[messageID] = card
	d.mu.Unlock()
}

// flush sends all pending updates and clears the buffer using the debouncer's
// lifecycle context. Callers that need a flush independent of that context
// (notably the shutdown path, where d.ctx is already cancelled) must call
// flushCtx with a fresh context so the last frame actually reaches Feishu.
func (d *cardDebouncer) flush() {
	d.flushCtx(d.ctx)
}

// flushCtx sends all pending updates using ctx. Split from flush so the
// shutdown path can pass a live context — d.ctx is cancelled by the time the
// final flush runs, so reusing it makes UpdateCard return immediately and the
// pending frame is silently dropped.
func (d *cardDebouncer) flushCtx(ctx context.Context) {
	d.mu.Lock()
	batch := d.pending
	d.pending = make(map[string][]byte, len(batch))
	d.mu.Unlock()
	for messageID, card := range batch {
		if err := d.bot.UpdateCard(ctx, messageID, card); err != nil {
			// Best-effort: a failed update is NOT re-enqueued (the batch was
			// already swapped out above), so this frame is lost. The next flush
			// only retries if a newer update for the same card arrives in the
			// interim. Terminal send paths call flush() first, then send the
			// final card, so a lost intermediate frame is usually harmless.
			continue
		}
	}
}

func (d *cardDebouncer) flushLoop() {
	ticker := time.NewTicker(d.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			// d.ctx is now cancelled; a fresh context lets the final flush
			// deliver any pending progress frame instead of no-op-ing.
			ctx, cancel := context.WithTimeout(context.Background(), finalFlushTimeout)
			d.flushCtx(ctx)
			cancel()
			return
		case <-ticker.C:
			d.flush()
		}
	}
}
