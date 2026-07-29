package bridgebase

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// ErrJobRunning is the sentinel a SingleFlightJobRunner returns (via the
// rejected callback) when a job is already in flight in the chosen scope.
// Exposed so callers can map it to a domain-specific "进行中" notice.
var ErrJobRunning = errors.New("bridgebase: job already running")

// JobNotice is the terminal callback every accepted job eventually fires.
// level/title/body follow the protocol.NoticePayload conventions
// ("info"/"success"/"warning"/"error"). Called exactly once per accepted
// job; never called for a rejected one.
type JobNotice func(level, title, body string)

// Job runs to completion under a caller-supplied context. The runner wires
// the per-job context with a timeout; the Job body should honour ctx.
// Returning a non-nil error maps to level=error in the notice.
type Job func(ctx context.Context) ([]byte, error)

// SingleFlightJobRunner runs jobs with at-most-one-in-flight semantics.
// Mode Global rejects any second job while one is running; mode PerChat
// rejects only a job whose chatID already has one running and lets other
// chats run in parallel.
//
// Both modes share the same lifecycle: Acquire (try-lock) → spawn goroutine
// → run with a bounded ctx → emit terminal notice on exit. Backends supply
// the Job (what to do) and a Notice (how to surface the result); the runner
// owns the slot tracking.
//
// The deploymonitor backend uses Global mode (one deploy at a time across
// all chats); the bridge GitRunner uses PerChat (different chats can
// /pull /push in parallel, same chat is serial).
type SingleFlightJobRunner struct {
	logger  *log.Logger
	timeout time.Duration

	// One of modeGlobal / modePerChat is non-nil. modeGlobal==nil &&
	// modePerChat==nil means "accept nothing" (a misconfiguration that
	// surfaces on first Acquire as a panic).
	globalMu  *sync.Mutex
	chatSlots sync.Map // chatID -> *sync.Mutex
}

// SingleFlightMode picks Global or PerChat semantics.
type SingleFlightMode int

const (
	// SingleFlightGlobal rejects any second job while one is running, across
	// all chatIDs.
	SingleFlightGlobal SingleFlightMode = iota
	// SingleFlightPerChat rejects only a job whose chatID already has one
	// running.
	SingleFlightPerChat
)

// NewSingleFlightJobRunner builds a runner. timeout <=0 falls back to
// defaultGitTimeout (5m) for parity with GitRunner — deploy / git jobs are
// the canonical callers. A nil logger is replaced with a no-op.
func NewSingleFlightJobRunner(mode SingleFlightMode, logger *log.Logger, timeout time.Duration) *SingleFlightJobRunner {
	if logger == nil {
		logger = log.Nop()
	}
	if timeout <= 0 {
		timeout = defaultGitTimeout
	}
	r := &SingleFlightJobRunner{logger: logger, timeout: timeout}
	if mode == SingleFlightGlobal {
		r.globalMu = &sync.Mutex{}
	}
	return r
}

// Acquire tries to take the slot for chatID. Returns true on accept (and
// spawns the job on its own goroutine), false on reject (in which case
// rejectNotice is invoked synchronously and the Job never runs). The
// accepted job's terminal notice fires from the goroutine — once with
// level/title/body describing the outcome.
//
// For Global mode the chatID is ignored (one slot for all callers). For
// PerChat it keys the per-chat slot map.
func (r *SingleFlightJobRunner) Acquire(chatID, label string, job Job, okNotice, rejectNotice JobNotice) bool {
	mu := r.slotFor(chatID)
	if !mu.TryLock() {
		r.logger.Info("job rejected: slot busy",
			log.FieldChatID, chatID, "label", label)
		if rejectNotice != nil {
			rejectNotice("warning", label+"进行中", "本群已有一次 "+label+" 操作正在执行，请等待其完成后再试。")
		}
		return false
	}
	go func() {
		defer mu.Unlock()
		r.runJob(chatID, label, job, okNotice)
	}()
	return true
}

func (r *SingleFlightJobRunner) runJob(chatID, label string, job Job, notice JobNotice) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	r.logger.Info("job start",
		log.FieldChatID, chatID, "label", label)
	out, err := job(ctx)
	if err != nil {
		r.logger.Error("job failed",
			log.FieldChatID, chatID, "label", label, log.FieldError, err)
		if notice != nil {
			notice("error", label+"失败", string(out)+"\n错误："+err.Error())
		}
		return
	}
	r.logger.Info("job done", log.FieldChatID, chatID, "label", label)
	if notice != nil {
		notice("success", label+"完成", string(out))
	}
}

func (r *SingleFlightJobRunner) slotFor(chatID string) *sync.Mutex {
	if r.globalMu != nil {
		return r.globalMu
	}
	v, _ := r.chatSlots.LoadOrStore(chatID, &sync.Mutex{})
	mu, _ := v.(*sync.Mutex)
	return mu
}
