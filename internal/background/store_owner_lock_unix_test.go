//go:build darwin || linux

package background

import (
	"path/filepath"
	"testing"

	"github.com/arasovic/pi-worker/internal/runlog"
)

// TestStoreCreate_TakesTheOwnerLockBeforeTheSnapshot requires that the run
// directory's owner lock is already held at the moment snapshot.json is
// created, so no reader can ever see a snapshot beside a free lock, and that
// the returned file is that lock: closing it frees the lock.
func TestStoreCreate_TakesTheOwnerLockBeforeTheSnapshot(t *testing.T) {
	root := t.TempDir()
	snap := buildValidSnapshot(t)
	runDir := filepath.Join(root, snap.RunID)

	original := openSnapshotForCreate
	t.Cleanup(func() { openSnapshotForCreate = original })
	var atOpen runlog.LockState = -1
	openSnapshotForCreate = func(path string) (snapshotWriteFile, error) {
		atOpen = runlog.ProbeOwnerLock(runDir)
		return original(path)
	}

	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	lock, err := store.Create(snap)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if atOpen != runlog.LockHeld {
		t.Fatalf("owner lock when the snapshot was opened = %v, want held (%v)", atOpen, runlog.LockHeld)
	}
	if got := runlog.ProbeOwnerLock(runDir); got != runlog.LockHeld {
		t.Fatalf("owner lock after Create = %v, want held", got)
	}
	lock.Close()
	if got := runlog.ProbeOwnerLock(runDir); got != runlog.LockFree {
		t.Fatalf("owner lock after closing the returned file = %v, want free", got)
	}
}
