package bridgebase

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

const (
	// defaultGitTimeout bounds one git job. git push/pull is normally
	// sub-minute; 5m is the safety net for slow networks or large repos.
	defaultGitTimeout = 5 * time.Minute
	// gitTailBytes caps the output embedded in the terminal notice: a full
	// git push log would flood the chat card.
	gitTailBytes = 500
)

// GitCommander runs a command (name with args) inside dir. The production
// implementation is ExecCommander; tests inject a fake. Structurally
// identical to deploymonitor.Commander — kept local so this package does
// not import a sibling backend package.
type GitCommander interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

// ExecCommander is the production GitCommander: CombinedOutput under dir.
type ExecCommander struct{}

func (ExecCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// GitNotice emits one notice for the chat that triggered the job. Binding
// the chatID/promptID is the caller's responsibility (each bridge wraps its
// own emit path), so GitRunner stays free of protocol types.
type GitNotice func(level, title, body string)

// GitRunner runs `git <args...>` in a chat's bound directory with per-chat
// single-flight: a second git job for the same chatID is rejected inline
// while one is running. Different chats run in parallel. Jobs run on
// background goroutines so the slash-command dispatcher returns
// immediately with a "triggered" notice; the terminal success/error notice
// is delivered via the GitNotice callback when git exits.
type GitRunner struct {
	cmd     GitCommander
	logger  *log.Logger
	timeout time.Duration
	slots   sync.Map // chatID -> *sync.Mutex
}

// NewGitRunner builds a runner. timeout <=0 → defaultGitTimeout. A nil
// logger is replaced with a no-op so tests can pass nil.
func NewGitRunner(cmd GitCommander, logger *log.Logger, timeout time.Duration) *GitRunner {
	if logger == nil {
		logger = log.Nop()
	}
	if timeout <= 0 {
		timeout = defaultGitTimeout
	}
	return &GitRunner{cmd: cmd, logger: logger, timeout: timeout}
}

// AcquireAndRun runs `git args...` in dir for chatID. If a job is already
// running for chatID it calls notice with a "进行中" warning and returns;
// otherwise it launches the job on a background goroutine, calls notice
// with a "已触发" info first, then a terminal success/error notice when
// the job finishes. dir must be non-empty (the caller validates).
func (r *GitRunner) AcquireAndRun(chatID, dir string, args []string, label string, notice GitNotice) {
	mu := r.slot(chatID)
	if !mu.TryLock() {
		r.logger.Info("git job rejected: chat busy",
			log.FieldChatID, chatID, "label", label)
		notice("warning", label+"进行中", "本群已有一次 "+label+" 操作正在执行，请等待其完成后再试。")
		return
	}
	notice("info", label+"已触发", "开始执行 "+gitLabel(args)+"，完成后会在此通知。")
	go func() {
		defer mu.Unlock()
		r.runJob(chatID, dir, args, label, notice)
	}()
}

// runJob is the goroutine body: bounded ctx, run git, emit terminal notice.
// context.Background (not the dispatcher's ctx) so the job outlives the
// triggering request — mirrors deploymonitor.runJob.
func (r *GitRunner) runJob(chatID, dir string, args []string, label string, notice GitNotice) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	r.logger.Info("git job start",
		log.FieldChatID, chatID,
		"dir", dir, "cmd", gitLabel(args))

	out, err := r.cmd.Run(ctx, dir, "git", args...)
	if err != nil {
		r.logger.Error("git job failed",
			log.FieldChatID, chatID, "cmd", gitLabel(args), log.FieldError, err)
		notice("error", label+"失败", tailGitOutput(out)+"\n错误："+err.Error())
		return
	}
	r.logger.Info("git job done", log.FieldChatID, chatID, "cmd", gitLabel(args))
	notice("success", label+"完成", tailGitOutput(out))
}

// slot returns the per-chat mutex, allocating one on first use.
// LoadOrStore guarantees a single canonical instance per chatID even under
// concurrent first-use; the occasional wasted &sync.Mutex{} is GC'd.
func (r *GitRunner) slot(chatID string) *sync.Mutex {
	v, _ := r.slots.LoadOrStore(chatID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func gitLabel(args []string) string {
	return strings.Join(append([]string{"git"}, args...), " ")
}

func tailGitOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) <= gitTailBytes {
		return s
	}
	return "…" + s[len(s)-gitTailBytes:]
}
