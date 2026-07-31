// Package clibase carries the minimal shared surface every CLI subprocess
// backend (claude / opencode / omp) wires into its client: a single
// `--version` readiness probe plus constants that bound the per-line scanner
// buffer and stderr capture used by each backend's local pump.
//
// The package deliberately does NOT provide a generic Client[Event] base or a
// shared Pump — the per-backend pump is concurrency-sensitive (process group
// kill, stderr goroutine, sem release), and merging three independent fault
// domains into one shared loop would make a bug in one backend take down all
// three.
package clibase

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
)

// maxVersionBytes caps how much of a `--version` (or list) stdout we keep. The
// real output is a single short line, but cmd.Output() would otherwise swallow
// a pathological CLI's unbounded stdout whole; the tail past the cap is
// discarded (drained) so the child is never blocked on a full pipe either.
const maxVersionBytes = 1 << 20 // 1 MiB

// MaxLineLen caps the per-line buffer for the stdout scanner. CLI stream-json
// / NDJSON lines are usually small but tool_result payloads (file reads,
// command output) can run to several MiB. The scanner buffer grows lazily to
// fit the largest line, so this is a per-run ceiling, not a pre-allocation;
// 16 MiB covers realistic tool output without letting a pathological stream
// exhaust memory.
const MaxLineLen = 1 << 24

// MaxStderrBytes bounds the stderr capture so a pathological CLI run cannot
// exhaust memory. The head of stderr is where the actionable diagnostic
// lives; 64 KiB is ample for that.
const MaxStderrBytes = 64 << 10

// ScannerInitBuf is the initial buffer for the stdout scanner. The scanner
// grows this lazily up to MaxLineLen, so it is a starting allocation, not a
// cap.
const ScannerInitBuf = 64 << 10

// MaxLogLineBytes caps how much of an unparseable line is written to the log
// on a parse failure. A pathological line can be up to MaxLineLen (16 MiB);
// logging it whole would bloat structured-log output, so only the head is
// kept.
const MaxLogLineBytes = 1 << 10 // 1 KiB

// CheckVersion runs `<cliPath> --version` as a startup readiness probe: it
// proves the binary exists, is executable, and can complete its cheapest
// invocation. A missing/misconfigured CLI fails here instead of mid-turn. The
// version is logged at Info so startup journals record it.
//
// backendName names the probe in error/log messages (e.g. "claude"). timeout
// bounds the probe so a hung binary does not block startup beyond a service
// manager's start timeout. extraFields are appended to the "ready" log line
// for backend-specific context (e.g. claude's permission_mode).
//
// All three CLI backends (claude/opencode/omp) had a byte-identical copy of
// this helper in their ready.go; the logger type differs in name only
// (`internal/log.Logger` is a `*slog.Logger` alias), so the shared version
// takes `*log.Logger` and works for all three call sites.
func CheckVersion(ctx context.Context, cliPath, backendName string, timeout time.Duration, logger *log.Logger, extraFields ...any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// #nosec G204 -- cliPath comes from trusted Options / config, not user input.
	cmd := exec.CommandContext(ctx, cliPath, "--version")
	// Sanitised env: strip the bridge's own secrets (FEISHU_APP_SECRET,
	// IPC_SECRET, ENCRYPT_KEY, …) so a user-run tool cannot read them via the
	// subprocess's environment (Low#19). The CLI's own *_API_KEY survives.
	cmd.Env = cmdutil.SanitizeChildEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s CLI not ready (%s --version): %w", backendName, cliPath, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s CLI not ready (%s --version): %w", backendName, cliPath, err)
	}
	// Bound the captured output (maxVersionBytes) and drain the rest so a
	// pathological CLI cannot OOM us via unbounded stdout nor block on a full
	// pipe. Drain happens before Wait per the os/exec StdoutPipe contract.
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, io.LimitReader(stdout, maxVersionBytes))
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s CLI not ready (%s --version): %w", backendName, cliPath, err)
	}
	fields := []any{"cli_path", cliPath}
	fields = append(fields, extraFields...)
	fields = append(fields, "version", strings.TrimSpace(buf.String()))
	logger.Info(backendName+" CLI ready", fields...)
	return nil
}
