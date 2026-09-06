package background

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helperRunDir builds <root>/<runID>/snapshot.json as a regular file and returns store + snapPath.
func helperRunDir(t *testing.T, root, runID string, payload []byte) (*Store, string) {
	t.Helper()
	runDir := filepath.Join(root, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", runDir, err)
	}
	snapPath := snapshotPath(root, runID)
	if err := os.WriteFile(snapPath, payload, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, snapPath
}

func helperValidPayload(t *testing.T, runID string) []byte {
	t.Helper()
	snap := buildValidSnapshot(t)
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	b = append(b, '\n')
	return b
}

// --- Remove tests ===================================================================

// Remove on a missing run directory preserves fs.ErrNotExist.
func TestRemove_MissingRunDirectory_PreservesErrNotExist(t *testing.T) {
	root := t.TempDir()
	store, _ := NewStore(root)
	runID := makeRunID(fixtureTime)

	err := store.Remove(runID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("wanted fs.ErrNotExist, got: %v", err)
	}
}

// Remove on an existing run directory with a missing snapshot.json preserves fs.ErrNotExist.
func TestRemove_MissingSnapshot_PreservesErrNotExist(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	store, _ := NewStore(root)

	// Create only the run directory; snapshot.json stays absent.
	runDir := filepath.Join(root, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := store.Remove(runID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("wanted fs.ErrNotExist, got: %v", err)
	}
}

// Remove rejects a symlinked run directory without modifying its target.
func TestRemove_SymlinkedRunDir_Rejected(t *testing.T) {
	parent := t.TempDir()
	realDir := filepath.Join(parent, "real_target")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real dir: %v", err)
	}
	runID := makeRunID(fixtureTime)
	linkPath := filepath.Join(parent, runID)
	err := os.Symlink(realDir, linkPath)
	if serr, ok := err.(*os.PathError); ok {
		if strings.HasPrefix(serr.Err.Error(), "operation not permitted") ||
			strings.HasPrefix(serr.Err.Error(), "Operation not permitted") {
			t.Skipf("symlinks not supported: %v", serr.Err)
		}
	} else if err != nil {
		t.Fatalf("symlink: %v", err)
	}

	store, _ := NewStore(parent)
	err = store.Remove(runID)
	if err == nil {
		t.Fatal("expected rejection for symlinked run dir")
	}
	if !strings.Contains(err.Error(), "refusing to remove through a symbolic link") {
		t.Errorf("want symlink-refusal error, got: %v", err)
	}
	// Target untouched.
	if _, statErr := os.Stat(realDir); statErr != nil {
		t.Error("symlink target was unexpectedly removed")
	}
}

// Remove rejects snapshot.json when it is a present symlink without changing target bytes.
func TestRemove_SnapshotSymlink_Rejected(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	target := filepath.Join(root, "evil_payload.json")
	payload := []byte(`{"schemaVersion":1}`)
	os.WriteFile(target, payload, 0o600)
	store, snapPath := helperRunDir(t, root, runID, nil)
	os.Remove(snapPath)

	err := os.Symlink(target, snapPath)
	if serr, ok := err.(*os.PathError); ok {
		if strings.HasPrefix(serr.Err.Error(), "operation not permitted") ||
			strings.HasPrefix(serr.Err.Error(), "Operation not permitted") {
			t.Skipf("symlinks not supported: %v", serr.Err)
		}
	} else if err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err = store.Remove(runID)
	if err == nil {
		t.Fatal("expected rejection for symlinked snapshot.json")
	}
	if !strings.Contains(err.Error(), "refusing to remove through a symbolic link") {
		t.Errorf("want symlink-refusal error, got: %v", err)
	}
	// Target still intact.
	data, err2 := os.ReadFile(target)
	if err2 != nil || string(data) != string(payload) {
		t.Error("symlink target was mutated or removed")
	}
}

// Remove rejects snapshot.json when it is a directory (not a regular file) without touching it.
func TestRemove_DirectorySnapshot_Rejected(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	store, snapPath := helperRunDir(t, root, runID, nil)
	os.Remove(snapPath)
	if err := os.Mkdir(snapPath, 0o700); err != nil {
		t.Fatalf("mkdir snapshot path: %v", err)
	}

	err := store.Remove(runID)
	if err == nil {
		t.Fatal("expected rejection for directory-as-snapshot")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("want 'not a regular file', got: %v", err)
	}
}

// Replace on a missing run directory preserves fs.ErrNotExist.
func TestReplace_MissingRunDirectory_PreservesErrNotExist(t *testing.T) {
	root := t.TempDir()
	store, _ := NewStore(root)
	snap := buildValidSnapshot(t)

	err := store.Replace(snap)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("wanted fs.ErrNotExist, got: %v", err)
	}
}
