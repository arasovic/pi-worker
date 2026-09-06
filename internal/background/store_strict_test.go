package background

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/run"
)

// === Helpers ===========================================================================

// buildValidSnapshot returns a valid baseline Snapshot at fixtureTime.
func buildValidSnapshot(t *testing.T) Snapshot {
	t.Helper()
	snap, err := NewSnapshot(
		makeRunID(fixtureTime),
		fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil,
	)
	if err != nil {
		t.Fatalf("build valid snapshot: %v", err)
	}
	return snap
}

// setUpRunDir creates a proper run directory and writes content into
// <root>/<runID>/snapshot.json. Returns the canonical store so Load can be exercised.
func setUpRunDir(t *testing.T, root, runID string, content []byte) *Store {
	t.Helper()
	runDir := filepath.Join(root, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	snapPath := snapshotPath(root, runID)
	if err := os.WriteFile(snapPath, content, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// buildGoodMap returns a minimal valid snapshot as a map for JSON round-tripping.
func buildGoodMap(runID string) map[string]any {
	return map[string]any{
		"schemaVersion": float64(1),
		"runId":         runID,
		"acceptedAt":    fixtureTime.UTC().Format(time.RFC3339Nano),
		"updatedAt":     fixtureTime.UTC().Format(time.RFC3339Nano),
		"state":         "accepted",
		"terminal":      false,
		"workspace":     "/ws",
		"supervisor":    map[string]any{"pid": float64(1), "createTime": float64(100)},
		"workers":       []map[string]any{},
	}
}

// buildTwoDocuments returns two valid JSON snapshots separated by a newline
// so that the standard decoder reads the first document, then a second
// dec.Decode(&extra) call sees more content (not io.EOF — the trailing-data
// guard fires).
func buildTwoDocuments(id string) []byte {
	b1, _ := json.Marshal(buildGoodMap(id))
	b2, _ := json.Marshal(buildGoodMap(id))
	return append(append(b1, '\n'), b2...)
}

// === RunID rejection (before any filesystem touch) =====================================

// Load rejects empty run ID and other malformed IDs because runlog.ParseRunID fails.
func TestLoad_RejectsMalformedRunIDs(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	for _, bad := range []string{
		"",              // zero-length
		"not-a-run-id",  // not parseable as timestamp run ID
		"../etc/passwd", // path traversal attempt
	} {
		_, err := store.Load(bad)
		if err == nil {
			t.Fatalf("Load(%q): expected failure", bad)
		}
	}
}

// === fs.ErrNotExist preservation =======================================================

// Load on a missing run directory returns an error wrapping fs.ErrNotExist.
func TestLoad_MissingRunDirectory_PreservesErrNotExist(t *testing.T) {
	root := t.TempDir()
	store, _ := NewStore(root)

	_, err := store.Load(makeRunID(fixtureTime))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("wanted fs.ErrNotExist, got: %v", err)
	}
}

// Load on an existing run directory but missing snapshot.json preserves fs.ErrNotExist.
func TestLoad_MissingSnapshot_PreservesErrNotExist(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	runDir := filepath.Join(root, runID)
	os.Mkdir(runDir, 0o700) // present directory
	store, _ := NewStore(root)

	_, err := store.Load(runID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("wanted fs.ErrNotExist, got: %v", err)
	}
}

// === Symlink run directory -----------------------------------------------------------

// Load refuses a symlink where the run directory should be.
func TestLoad_RunDirSymlink_Rejected(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	linkPath := filepath.Join(root, runID)
	realPath := filepath.Join(root, "real-target")

	// Create the real directory as symlink target.
	if err := os.Mkdir(realPath, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	err := os.Symlink(realPath, linkPath)
	if serr, ok := err.(*os.PathError); ok {
		if strings.HasPrefix(serr.Err.Error(), "operation not permitted") ||
			strings.HasPrefix(serr.Err.Error(), "Operation not permitted") {
			t.Skipf("symlinks not supported on this platform: %v", serr.Err)
		}
	} else if err != nil {
		t.Fatalf("symlink: %v", err)
	}

	store, _ := NewStore(root)
	_, err = store.Load(runID)
	if err == nil {
		t.Fatal("expected error for symlinked run dir")
	}
	if !strings.Contains(err.Error(), "refusing to read through a symbolic link") {
		t.Errorf("want symlink refusal, got: %v", err)
	}

	// Verify target directory was NOT modified.
	if fi, _ := os.Stat(realPath); fi == nil || !fi.IsDir() {
		t.Error("target was unexpectedly removed or changed")
	}
}

// Load refuses a regular file where the run directory should be.
func TestLoad_RunDirIsFile_Rejected(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	dirPath := filepath.Join(root, runID)
	os.WriteFile(dirPath, []byte("not a dir"), 0o600)

	store, _ := NewStore(root)
	_, err := store.Load(runID)
	if err == nil {
		t.Fatal("expected error for file-as-run-dir")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("want 'not a directory', got: %v", err)
	}
}

// === snapshot.json symlink handling ---------------------------------------------------

// Load rejects snapshot.json when it is a present symlink.
func TestLoad_SnapshotPresentSymlink_Rejected(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	target := filepath.Join(root, "hidden_target.json")
	os.WriteFile(target, []byte(`{"schemaVersion":1}`), 0o600)
	setUpRunDir(t, root, runID, nil) // placeholder file for the run dir
	// Replace snapshot.json with a symlink to target.
	snapPath := snapshotPath(root, runID)
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

	store, _ := NewStore(root)
	_, err = store.Load(runID)
	if err == nil {
		t.Fatal("expected error for symlinked snapshot")
	}
	if !strings.Contains(err.Error(), "refusing to read through a symbolic link") {
		t.Errorf("want symlink refusal, got: %v", err)
	}

	// Original target file must still exist unchanged.
	data, err := os.ReadFile(target)
	if err != nil || string(data) != `{"schemaVersion":1}` {
		t.Error("symlink target was changed or removed")
	}
}

// Load rejects snapshot.json when it is a dangling symlink.
func TestLoad_SnapshotDanglingSymlink_Rejected(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	snapPath := snapshotPath(root, runID)

	if err := os.MkdirAll(filepath.Dir(snapPath), 0o700); err != nil {
		t.Fatalf("mkdir parent of snapshot path: %v", err)
	}

	err := os.Symlink("/nonexistent/path/nowhere", snapPath)
	if serr, ok := err.(*os.PathError); ok {
		if strings.HasPrefix(serr.Err.Error(), "operation not permitted") ||
			strings.HasPrefix(serr.Err.Error(), "Operation not permitted") {
			t.Skipf("symlinks not supported: %v", serr.Err)
		}
	} else if err != nil {
		t.Fatalf("symlink: %v", err)
	}

	store, _ := NewStore(root)
	_, err = store.Load(runID)
	if err == nil {
		t.Fatal("expected error for dangling symlink snapshot")
	}
	if !strings.Contains(err.Error(), "refusing to read through a symbolic link") {
		t.Errorf("want symlink refusal, got: %v", err)
	}
}

// Load rejects snapshot.json when it is a directory (not a regular file).
func TestLoad_SnapshotIsDirectory_Rejected(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	snapPath := snapshotPath(root, runID)

	if err := os.MkdirAll(filepath.Dir(snapPath), 0o700); err != nil {
		t.Fatalf("mkdir parent of snapshot path: %v", err)
	}
	// snapPath is a directory, not a file
	if err := os.Mkdir(snapPath, 0o700); err != nil {
		t.Fatalf("mkdir snapshot path as dir: %v", err)
	}

	store, _ := NewStore(root)
	_, err := store.Load(runID)
	if err == nil {
		t.Fatal("expected error for directory-as-snapshot")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("want 'not a regular file', got: %v", err)
	}
}

// === Raw JSON table tests ============================================================

type rawJSONTestCase struct {
	name    string
	content func(runID string) []byte
	wantErr string // substring required in error
}

func loadRaw(t *testing.T, root, runID string, rawContent []byte) (*Store, Snapshot, error) {
	t.Helper()
	store := setUpRunDir(t, root, runID, rawContent)
	snap, err := store.Load(runID)
	return store, snap, err
}

// TestLoad_RawJSONTable_Failures covers every JSON-level strictness gate:
//
//	malformed document, unknown fields, trailing second document,
//	decoded runId mismatch, and a structurally invalid Snapshot
//	whose schemaVersion and runId are present but required fields are missing.
func TestLoad_RawJSONTable_Failures(t *testing.T) {
	runID := makeRunID(fixtureTime)
	base := buildGoodMap(runID)

	tests := []rawJSONTestCase{
		{
			name: "malformed_document",
			content: func(_ string) []byte {
				return []byte(`{broken json`)
			},
			wantErr: "decode",
		},
		{
			name: "unknown_field",
			content: func(_ string) []byte {
				m := make(map[string]any)
				for k, v := range base {
					m[k] = v
				}
				m["bogusField"] = "x"
				b, _ := json.Marshal(m)
				return b
			},
			wantErr: "unknown field",
		},
		{
			name: "trailing_second_document",
			content: func(id string) []byte {
				return buildTwoDocuments(id)
			},
			wantErr: "trailing data",
		},
		{
			name: "decoded_runid_mismatch",
			content: func(_ string) []byte {
				m := buildGoodMap("some-different-run-id")
				b, _ := json.Marshal(m)
				return b
			},
			wantErr: "does not match requested",
		},
		{
			name: "structurally_invalid_snapshot",
			content: func(_ string) []byte {
				// Only schemaVersion + runId → missing workspace, supervisor, tasks ⇒ Validate fails.
				return []byte(`{"schemaVersion":1,"runId":"` + runID + `"}`)
			},
			wantErr: "validate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.content(runID)
			_, _, err := loadRaw(t, t.TempDir(), runID, raw)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("want error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

// === Create refusals =================================================================

// Create refuses a root symlink without modifying its target.
func TestCreate_RefusesRootSymlink_NoMutation(t *testing.T) {
	parent := t.TempDir()
	rootLink := filepath.Join(parent, "background")
	realDir := filepath.Join(parent, "real_root")
	os.Mkdir(realDir, 0o700)

	err := os.Symlink(realDir, rootLink)
	if serr, ok := err.(*os.PathError); ok {
		if strings.HasPrefix(serr.Err.Error(), "operation not permitted") ||
			strings.HasPrefix(serr.Err.Error(), "Operation not permitted") {
			t.Skipf("symlinks not supported: %v", serr.Err)
		}
	} else if err != nil {
		t.Fatalf("symlink: %v", err)
	}

	store, err := NewStore(rootLink)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	snap := buildValidSnapshot(t)
	err = store.Create(snap)
	if err == nil {
		t.Fatal("expected error for symlinked root")
	}
	if !strings.Contains(err.Error(), "refusing to write through a symbolic link") {
		t.Errorf("want symlink refusal, got: %v", err)
	}

	// Target directory must remain unchanged (empty, untouched).
	entries, _ := os.ReadDir(realDir)
	if len(entries) != 0 {
		t.Error("symlink target was mutated despite refusal")
	}
}

// Create refuses when the run directory already exists and does not mutate anything.
func TestCreate_RefusesExistingRunDir_NoMutation(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	runDir := filepath.Join(root, runID)

	// Pre-create run directory and a marker file.
	os.Mkdir(runDir, 0o700)
	os.WriteFile(filepath.Join(runDir, "marker.txt"), []byte("existing"), 0o600)

	store, _ := NewStore(root)
	snap := buildValidSnapshot(t)

	err := store.Create(snap)
	if err == nil {
		t.Fatal("expected error for pre-existing run dir")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("want 'already exists', got: %v", err)
	}

	// Marker file must be untouched.
	data, err := os.ReadFile(filepath.Join(runDir, "marker.txt"))
	if err != nil || string(data) != "existing" {
		t.Error("run dir contents were mutated during failed Create")
	}
}

// === Replace refusals ==============================================================

// Replace refuses a symlinked snapshot without changing its target.
func TestReplace_RefusesSymlinkSnapshot_NoMutation(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	target := filepath.Join(root, "evil_target.json")
	os.WriteFile(target, []byte(`{"schemaVersion":1}`), 0o600)
	store := setUpRunDir(t, root, runID, nil)

	snapPath := snapshotPath(root, runID)
	os.Remove(snapPath)

	symlinkErr := os.Symlink(target, snapPath)
	if serr, ok := symlinkErr.(*os.PathError); ok {
		if strings.HasPrefix(serr.Err.Error(), "operation not permitted") ||
			strings.HasPrefix(serr.Err.Error(), "Operation not permitted") {
			t.Skipf("symlinks not supported: %v", serr.Err)
		}
	} else if symlinkErr != nil {
		t.Fatalf("symlink: %v", symlinkErr)
	}

	newSnap := buildValidSnapshot(t)
	err := store.Replace(newSnap)
	if err == nil {
		t.Fatal("expected error for symlinked snapshot in Replace")
	}
	if !strings.Contains(err.Error(), "refusing to replace through a symbolic link") {
		t.Errorf("want symlink refusal, got: %v", err)
	}

	// Target unchanged.
	data, readErr := os.ReadFile(target)
	if readErr != nil || string(data) != `{"schemaVersion":1}` {
		t.Error("symlink target was changed")
	}
}

// Replace refuses when the snapshot path is a directory (not a regular file).
func TestReplace_DirectorySnapshot_Refused(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	snapPath := snapshotPath(root, runID)

	if err := os.MkdirAll(filepath.Dir(snapPath), 0o700); err != nil {
		t.Fatalf("mkdir parent of snapshot path: %v", err)
	}
	// special entry: directory instead of file
	if err := os.Mkdir(snapPath, 0o700); err != nil {
		t.Fatalf("mkdir snapshot path as dir: %v", err)
	}

	store, _ := NewStore(root)
	newSnap := buildValidSnapshot(t)
	err := store.Replace(newSnap)
	if err == nil {
		t.Fatal("expected error for directory as snapshot")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("want 'not a regular file', got: %v", err)
	}
}

// === Invalid snapshot prevents filesystem mutation ================================

// Invalid snapshot passed to Create fails before touching filesystem.
func TestCreate_InvalidSnapshot_NoFilesystemChange(t *testing.T) {
	root := t.TempDir()
	store, _ := NewStore(root)

	// Build a snapshot that passes initial construction but has a state value
	// that Snapshot.Validate rejects.
	snap := buildValidSnapshot(t)
	snap.State = "corrupt_state"

	err := store.Create(snap)
	if err == nil {
		t.Fatal("expected error for invalid snapshot in Create")
	}
	if !strings.Contains(err.Error(), "validate") {
		t.Errorf("want 'validate' error, got: %v", err)
	}

	// Nothing should have been created under root.
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Error("filesystem was mutated despite invalid snapshot in Create")
	}
}

// Invalid snapshot passed to Replace fails without creating temp files or mutating originals.
func TestReplace_InvalidSnapshot_NoFilesystemChange(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	store := setUpRunDir(t, root, runID, nil)

	// Same corrupt-state approach as Create.
	snap := buildValidSnapshot(t)
	snap.State = "corrupt_state"

	err := store.Replace(snap)
	if err == nil {
		t.Fatal("expected error for invalid snapshot in Replace")
	}
	if !strings.Contains(err.Error(), "validate") {
		t.Errorf("want 'validate' error, got: %v", err)
	}

	// Original snapshot.json must still exist; no temp files left behind.
	origDir := filepath.Join(root, runID)
	entries, _ := os.ReadDir(origDir)
	if len(entries) != 1 || entries[0].Name() != "snapshot.json" {
		t.Error("unexpected extra files in run directory after failed Replace")
	}
	if _, statErr := os.Lstat(snapshotPath(root, runID)); statErr != nil {
		t.Error("original snapshot.json was removed")
	}
}
