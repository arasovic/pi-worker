//go:build !windows

package background

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// === FakeSnapshotFile — scripted snapshotWriteFile =============================

// FakeSnapshotFile implements snapshotWriteFile with configurable Write, Sync,
// Close, Chmod, and Name behaviour.  It buffers all bytes internally (no
// external buffer needed).  On its second Close call it panics, exposing any
// accidental double-close in the production path.
type FakeSnapshotFile struct {
	writeBuf      []byte
	writeError    error
	shortWriteCap int  // > 0 means "only accept this many bytes → illusion of short write"
	closed        bool // panics on second Close
	syncError     error
	closeError    error
	chmodError    error
	name          string
}

func (f *FakeSnapshotFile) Write(p []byte) (int, error) {
	if f.shortWriteCap > 0 && len(p) >= f.shortWriteCap {
		f.writeBuf = append(f.writeBuf, p[:f.shortWriteCap]...)
		return f.shortWriteCap, nil // short-write illusion: n < len(p), err == nil
	}
	if f.writeError != nil {
		f.writeBuf = append(f.writeBuf, p...)
		return len(p), f.writeError
	}
	f.writeBuf = append(f.writeBuf, p...)
	return len(p), nil
}

func (f *FakeSnapshotFile) Sync() error { return f.syncError }
func (f *FakeSnapshotFile) Close() error {
	if f.closed {
		panic("double-close")
	}
	f.closed = true
	return f.closeError
}
func (f *FakeSnapshotFile) Chmod(fs.FileMode) error { return f.chmodError }
func (f *FakeSnapshotFile) Name() string            { return f.name }

// WrittenLen reports how many bytes were written.
func (f *FakeSnapshotFile) WrittenLen() int { return len(f.writeBuf) }

// === helpers ================================================================

// restoreSeamsForCreate captures the current values of every seam used by
// store.Create and restores them in a t.Cleanup callback.
func restoreSeamsForCreate(t *testing.T) {
	t.Helper()
	origOpenSnapshot := openSnapshotForCreate
	origCreateTemp := createSnapshotTemp
	origRename := renameSnapshot
	origRemove := removeSnapshotForCleanup
	origOpenDir := openDirectoryForSync
	t.Cleanup(func() {
		openSnapshotForCreate = origOpenSnapshot
		createSnapshotTemp = origCreateTemp
		renameSnapshot = origRename
		removeSnapshotForCleanup = origRemove
		openDirectoryForSync = origOpenDir
	})
}

// mustEncode is a convenience for test construction — panic if encode fails.
func mustEncode(s Snapshot) []byte {
	b, err := encodeSnapshot(s)
	if err != nil {
		panic("encodeSnapshot failed: " + err.Error())
	}
	return b
}

// === table-of-failure tests ---------------------------------------------------

// ---- write error ------------------------------------------------------------

func TestStoreCreate_WriteErrorRemovesRunDir(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	snap := buildValidSnapshot(t)
	runDir := filepath.Join(root, snap.RunID)

	mock := &FakeSnapshotFile{writeError: errors.New("write boom"), name: "mock"}

	restoreSeamsForCreate(t)
	openSnapshotForCreate = func(string) (snapshotWriteFile, error) {
		return mock, nil
	}

	err = store.Create(snap)
	if err == nil {
		t.Fatal("expected error from WriteError")
	}
	if !strings.Contains(err.Error(), "write boom") {
		t.Errorf("error=%q; want substring \"write boom\"", err)
	}
	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Error("run dir was not removed after write error")
	}
}

// ---- short write ------------------------------------------------------------

func TestStoreCreate_ShortWriteReturnsIoErrAndRemovesRunDir(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	snap := buildValidSnapshot(t)
	runDir := filepath.Join(root, snap.RunID)

	encoded := mustEncode(snap)
	mock := &FakeSnapshotFile{shortWriteCap: len(encoded) / 2, name: "mock"}

	restoreSeamsForCreate(t)
	openSnapshotForCreate = func(string) (snapshotWriteFile, error) {
		return mock, nil
	}

	err = store.Create(snap)
	if err == nil {
		t.Fatal("expected error from short write")
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("error=%q; want wrapping io.ErrShortWrite", err)
	}
	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Error("run dir was not removed after short write")
	}
}

// ---- sync error -------------------------------------------------------------

func TestStoreCreate_SyncErrorReturnsSyncMessageAndRemovesRunDir(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	snap := buildValidSnapshot(t)
	runDir := filepath.Join(root, snap.RunID)

	mock := &FakeSnapshotFile{syncError: errors.New("sync bomb"), name: "mock"}

	restoreSeamsForCreate(t)
	openSnapshotForCreate = func(string) (snapshotWriteFile, error) {
		return mock, nil
	}

	err = store.Create(snap)
	if err == nil {
		t.Fatal("expected error from SyncError")
	}
	if !strings.Contains(err.Error(), "sync bomb") {
		t.Errorf("error=%q; want substring \"sync bomb\"", err)
	}
	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Error("run dir was not removed after sync error")
	}
}

// ---- close error ------------------------------------------------------------

func TestStoreCreate_CloseErrorReturnsCloseMessageAndRemovesRunDir(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	snap := buildValidSnapshot(t)
	runDir := filepath.Join(root, snap.RunID)

	mock := &FakeSnapshotFile{closeError: errors.New("close fail"), name: "mock"}

	restoreSeamsForCreate(t)
	openSnapshotForCreate = func(string) (snapshotWriteFile, error) {
		return mock, nil
	}

	err = store.Create(snap)
	if err == nil {
		t.Fatal("expected error from CloseError")
	}
	if !strings.Contains(err.Error(), "close fail") {
		t.Errorf("error=%q; want substring \"close fail\"", err)
	}
	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Error("run dir was not removed after close error")
	}
}

// ---- injected cleanup removal failure joined with primary error --------------

func TestStoreCreate_InjectedCleanupFailureJoinedWithPrimaryError(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	snap := buildValidSnapshot(t)
	expectedSnapPath := filepath.Join(root, snap.RunID, "snapshot.json")

	cleanupFail := errors.New("cleanup boom")
	removedPaths := make(map[string]bool)

	restoreSeamsForCreate(t)
	openSnapshotForCreate = func(string) (snapshotWriteFile, error) {
		return &FakeSnapshotFile{syncError: errors.New("sync fail"), name: "mock"}, nil
	}
	removeSnapshotForCleanup = func(path string) error {
		removedPaths[path] = true
		return cleanupFail
	}

	err = store.Create(snap)
	if err == nil {
		t.Fatal("expected error")
	}
	errStr := err.Error()

	// Both the primary error AND the cleanup removal failure appear.
	if !strings.Contains(errStr, "sync fail") {
		t.Errorf("missing primary error message: %q", errStr)
	}
	if !strings.Contains(errStr, "cleanup boom") {
		t.Errorf("missing cleanup failure message: %q", errStr)
	}

	// Verify cleanup targeted both paths.
	if !removedPaths[expectedSnapPath] {
		t.Error("snapPath was not cleaned")
	}
	runDir := filepath.Join(root, snap.RunID)
	if !removedPaths[runDir] {
		t.Error("runDir was not cleaned")
	}
}

// === fakeDirectorySyncHandle — scripted directorySyncHandle ==================

type fakeDirectorySyncHandle struct {
	syncErr error
}

func (f *fakeDirectorySyncHandle) Sync() error  { return f.syncErr }
func (f *fakeDirectorySyncHandle) Close() error { return nil }

// ---- injected directory-sync failure after file close ------------------------

func TestStoreCreate_DirSyncAfterCloseFailsRemovesRunDirNoSnapshotRemains(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	snap := buildValidSnapshot(t)
	runDir := filepath.Join(root, snap.RunID)
	snapPath := filepath.Join(runDir, "snapshot.json")

	dirSyncFail := errors.New("parent sync fail")

	restoreSeamsForCreate(t)
	origOpenDir := openDirectoryForSync
	callCount := 0
	openDirectoryForSync = func(path string) (directorySyncHandle, error) {
		if path == runDir && callCount == 0 {
			callCount++
			return &fakeDirectorySyncHandle{syncErr: dirSyncFail}, nil
		}
		callCount++
		return origOpenDir(path)
	}

	err = store.Create(snap)
	if err == nil {
		t.Fatal("expected error from DirSyncAfterClose")
	}
	if !strings.Contains(err.Error(), "parent sync fail") {
		t.Errorf("error=%q; want \"parent sync fail\"", err)
	}

	// No snapshot or temp file remains.
	if _, serr := os.Stat(snapPath); !os.IsNotExist(serr) {
		t.Error("snapshot file still exists after dir sync failure")
	}
	// Run directory should also be removed.
	if _, serr := os.Stat(runDir); !os.IsNotExist(serr) {
		t.Error("run dir still exists after dir sync failure")
	}
}

// ---- root stays intact, unrelated paths untouched ----------------------------

func TestStoreCreate_FailureDoesNotRemoveRootOrUnrelatedPaths(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	snap := buildValidSnapshot(t)

	unrelated := filepath.Join(root, "unrelated-"+t.Name())
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatalf("create unrelated dir: %v", err)
	}

	restoreSeamsForCreate(t)
	openSnapshotForCreate = func(string) (snapshotWriteFile, error) {
		return &FakeSnapshotFile{writeError: errors.New("fail"), name: "mock"}, nil
	}

	err = store.Create(snap)
	if err == nil {
		t.Fatal("expected error")
	}

	// Root directory must survive.
	fi, rerr := os.Stat(root)
	if rerr != nil || !fi.IsDir() {
		t.Error("store root was unexpectedly removed")
	}

	// Unrelated path must survive.
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated path was removed: %v", err)
	}
}
