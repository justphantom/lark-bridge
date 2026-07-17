// Package router maps a Feishu chatID to a persistent backend session id
// and the working directory that session lives in, plus the pinned model,
// agent, permission mode, effort level and settings file the bridge
// forwards to the backend on every prompt.
//
// One chatID maps to exactly one Binding for the lifetime of the bridge
// process. If persistPath is non-empty, bindings are saved to disk on every
// mutation and loaded on startup so they survive process restarts.
package router

import (
	"fmt"
	"sync"

	"github.com/hu/lark-bridge/internal/log"
)

// Binding pairs a backend session id with the working directory the session
// lives in, a title snapshot used for display, and the pinned model / agent
// (opencode) / permission mode / effort level / settings file (claude) that
// the bridge should forward. Fields are the union of both source backends.
//
// Empty means "not set" for every optional field; callers fall back to the
// backend's configured default.
type Binding struct {
	SessionID      string `json:"sessionID,omitempty"`
	Directory      string `json:"directory,omitempty"`
	Title          string `json:"title,omitempty"`
	ModelSpec      string `json:"modelSpec,omitempty"`
	Agent          string `json:"agent,omitempty"`          // opencode
	PermissionMode string `json:"permissionMode,omitempty"` // claude
	EffortLevel    string `json:"effortLevel,omitempty"`    // claude
	SettingsFile   string `json:"settingsFile,omitempty"`   // claude
}

// Router is safe for concurrent use.
type Router struct {
	mu          sync.RWMutex
	bindings    map[string]Binding
	persistPath string
	saveMu      sync.Mutex
	logger      *log.Logger

	// saveCh / saveStop / saveDone drive the single-worker save coalescer.
	// saveCh is buffered(1): a non-blocking send triggers a save; if a save
	// is already pending, additional sends are dropped (coalescing) because
	// the worker always reads the freshest r.bindings snapshot. saveStop is
	// closed by Close to terminate saveLoop; saveDone is closed by saveLoop
	// on exit so Close can wait for the loop to drain before doing the final
	// synchronous save. All are nil when persistPath == "" so saveAsync is a
	// pure no-op and no goroutine is started.
	saveCh    chan struct{}
	saveStop  chan struct{}
	saveDone  chan struct{}
	closeOnce sync.Once
}

// New returns a router that persists bindings to persistPath (loaded on
// startup, saved on every mutation). persistPath="" → in-memory only.
//
// logger is read by load()/save() (the latter runs on the saveLoop goroutine
// started here), so it MUST be supplied at construction — setting it after
// New returns races saveLoop. A nil logger falls back to a no-op logger.
func New(persistPath string, logger *log.Logger) (*Router, error) {
	if logger == nil {
		logger = log.Nop()
	}
	r := &Router{
		bindings:    make(map[string]Binding),
		persistPath: persistPath,
		logger:      logger,
	}
	if persistPath != "" {
		r.saveCh = make(chan struct{}, 1)
		r.saveStop = make(chan struct{})
		r.saveDone = make(chan struct{})
		if err := r.load(); err != nil {
			return nil, fmt.Errorf("router: load %s: %w", persistPath, err)
		}
		go r.saveLoop()
	}
	return r, nil
}

// Close stops the save coalescer goroutine and performs a final synchronous
// save so the last mutation before shutdown is not lost. Idempotent via
// sync.Once. The caller should invoke it during process shutdown; in-memory
// routers (persistPath == "") do not need it.
//
// Correctness invariant: the final synchronous save() below is load-bearing.
// A mutation that called saveAsync between close(saveStop) and saveLoop
// observing it may leave its signal sitting in saveCh forever (the loop has
// exited), so that mutation is never persisted by the loop. The final save()
// rescues it because it re-reads r.bindings directly. Do not remove it even
// though it looks redundant with the loop's own saves.
//
// Lifecycle constraint: callers MUST NOT invoke any Set*/mutate after Close
// returns. Once the save loop has exited, a later mutation lands in r.bindings
// but no save will ever fire, so it is silently lost on the next restart. The
// bridge binaries respect this by stopping the handler (which gates all
// mutations) before calling Close in the LIFO defer chain.
func (r *Router) Close() {
	r.closeOnce.Do(func() {
		if r.saveStop != nil {
			close(r.saveStop)
			// Wait for saveLoop to exit so no concurrent save() can race the
			// final save() below.
			<-r.saveDone
			if err := r.save(); err != nil {
				r.logger.Error("router final save failed", log.FieldPath, r.persistPath, log.FieldError, err)
			}
		}
	})
}
