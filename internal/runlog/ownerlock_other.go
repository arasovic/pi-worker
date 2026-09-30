//go:build !darwin && !linux

package runlog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// AcquireOwnerLock is not supported on this platform.
func AcquireOwnerLock(string) (*os.File, error) {
	return nil, errors.ErrUnsupported
}

// ProbeOwnerLock cannot test the lock on this platform: a lock file
// that exists reads as unknown, which readers treat as alive.
func ProbeOwnerLock(runDir string) LockState {
	_, err := os.Lstat(filepath.Join(runDir, OwnerLockName))
	if errors.Is(err, fs.ErrNotExist) {
		return LockAbsent
	}
	return LockUnknown
}

// TryLockOwner is not supported on this platform.
func TryLockOwner(*os.File) (bool, error) {
	return false, errors.ErrUnsupported
}
