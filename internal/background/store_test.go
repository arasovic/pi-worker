package background

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/run"
)

// newRunningSnapshot builds a valid running-state Snapshot derived from an
// accepted baseline so that every assertion stays tied to real validation
func newRunningSnapshot(t *testing.T, orig Snapshot) Snapshot {
	t.Helper()
	startedAt := orig.AcceptedAt.Add(5 * time.Second)
	now := orig.AcceptedAt.Add(5 * time.Second)

	orig.State = RunRunning
	orig.Terminal = false
	orig.Status = nil
	orig.Outcome = nil
	orig.Result = nil
	orig.UpdatedAt = now

	w := orig.Workers[0]
	w.State = WorkerRunning
	w.StartedAt = &startedAt
	w.FinishedAt = nil
	w.Process = &ProcessIdentity{PID: 42, CreateTime: 100}
	w.Result = nil
	orig.Workers = []WorkerSnapshot{w}

	if err := orig.Validate(); err != nil {
		t.Fatalf("running snapshot failed validation: %v", err)
	}
	return orig
}

// --- Constructor behaviour -----------------------------------------------------

// NewStore rejects empty roots without creating any filesystem content.
func TestNewStore_RejectsEmptyRoot(t *testing.T) {
	_, err := NewStore("")
	if err == nil {
		t.Fatal("expected error for empty root")
	}
}

// NewStore never touches the filesystem — constructing it around a temp
// path must leave that path absent.
func TestNewStore_ExplicitTempRootHasNoSideEffect(t *testing.T) {
	root := filepath.Join(t.TempDir(), "explicit-temp")
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore(%q): %v", root, err)
	}
	if got := store.root; got != root {
		t.Errorf("store.root = %q; want %q", got, root)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Errorf("root was created prematurely (%v)", statErr)
	}
}

// DefaultRoot is exactly UserDir / background.
func TestDefaultRoot_EqualsUserDirBackground(t *testing.T) {
	userDir, err := config.UserDir()
	if err != nil {
		t.Fatalf("config.UserDir: %v", err)
	}
	got, err := DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	want := filepath.Join(userDir, "background")
	if got != want {
		t.Errorf("DefaultRoot() = %q; want %q", got, want)
	}
}

// --- Full lifecycle ------------------------------------------------------------

// Create → Load round-trips via reflect.DeepEqual.
func TestLifecycle_CreateAndLoad(t *testing.T) {
	root := t.TempDir()
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/workspace",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Create(snap); err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := store.Load(snap.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(loaded, snap) {
		t.Error("Load after Create did not return the original snapshot")
	}
}

// Replace swaps in a running snapshot whose updatedAt is later than the
// baseline; subsequent Load matches the replacement verbatim.
func TestLifecycle_Replace(t *testing.T) {
	root := t.TempDir()
	base, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(base); err != nil {
		t.Fatalf("Create: %v", err)
	}

	running := newRunningSnapshot(t, base)
	if err := store.Replace(running); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	after, err := store.Load(running.RunID)
	if err != nil {
		t.Fatalf("Load after Replace: %v", err)
	}
	if !reflect.DeepEqual(after, running) {
		t.Error("snapshot after Replace does not match replacement")
	}
}

// Remove deletes the run directory; follow-up Load preserves fs.ErrNotExist.
func TestLifecycle_Remove(t *testing.T) {
	root := t.TempDir()
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(snap); err != nil {
		t.Fatalf("Create: %v", err)
	}

	runDir := filepath.Join(root, snap.RunID)

	if err := store.Remove(snap.RunID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Fatalf("run directory still exists after Remove (%v)", statErr)
	}

	_, loadErr := store.Load(snap.RunID)
	if !errors.Is(loadErr, os.ErrNotExist) {
		t.Errorf("Load after Remove returned %v; expected fs.ErrNotExist", loadErr)
	}
}

// --- File layout and permissions ------------------------------------------------

// Create writes exactly one compact JSON document followed by a single newline
// at <root>/<runId>/snapshot.json, with strict permissions (off-Windows).
func TestCreate_ProducesExactlyOneJSONDocumentWithPermissions(t *testing.T) {
	root := t.TempDir()
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(snap); err != nil {
		t.Fatalf("Create: %v", err)
	}

	snapPath := filepath.Join(root, snap.RunID, "snapshot.json")
	data, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Must end with exactly one newline.
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("last byte is not \\n (data len=%d)", len(data))
	}

	// Decode stripped content as a single JSON object — mirrors how
	// [Store.Load] validates the document shape.
	truncated := data[:len(data)-1]
	dec := json.NewDecoder(strings.NewReader(string(truncated)))
	dec.DisallowUnknownFields()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("strict decode of stripped content: %v", err)
	}
	var peek any
	if err := dec.Decode(&peek); err == nil {
		t.Fatal("more than one JSON document found")
	}

	// Permissions (off-Windows only).
	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Join(root, snap.RunID))
		if err != nil {
			t.Fatalf("stat run dir: %v", err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Errorf("run dir perm = 0o%o; want 0o700", got)
		}
		fileInfo, err := os.Stat(snapPath)
		if err != nil {
			t.Fatalf("stat snapshot: %v", err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Errorf("snapshot file perm = 0o%o; want 0o600", got)
		}
	}
}

// --- Idempotent-create guard ---------------------------------------------------

// A second Create for the same run ID fails without mutating the original bytes.
func TestCreate_DuplicateRunIDPreservesOriginal(t *testing.T) {
	root := t.TempDir()
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Create(snap); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	originalBytes, err := os.ReadFile(snapshotPath(root, snap.RunID))
	if err != nil {
		t.Fatalf("read original snapshot: %v", err)
	}

	err = store.Create(snap)
	if err == nil {
		t.Fatal("second Create should fail for existing run ID")
	}

	stillExists, err := os.ReadFile(snapshotPath(root, snap.RunID))
	if err != nil {
		t.Fatalf("read snapshot after duplicate Create: %v", err)
	}
	if !reflect.DeepEqual(stillExists, originalBytes) {
		t.Fatal("duplicate Create mutated the original snapshot")
	}
}

// --- Remove refuses when run directory has extra entries ------------------------

// Remove aborts if the run directory contains anything beyond snapshot.json,
// leaving both files untouched.
func TestRemove_RefusesWithExtraEntry(t *testing.T) {
	root := t.TempDir()
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(snap); err != nil {
		t.Fatalf("Create: %v", err)
	}

	runDir := filepath.Join(root, snap.RunID)
	extraPath := filepath.Join(runDir, "extra.txt")
	if err := os.WriteFile(extraPath, []byte("should survive"), 0o600); err != nil {
		t.Fatalf("create extra entry: %v", err)
	}

	originalSnap, err := os.ReadFile(snapshotPath(root, snap.RunID))
	if err != nil {
		t.Fatalf("read original snapshot: %v", err)
	}
	originalExtra, err := os.ReadFile(extraPath)
	if err != nil {
		t.Fatalf("read extra entry: %v", err)
	}

	err = store.Remove(snap.RunID)
	if err == nil {
		t.Fatal("Remove should fail when extra entries are present")
	}

	// Both files must be untouched.
	reSnap, err := os.ReadFile(snapshotPath(root, snap.RunID))
	if err != nil {
		t.Fatalf("read snapshot after failed Remove: %v", err)
	}
	if !reflect.DeepEqual(reSnap, originalSnap) {
		t.Fatal("snapshot was altered during failed Remove")
	}

	reExtra, err := os.ReadFile(extraPath)
	if err != nil {
		t.Fatalf("read extra entry after failed Remove: %v", err)
	}
	if string(reExtra) != string(originalExtra) {
		t.Fatal("extra entry was altered during failed Remove")
	}
}

// --- Bounded concurrency -------------------------------------------------------

// Two Store instances sharing one root call Create concurrently with the same
// valid Snapshot. Exactly one succeeds and the other fails with a collision
// error; the surviving Load equals the original Snapshot.
func TestConcurrentCreate_Collision(t *testing.T) {
	root := t.TempDir()
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	storeA, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore A: %v", err)
	}
	storeB, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore B: %v", err)
	}

	type result struct {
		err error
	}
	done := make(chan result, 2)

	go func() { done <- result{storeA.Create(snap)} }()
	go func() { done <- result{storeB.Create(snap)} }()

	var results [2]result
	for i := 0; i < 2; i++ {
		results[i] = <-done
	}

	succeeded, failed := 0, 0
	for _, r := range results {
		if r.err == nil {
			succeeded++
		} else {
			failed++
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 successful Create, got %d (errors: %v, %v)", succeeded, results[0].err, results[1].err)
	}

	loaded, err := storeA.Load(snap.RunID)
	if err != nil {
		t.Fatalf("Load after concurrent Create: %v", err)
	}
	if !reflect.DeepEqual(loaded, snap) {
		t.Error("snapshot after collision does not match the original")
	}
}

// After Create, one writer goroutine performs 30 valid running-state Replace
// calls while two reader goroutines repeatedly call Load until the writer
// finishes. Every Load must return a fully valid Snapshot with the same runId
// and either accepted or running state; no decode or partial error is allowed.
func TestConcurrentReplaceAndLoad(t *testing.T) {
	root := t.TempDir()
	base, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100},
		[]run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(base); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const writerOps = 30
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	writerDone := make(chan struct{})
	writerErr := make(chan error, 1)     // capacity-1: send first error or nil
	readerResults := make(chan error, 2) // one result per reader

	// Writer: perform up to 30 running-state Replaces; stops on first error.
	go func() {
		defer close(writerDone)
		var errored bool
		for i := 1; i <= writerOps; i++ {
			t0 := fixtureTime.Add(time.Duration(i) * time.Second)
			snap := base
			snap.State = RunRunning
			snap.Terminal = false
			snap.Status = nil
			snap.Outcome = nil
			snap.Result = nil
			snap.UpdatedAt = t0
			w := snap.Workers[0]
			w.State = WorkerRunning
			w.StartedAt = &t0
			w.FinishedAt = nil
			w.Process = &ProcessIdentity{PID: 42 + i, CreateTime: 100}
			w.Result = nil
			snap.Workers = []WorkerSnapshot{w}
			if err := snap.Validate(); err != nil {
				writerErr <- fmt.Errorf("replace op %d validate: %w", i, err)
				errored = true
				break
			}
			if err := store.Replace(snap); err != nil {
				writerErr <- fmt.Errorf("replace op %d: %w", i, err)
				errored = true
				break
			}
		}
		if !errored {
			writerErr <- nil
		}
	}()

	// Two reader goroutines started immediately.
	for r := 0; r < 2; r++ {
		ri := r
		go func() {
			attempt := 0
			for {
				select {
				case <-writerDone:
					// Writer finished — one final check, then return.
					snap, err := store.Load(base.RunID)
					if err != nil {
						readerResults <- fmt.Errorf("reader %d final Load: %w", ri, err)
						return
					}
					if err := snap.Validate(); err != nil {
						readerResults <- fmt.Errorf("reader %d final Validate: %w", ri, err)
						return
					}
					readerResults <- nil
					return

				case <-ctx.Done():
					readerResults <- fmt.Errorf("reader %d timed out: %w", ri, ctx.Err())
					return

				default:
					attempt++
					// Writer still running — perform a normal check.
					snap, err := store.Load(base.RunID)
					if err != nil {
						readerResults <- fmt.Errorf("reader %d attempt %d Load: %w", ri, attempt, err)
						return
					}
					if snap.RunID != base.RunID {
						readerResults <- fmt.Errorf("reader %d attempt %d: runId mismatch %q vs %q", ri, attempt, snap.RunID, base.RunID)
						return
					}
					if snap.State != RunAccepted && snap.State != RunRunning {
						readerResults <- fmt.Errorf("reader %d attempt %d: unexpected state %q", ri, attempt, snap.State)
						return
					}
				}
			}
		}()
	}

	// Main goroutine: collect one writer result and two reader results.
	if err := <-writerErr; err != nil {
		t.Fatalf("writer error: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := <-readerResults; err != nil {
			t.Fatal(err)
		}
	}
}
