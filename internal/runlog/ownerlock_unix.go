//go:build darwin || linux

package runlog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireOwnerLock creates runDir/owner.lock and takes an exclusive
// lock on it, blocking only while a probe briefly holds a shared lock.
// The file must not exist yet: a pre-existing file or a symbolic link
// at that name is refused, so the lock is always one this process
// created. The caller keeps the returned file open for as long as it
// owns the run; closing it, or exiting, releases the lock.
func AcquireOwnerLock(runDir string) (*os.File, error) {
	path := filepath.Join(runDir, OwnerLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("owner lock: create %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("owner lock: flock %s: %w", path, err)
	}
	return f, nil
}

// ProbeOwnerLock reports the state of runDir's owner lock without ever
// creating the file. It takes a shared lock, so concurrent probes never
// see each other as the owner.
func ProbeOwnerLock(runDir string) LockState {
	f, err := os.OpenFile(filepath.Join(runDir, OwnerLockName), os.O_RDONLY|syscall.O_NOFOLLOW|openNonBlock, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return LockAbsent
	}
	if err != nil {
		return LockUnknown
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return LockHeld
	}
	if err != nil {
		return LockUnknown
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return LockFree
}

// TryLockOwner takes an exclusive lock on an already open owner lock
// file without waiting. It reports false with a nil error when another
// process holds the lock.
func TryLockOwner(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("owner lock: flock %s: %w", f.Name(), err)
	}
	return true, nil
}
