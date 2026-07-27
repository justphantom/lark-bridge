//go:build !linux && !darwin

package atomicwrite

import "os"

// createFlags on non-POSIX platforms omits O_NOFOLLOW (the flag does not
// exist on Windows, which has no symlink-following open vector for this
// pattern anyway). atomicwrite itself stays cross-platform compilable.
const createFlags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
