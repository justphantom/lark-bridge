package ws

import (
	"context"
	"fmt"
	"sync"
)

// Pool sizing. A var (not const) so tests can shrink the queue to exercise
// the full-queue fallback. Four workers keep one slow consumer (file
// download + convert, tens of seconds) from head-of-line blocking every
// other chat's events; 64 queued events absorb a burst without growing
// without bound.
var (
	dispatchWorkers  = 4
	dispatchQueueCap = 64
)

// dispatchJob is one fully reassembled event awaiting dispatch to the sink.
// errf surfaces dispatch errors and panics (Lifecycle.OnError in production);
// the ACK has already been sent by the time a job runs, so this is the only
// reporting channel left.
type dispatchJob struct {
	ctx     context.Context
	rt      *router
	payload []byte
	errf    func(error)
}

// run dispatches one job. The recover keeps a panicking sink from killing
// the worker goroutine (and, on the inline fallback path, the receive loop);
// the panic is reported through errf instead of crashing the process.
func (j dispatchJob) run() {
	defer func() {
		if r := recover(); r != nil {
			j.errf(fmt.Errorf("ws: dispatch panic: %v", r))
		}
	}()
	if err := j.rt.dispatch(j.ctx, j.payload); err != nil {
		j.errf(fmt.Errorf("ws: dispatch: %w", err))
	}
}

// dispatchPool is a fixed-size worker pool draining a bounded job queue.
// One pool lives per session (runSession); close() drains queued jobs and
// guarantees every worker has returned before the session teardown completes,
// so workers never outlive their session across reconnects.
type dispatchPool struct {
	jobs chan dispatchJob
	wg   sync.WaitGroup
}

func newDispatchPool() *dispatchPool {
	p := &dispatchPool{jobs: make(chan dispatchJob, dispatchQueueCap)}
	p.wg.Add(dispatchWorkers)
	for range dispatchWorkers {
		go p.worker()
	}
	return p
}

// submit enqueues j without blocking. It reports false when the queue is
// full so the caller can fall back to inline dispatch — events are never
// silently dropped; backpressure lands on the read loop instead.
func (p *dispatchPool) submit(j dispatchJob) bool {
	select {
	case p.jobs <- j:
		return true
	default:
		return false
	}
}

// close shuts the pool down. Callers MUST have stopped submitting (in
// runSession this is guaranteed: receiveLoop has returned before close runs).
// Queued jobs are drained — closing the channel lets workers finish what is
// buffered rather than abandoning it — then close blocks until every worker
// has exited.
func (p *dispatchPool) close() {
	close(p.jobs)
	p.wg.Wait()
}

func (p *dispatchPool) worker() {
	defer p.wg.Done()
	for j := range p.jobs {
		j.run()
	}
}
