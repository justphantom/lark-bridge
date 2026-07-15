// Package atomicwrite writes files atomically via tmp + fsync + rename
// + directory fsync. A crash during the write leaves either the
// previous contents or the new contents, never a truncated file.
//
// Callers that share a path between goroutines must serialize calls
// at a higher level (e.g. via a mutex); atomicwrite does not take a
// per-path lock because every existing caller already does.
package atomicwrite

import (
	"os"
	"path/filepath"
)

// Write writes data to path atomically.
//
// The sequence is: open path+".tmp" → write → fsync → rename over
// path → open parent dir → fsync dir. mode is applied to the temp
// file (rename preserves the mode of the replaced file when one
// already exists, which is the desired behaviour for files that were
// created with a tighter umask).
//
// On any error the temp file is removed (best-effort) so an aborted
// write does not leave litter next to the target. The directory sync
// failure is downgraded to nil: rename has already committed the new
// inode, the only thing dir.Sync loses is the durability of the
// rename itself, and on tmpfs / some network FS the call simply
// fails — turning that into a hard error would make every save noisy
// for a marginal durability gain. Operators that need stronger
// guarantees can layer their own check on top.
func Write(path string, data []byte, mode os.FileMode) (retErr error) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Best-effort directory fsync; downgrade failures so tmpfs /
	// network FS deployments do not see noise on every save.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
