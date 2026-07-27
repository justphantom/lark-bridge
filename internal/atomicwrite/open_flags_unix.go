//go:build linux || darwin

package atomicwrite

import (
	"os"
	"syscall"
)

// createFlags are the open flags used for the temp file. O_NOFOLLOW refuses
// to follow a symlink at the tmp path: if an attacker with write access to
// the state directory pre-creates path+".tmp" as a symlink to a privileged
// target, the open fails rather than writing through the link. POSIX-only.
const createFlags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC | syscall.O_NOFOLLOW
