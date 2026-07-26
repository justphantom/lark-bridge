package cmdutil

import (
	"os"
	"strings"
)

// sensitiveEnvSubstrings names env-var key fragments that mark a variable as
// secret. The bridge's own FEISHU_APP_SECRET / IPC_SECRET / ENCRYPT_KEY /
// OPENCODE_SERVER_PASSWORD etc. must not leak into CLI child processes where
// a user-run Bash tool could read them via `env`. Matched case-insensitively
// against the uppercased key.
var sensitiveEnvSubstrings = []string{"SECRET", "TOKEN", "ENCRYPT", "PASS", "PRIVATE_KEY", "CREDENTIAL"}

// SanitizeChildEnv returns the current process environment with secret keys
// removed. Used by backend wrappers before spawning CLI subprocesses (claude /
// opencode / miniagent) so the bridge's own credentials are not inherited by a
// process the user can introspect.
//
// This is a deny-list (not allow-list): a CLI may legitimately need its own
// *_API_KEY (e.g. ANTHROPIC_API_KEY, MINIAGENT_API_KEY), and an allow-list
// would have to track every CLI's requirements. The deny-list targets the
// bridge-specific secret suffixes and is safe to extend.
func SanitizeChildEnv() []string {
	return FilterChildEnv(os.Environ())
}

// FilterChildEnv applies the secret deny-list to an explicit env slice. Split
// out so tests do not depend on the live process environment.
func FilterChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if isSensitiveEnvKey(key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isSensitiveEnvKey(key string) bool {
	up := strings.ToUpper(key)
	for _, frag := range sensitiveEnvSubstrings {
		if strings.Contains(up, frag) {
			return true
		}
	}
	return false
}
