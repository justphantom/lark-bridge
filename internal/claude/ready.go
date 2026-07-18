package claude

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// checkVersion runs `<cliPath> --version` as a startup readiness probe:
// it proves the binary exists, is executable, and can complete its
// cheapest invocation. A missing/misconfigured CLI fails here instead of
// mid-turn. version is logged at Info so startup journals record it.
//
// backendName names the probe in error/log messages (e.g. "claude").
// timeout bounds the probe so a hung binary does not block startup beyond
// systemd's TimeoutStartSec. extraFields are appended to the "ready" log
// line for backend-specific context (e.g. claude's permission_mode).
func checkVersion(ctx context.Context, cliPath, backendName string, timeout time.Duration, logger *log.Logger, extraFields ...any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// #nosec G204 -- cliPath comes from the trusted config file, not user input.
	cmd := exec.CommandContext(ctx, cliPath, "--version")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s CLI not ready (%s --version): %w", backendName, cliPath, err)
	}
	fields := []any{"cli_path", cliPath}
	fields = append(fields, extraFields...)
	fields = append(fields, "version", strings.TrimSpace(string(out)))
	logger.Info(backendName+" CLI ready", fields...)
	return nil
}
