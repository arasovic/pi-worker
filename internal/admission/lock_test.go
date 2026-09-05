//go:build darwin || linux

package admission

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockAndUnlockAcquiresAndReleases(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockState(root)
	if err != nil {
		t.Fatalf("lockState() error: %v", err)
	}
	unlock()
}

func TestLockFileCreatedWithOwnerOnlyPermissions(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockState(root)
	if err != nil {
		t.Fatalf("lockState() error: %v", err)
	}
	defer unlock()

	lockPath := filepath.Join(root, "queue.lock")
	fi, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat(queue.lock): %v", err)
	}
	perm := fi.Mode().Perm()
	if perm&0o077 != 0 {
		t.Errorf("lock file mode %o: group and other bits must be unset", perm)
	}
}

func TestRootDirectoryCreatedWithOwnerOnlyPermissions(t *testing.T) {
	root := t.TempDir()
	// Remove the directory so lockState must create it.
	os.RemoveAll(root)
	unlock, err := lockState(root)
	if err != nil {
		t.Fatalf("lockState() error: %v", err)
	}
	defer unlock()

	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("Stat(root): %v", err)
	}
	perm := fi.Mode().Perm()
	if perm&0o077 != 0 {
		t.Errorf("root directory mode %o: group and other bits must be unset", perm)
	}
}

func TestUnlockIsIdempotent(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockState(root)
	if err != nil {
		t.Fatalf("lockState() error: %v", err)
	}
	unlock()
	unlock() // second call must not panic
	unlock() // third call
}

func TestLockBlocksConcurrentAcquire(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockState(root)
	if err != nil {
		t.Fatalf("lockState() error: %v", err)
	}

	// aboutToLock signals that the goroutine is about to call lockState.
	// result carries the goroutine's outcome after lockState returns.
	aboutToLock := make(chan struct{})
	type result struct {
		unlock func()
		err    error
	}
	resultCh := make(chan result)

	go func() {
		close(aboutToLock)
		u, err := lockState(root)
		resultCh <- result{unlock: u, err: err}
	}()

	// Wait until the goroutine is about to call lockState.
	<-aboutToLock

	// Give a small window so the goroutine starts blocking.
	time.Sleep(50 * time.Millisecond)

	// Before we unlock, any result arriving is an early failure
	// (the lock was not truly exclusive).
	early := false
	select {
	case r := <-resultCh:
		// Clean up before recording failure.
		if r.err == nil {
			r.unlock()
		}
		early = true
		// Fall through to release first lock and join goroutine below.
	case <-time.After(100 * time.Millisecond):
		// Expected: goroutine is still blocked.
	}

	// Release the first lock; the goroutine must acquire within 1 s.
	unlock()

	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("goroutine lockState() error: %v", r.err)
		}
		r.unlock()
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for concurrent acquire after unlock")
	}

	if early {
		t.Fatal("lock was acquired concurrently while first lock was held — exclusivity violated")
	}
}

func TestRejectsSymlinkedLockFile(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "queue.lock")

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := lockState(root)
	if err == nil {
		t.Fatal("lockState() = nil, want symlink refusal error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("error = %v, want message about symbolic link", err)
	}
	// Symlink is untouched.
	fi, lerr := os.Lstat(lockPath)
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink changed: %v %v", lerr, fi.Mode())
	}
}

func TestRejectsDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "queue.lock")

	missing := filepath.Join(t.TempDir(), "no", "such", "file")
	if err := os.Symlink(missing, lockPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := lockState(root)
	if err == nil {
		t.Fatal("lockState() = nil, want symlink refusal error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("error = %v, want message about symbolic link", err)
	}
	// Dangling link is untouched.
	fi, lerr := os.Lstat(lockPath)
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("dangling link changed: %v %v", lerr, fi.Mode())
	}
}

func TestLockNonExistentParentIsCreated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deep", "nested", "root")
	unlock, err := lockState(root)
	if err != nil {
		t.Fatalf("lockState() error: %v", err)
	}
	defer unlock()

	lockPath := filepath.Join(root, "queue.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
}

func TestErrUnsupportedNotReturnedOnUnix(t *testing.T) {
	root := t.TempDir()
	_, err := lockState(root)
	if errors.Is(err, ErrUnsupported) {
		t.Error("lockState() returned ErrUnsupported on Unix")
	}
}
