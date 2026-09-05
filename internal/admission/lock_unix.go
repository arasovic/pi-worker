//go:build darwin || linux

package admission

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// lockState obtains an exclusive advisory lock on root/queue.lock and
// returns an idempotent unlock function. The root directory is created
// and tightened to 0700 where supported. The lock file is created with
// 0600 permissions. A symlink at the final queue.lock path — whether
// dangling or pointing to a real file — is rejected. The returned
// unlock function releases the lock, closes the file descriptor, and
// is safe to call more than once.
func lockState(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("lock state: create root %s: %w", root, err)
	}
	// Tighten the root directory to owner-only where supported.
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("lock state: set root permissions: %w", err)
	}

	lockPath := filepath.Join(root, "queue.lock")

	// Reject any symbolic link before opening — covers dangling links
	// because Lstat succeeds on them.
	fi, err := os.Lstat(lockPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("lock state: inspect %s: %w", lockPath, err)
	}
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("lock state: %s is a symbolic link", lockPath)
	}

	// Open (or create) the lock file without following symlinks.
	flags := os.O_CREATE | os.O_RDWR | syscall.O_NOFOLLOW
	f, err := os.OpenFile(lockPath, flags, 0o600)
	if err != nil {
		// ELOOP surfaces when O_NOFOLLOW encounters a symlink that
		// appeared between the Lstat above and the open. Treat it
		// the same as the explicit symlink check.
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("lock state: %s is a symbolic link", lockPath)
		}
		return nil, fmt.Errorf("lock state: open %s: %w", lockPath, err)
	}
	// Tighten the lock file to owner-only where supported.
	// Permissions are already 0600 from the create, but an existing
	// file might have been left with different bits.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock state: set lock permissions: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock state: flock %s: %w", lockPath, err)
	}

	var once sync.Once
	unlock := func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}
	return unlock, nil
}
