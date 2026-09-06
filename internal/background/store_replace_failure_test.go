//go:build !windows

package background

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// closeHookSnapshotFile wraps any snapshotWriteFile and runs one callback
// only after the underlying file's Close returns.
type closeHookSnapshotFile struct {
	inner   snapshotWriteFile
	onClose func()
}

func (h *closeHookSnapshotFile) Write(p []byte) (int, error) { return h.inner.Write(p) }
func (h *closeHookSnapshotFile) Sync() error                 { return h.inner.Sync() }
func (h *closeHookSnapshotFile) Chmod(m os.FileMode) error   { return h.inner.Chmod(m) }
func (h *closeHookSnapshotFile) Name() string                { return h.inner.Name() }
func (h *closeHookSnapshotFile) Close() error {
	err := h.inner.Close()
	if h.onClose != nil {
		h.onClose()
	}
	return err
}

// noTempFiles asserts that no entry in dir starts with the given prefix.
func noTempFiles(t *testing.T, dir, prefix string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			t.Errorf("unexpected file starting with %q: %s", prefix, e.Name())
		}
	}
}

// --- Test 1: renameSnapshot fails → old snapshot preserved, no temp ----------

func TestStoreReplace_RenameFailsPreservesOriginalNoTemp(t *testing.T) {
	root := t.TempDir()
	orig := buildValidSnapshot(t)
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(orig); err != nil {
		t.Fatalf("Create: %v", err)
	}

	running := newRunningSnapshot(t, orig)
	runDir := filepath.Join(root, orig.RunID)
	snapPath := filepath.Join(runDir, "snapshot.json")

	restoreSeamsForCreate(t)
	renameFail := errors.New("rename fail")
	renameSnapshot = func(src, dst string) error {
		return renameFail
	}

	err = store.Replace(running)
	if err == nil {
		t.Fatal("expected error from failed rename")
	}
	if !strings.Contains(err.Error(), "rename fail") {
		t.Errorf("error=%q; want \"rename fail\"", err)
	}

	// Load must return the original accepted snapshot, unchanged.
	loaded, err := store.Load(orig.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(loaded, orig) {
		t.Error("Load after failed rename did not return original accepted snapshot")
	}

	// No temp file should remain in the run directory.
	noTempFiles(t, runDir, ".snapshot.json.tmp-")

	// Original snapshot file must still exist and be valid.
	data, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read original snapshot: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Error("original snapshot file corrupted or missing trailing newline")
	}
}

// --- Test 2: first runDir sync after rename fails → new snapshot complete ----

func TestStoreReplace_ReplaceSyncRunDirFailsReturnsErrorNewSnapshotComplete(t *testing.T) {
	root := t.TempDir()
	orig := buildValidSnapshot(t)
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(orig); err != nil {
		t.Fatalf("Create: %v", err)
	}

	running := newRunningSnapshot(t, orig)
	runDir := filepath.Join(root, orig.RunID)

	dirSyncFail := errors.New("parent sync fail")
	restoreSeamsForCreate(t)
	origOpenDir := openDirectoryForSync
	callCount := 0
	openDirectoryForSync = func(path string) (directorySyncHandle, error) {
		if path == runDir && callCount == 0 {
			callCount++
			return &fakeDirectorySyncHandle{syncErr: dirSyncFail}, nil
		}
		return origOpenDir(path)
	}

	err = store.Replace(running)
	if err == nil {
		t.Fatal("expected error from failed runDir sync")
	}
	if !strings.Contains(err.Error(), "parent sync fail") {
		t.Errorf("error=%q; want \"parent sync fail\"", err)
	}

	// Despite the sync error, the new snapshot is on disk.
	// (rename already moved the temp file into place atomically.)
	loaded, err := store.Load(running.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(loaded, running) {
		t.Error("Load after failed post-rename sync did not return the complete new running snapshot")
	}

	// No leftover temp file.
	noTempFiles(t, runDir, ".snapshot.json.tmp-")
}

// TestStoreReplace_RejectsSymlinkDestination re-checks snapPath after the
// temp file is closed but before rename: if an outside entity swapped
// snapshot.json for a symbolic link, Replace must refuse the rename and
// leave both the target file and the symlink intact.
func TestStoreReplace_RejectsSymlinkDestination(t *testing.T) {
	root := t.TempDir()
	orig := buildValidSnapshot(t)
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(orig); err != nil {
		t.Fatalf("Create: %v", err)
	}

	running := newRunningSnapshot(t, orig)
	runDir := filepath.Join(root, orig.RunID)
	snapPath := filepath.Join(runDir, "snapshot.json")

	// Create the outside target file with known bytes.
	targetData := []byte("outside-target")
	targetPath := filepath.Join(t.TempDir(), "symlink-target")
	if err := os.WriteFile(targetPath, targetData, 0o644); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}

	// Capture original createSnapshotTemp so we can compose with it.
	restoreSeamsForCreate(t)

	var symlinkErr error
	origCreateTemp := createSnapshotTemp

	// Override createSnapshotTemp: return a close-hook wrapper whose
	// callback replaces snapshot.json with a symlink pointing at the
	// outside target. If symlinks are not supported (e.g. on CI tmpfs),
	// skip the test gracefully.
	callCount := 0
	createSnapshotTemp = func(dir, pattern string) (snapshotWriteFile, error) {
		// The real temp creator should only be called once per Replace.
		if callCount > 0 {
			return nil, errors.New("unexpected second call to createSnapshotTemp")
		}
		callCount++

		inner, err := origCreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}

		onClose := func() {
			_ = os.Remove(snapPath)
			symlinkErr = os.Symlink(targetPath, snapPath)
			if symlinkErr != nil {
				// Symlink creation unsupported — leave snapshot.json alone.
				data, _ := json.Marshal(orig)
				_ = os.WriteFile(snapPath, append(data, '\n'), 0o600)
			}
		}
		return &closeHookSnapshotFile{inner: inner, onClose: onClose}, nil
	}

	err = store.Replace(running)
	if symlinkErr != nil {
		t.Skipf("symlink unsupported: %v", symlinkErr)
	}
	if err == nil {
		t.Fatal("expected error from symlink destination swap")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("error=%q; want 'symbolic link'", err)
	}

	// Verify the outside target was not mutated.
	targetAfter, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target after Replace: %v", err)
	}
	if !reflect.DeepEqual(targetAfter, targetData) {
		t.Errorf("target bytes changed: %q; want %q", targetAfter, targetData)
	}

	// Snapshot.json must still be a symlink (not replaced with real data).
	info, err := os.Lstat(snapPath)
	if err != nil {
		t.Fatalf("Lstat snapshot.json: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("snapshot.json should still be a symlink after rejected Replace")
	}

	// No temp file should remain.
	noTempFiles(t, runDir, ".snapshot.json.tmp-")
}
