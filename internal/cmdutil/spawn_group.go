package cmdutil

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// GroupKillTimeout bounds how long exec.CommandContext waits after sending
// the cancel signal (group SIGKILL) before force-closing stdout/stderr
// pipes. Without it, Wait blocks until every grandchild that inherited a
// pipe write end exits — which for `make deploy` (recursive make, docker,
// npm, ssh …) can be never.
const GroupKillTimeout = 30 * time.Second

// MaxCombinedOutput bounds RunCombinedBounded's capture. Deploy logs can
// run to hundreds of MB; the head is enough for diagnostics, the caller's
// tailOutput renders the chat-friendly suffix.
const MaxCombinedOutput = 1 << 20 // 1 MiB

// ApplyGroupCancel configures cmd so a ctx cancellation SIGKILLs the whole
// process tree (the spawned process plus any tool subprocesses it forks),
// not just the leaf. It must be called before cmd.Start().
//
// Why: exec.CommandContext's default Cancel only SIGKILLs cmd.Process.Pid.
// Tool subprocesses (bash, git, npm …) inherit the stdout write end of
// the pipe; when the main process dies, CombinedOutput/StdoutPipe readers
// stay blocked waiting for those grandchildren to exit. Setting a process
// group (Setpgid) makes -cmd.Process.Pid address the whole tree, and
// WaitDelay forces exec to close the pipes within GroupKillTimeout even
// if a grandchild escapes the SIGKILL.
//
// POSIX-only: process groups are a Linux/Darwin concept.
func ApplyGroupCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = GroupKillTimeout
}

// RunCombinedBounded runs name args in dir under ctx, capturing combined
// stdout+stderr up to MaxCombinedOutput bytes. On ctx cancel the whole
// process tree is SIGKILLed within GroupKillTimeout.
//
// Use for short-running bounded commands (git, make) where streaming
// parsing is not required. For long-running streaming subprocesses, use
// exec.CommandContext + ApplyGroupCancel + StdoutPipe directly.
func RunCombinedBounded(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	ApplyGroupCancel(cmd)
	w := newLimitedWriter(MaxCombinedOutput)
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	return w.bytes(), err
}

// limitedWriter is a concurrency-safe byte buffer that silently drops
// writes past maxBytes. It reports the full write length to the caller so
// the producer (cmd.Run's internal pipe copier) does not surface a "short
// write" error.
type limitedWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newLimitedWriter(max int) *limitedWriter {
	return &limitedWriter{max: max}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if len(w.buf) < w.max {
		room := w.max - len(w.buf)
		if room > n {
			room = n
		}
		w.buf = append(w.buf, p[:room]...)
	}
	return n, nil
}

func (w *limitedWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
}
