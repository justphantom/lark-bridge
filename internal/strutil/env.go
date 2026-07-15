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
