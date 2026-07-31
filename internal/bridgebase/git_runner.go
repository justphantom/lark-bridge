package bridgebase

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
)

const (
	// defaultGitTimeout bounds one git job. git push/pull is normally
	// sub-minute; 5m is the safety net for slow networks or large repos.
	defaultGitTimeout = 5 * time.Minute
	// gitTailRunes caps the output embedded in the terminal notice (in RUNES,
	// not bytes): a full git push log would flood the chat card. A rune budget
	// keeps multi-byte logs (Chinese commit messages, 3 bytes/char) legible
	// where a byte budget would split a character.
	gitTailRunes = 500
)

// GitCommander runs a command (name with args) inside dir. The production
// implementation is ExecCommander; tests inject a fake. Structurally
// identical to deploymonitor.Commander — kept local so this package does
// not import a sibling backend package.
type GitCommander interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

// ExecCommander is the production GitCommander: CombinedOutput under dir.
// Tree-wide SIGKILL on ctx cancel so git's remote helpers (git-remote-https,
// gpg) cannot survive a /pull or /push abort.
type ExecCommander struct{}

func (ExecCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return cmdutil.RunCombinedBounded(ctx, dir, name, args...)
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
// running for chatID it calls notice with a "进行中" warning and returns
// false; otherwise it launches the job on a background goroutine and returns
// true. The caller owns the non-terminal "running" banner (emitted on true);
// notice is called only for terminal states — busy (synchronous reject) and
// success/error when git exits — so a caller can bind every terminal to the
// triggering promptID and patch the progress card in place. dir must be
// non-empty (the caller validates).
func (r *GitRunner) AcquireAndRun(chatID, dir string, args []string, label string, notice GitNotice) bool {
	mu := r.slot(chatID)
	if !mu.TryLock() {
		r.logger.Info("git job rejected: chat busy",
			log.FieldChatID, chatID, "label", label)
		notice("warning", label+"进行中", "本群已有一次 "+label+" 操作正在执行，请等待其完成后再试。")
		return false
	}
	// GoSafe: a panic inside runJob must not crash the backend process.
	// mu.Unlock is deferred inside fn so the per-chat slot is still released
	// during the panic unwind before GoSafe's recover catches it.
	GoSafe(r.logger, "git job: "+label, func() {
		defer mu.Unlock()
		r.runJob(chatID, dir, args, label, notice)
	})
	return true
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
	mu, _ := v.(*sync.Mutex)
	return mu
}

func gitLabel(args []string) string {
	return strings.Join(append([]string{"git"}, args...), " ")
}

// tailGitOutput returns the last ~gitTailRunes runes of out, advanced to the
// next line boundary. A byte-based tail garbles UTF-8 (Chinese deploy/commit
// logs are 3 bytes/char) and can open mid-line; the rune+line form keeps the
// excerpt readable.
func tailGitOutput(out []byte) string {
	return tailRunes(string(out), gitTailRunes)
}

// tailRunes returns the last ~maxRunes runes of s (TrimSpace'd), advanced to
// the next line boundary so the excerpt never opens on a half-line fragment.
// maxRunes<=0 disables truncation. Shared shape with deploymonitor.tailOutput;
// duplicated rather than shared so neither package imports the other.
func tailRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	cut := string(r[len(r)-maxRunes:])
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "…" + cut
}
