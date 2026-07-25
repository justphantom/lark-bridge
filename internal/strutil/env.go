package strutil

import (
	"os"
	"regexp"
)

// EnvVarPattern matches ${VAR} references where VAR is a valid env var name.
// Exported so other packages (e.g. config) share one definition of the
// ${VAR} surface syntax instead of duplicating the regex.
var EnvVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnvVars replaces ${VAR} patterns in s with the corresponding
// environment variable values. Variables that are unset or empty are left
// untouched so the caller can decide how to handle them.
//
// This is the LENIENT variant: missing values keep the literal ${VAR} text.
// It is intentionally distinct from internal/config.expandEnvVars, which is
// STRICT (unset/empty → error). Config injection must use the strict version
// so a missing IPC_SECRET cannot ship as the literal token. The lenient
// variant suits user-facing path expansion (e.g. SettingsFile) where a
// literal fallback can be a useful breadcrumb. Callers MUST NOT use this for
// secrets or any field where a silent literal pass-through is a security
// risk; reach for config.expandEnvVars instead.
func ExpandEnvVars(s string) string {
	return EnvVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		m := EnvVarPattern.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		val, ok := os.LookupEnv(m[1])
		if !ok {
			return match
		}
		return val
	})
}
