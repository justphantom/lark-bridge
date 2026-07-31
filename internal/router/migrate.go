package router

import (
	"os"
	"path/filepath"

	"github.com/justphantom/lark-bridge/internal/atomicwrite"
	"github.com/justphantom/lark-bridge/internal/log"
)

// legacyRouterFile is the pre-split shared router filename each backend used
// to read and write under state_dir. After the per-backend router split (R2)
// each backend owns router-<backend>.v5.json; this constant names the legacy
// file so a one-time migration can carry existing bindings forward.
const legacyRouterFile = "router.v5.json"

// MigrateLegacyBindings copies the legacy shared router file
// ({dir}/router.v5.json, where {dir} is the directory of targetPath) onto
// targetPath when targetPath does not yet exist but the legacy file does.
//
// This is a one-time best-effort migration introduced by the per-backend
// router split (R2): an existing deployment's bindings live in the shared
// file, and without this copy the first start of a backend with its new
// per-backend filename would see an empty router and reset every binding on
// the next save. After the copy, the per-backend file is authoritative and
// the legacy file is left in place (untouched) for operator rollback.
//
// Any error is logged at Warn and swallowed — a failed migration must never
// block startup, since bindings rebuild on the next /model etc. command.
// A nil logger silences the diagnostics (used by tests).
func MigrateLegacyBindings(targetPath string, logger *log.Logger) {
	if targetPath == "" {
		return
	}
	// Target already exists → nothing to migrate (per-backend file already
	// populated by a prior run or operator). A stat error other than
	// NotExist means the target dir is unreadable; bail rather than risk a
	// partial write, and let router.New's load surface the real problem.
	if info, err := os.Stat(targetPath); err == nil {
		if info.IsDir() {
			return
		}
		return
	} else if !os.IsNotExist(err) {
		return
	}

	legacy := filepath.Join(filepath.Dir(targetPath), legacyRouterFile)
	data, err := os.ReadFile(legacy) //nolint:gosec // G304: legacy path derives from the configured state_dir, not user input
	if err != nil {
		return // legacy absent or unreadable → clean install, nothing to migrate
	}
	if err := atomicwrite.Write(targetPath, data, filePerm); err != nil {
		if logger != nil {
			logger.Warn("router legacy migration skipped",
				"from", legacy, "to", targetPath, log.FieldError, err)
		}
		return
	}
	if logger != nil {
		logger.Info("router legacy bindings migrated to per-backend file",
			"from", legacy, "to", targetPath, "bytes", len(data))
	}
}
