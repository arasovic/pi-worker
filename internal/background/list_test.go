package background

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// createTerminalSnapshot builds and stores one completed background run
// under root, carrying one worker per model in models, and returns the
// snapshot path it wrote. The expected listing values are built as
// literals in the test bodies; this helper only arranges state.
func createTerminalSnapshot(t *testing.T, root, runID string, acceptedAt time.Time, models []string) string {
	t.Helper()
	tasks := make([]run.Task, len(models))
	for i, model := range models {
		tasks[i] = run.Task{Prompt: "task", Model: model, ThinkingLevel: pi.ThinkingLow}
	}
	snap, err := NewSnapshot(runID, acceptedAt, "/test-workspace", ProcessIdentity{PID: 1, CreateTime: 1}, tasks, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	finishedAt := acceptedAt.Add(time.Second)
	snap.State = RunCompleted
	snap.Terminal = true
	snap.UpdatedAt = finishedAt
	snap.Status = ptrRunStatus(contracts.RunCompleted)
	outcome := contracts.OutcomeCompleted
	snap.Outcome = &outcome
	for i := range snap.Workers {
		snap.Workers[i].State = WorkerCompleted
		snap.Workers[i].FinishedAt = &finishedAt
	}
	if err := snap.Validate(); err != nil {
		t.Fatalf("validate terminal snapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(snap); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return snapshotPath(root, runID)
}

// TestListRunsMissingRoot requires that a root that does not exist lists
// nothing and does not error: a missing background store means there are
// no background runs, not a failure.
func TestListRunsMissingRoot(t *testing.T) {
	runs, err := ListRuns(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if runs == nil {
		t.Fatal("ListRuns returned nil; want a non-nil empty slice")
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %+v; want empty", runs)
	}
}

// TestListRunsTerminalSnapshot requires that a finished run lists with its
// own outcome, the RFC3339 acceptedAt, the worker count, and the distinct
// models in worker order.
func TestListRunsTerminalSnapshot(t *testing.T) {
	root := t.TempDir()
	acceptedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	path := createTerminalSnapshot(t, root, "20260901T120000Z-1", acceptedAt, []string{"acme/a", "acme/b", "acme/a"})

	runs, err := ListRuns(root)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	want := []runlog.Run{{
		RunID:     "20260901T120000Z-1",
		StartedAt: "2026-09-01T12:00:00Z",
		Workspace: "/test-workspace",
		Tasks:     3,
		Models:    []string{"acme/a", "acme/b"},
		Outcome:   "completed",
		Path:      path,
	}}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs = %+v, want %+v", runs, want)
	}
}

// TestListRunsUnreadableSnapshotIsUnknown requires that a run directory
// whose snapshot cannot be read is still listed, with the directory's run
// id, the snapshot path, and outcome unknown.
func TestListRunsUnreadableSnapshotIsUnknown(t *testing.T) {
	root := t.TempDir()
	runID := "20260901T120000Z-3"
	if err := os.Mkdir(filepath.Join(root, runID), 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(snapshotPath(root, runID), []byte("not a snapshot\n"), 0o600); err != nil {
		t.Fatalf("write garbage snapshot: %v", err)
	}

	runs, err := ListRuns(root)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	want := []runlog.Run{{
		RunID:   runID,
		Models:  []string{},
		Outcome: "unknown",
		Path:    snapshotPath(root, runID),
	}}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs = %+v, want %+v", runs, want)
	}
}

// TestListRunsSkipsNonRunIDEntries requires that a directory entry whose
// name is not a run id is skipped silently while a real run beside it is
// still listed.
func TestListRunsSkipsNonRunIDEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "not-a-run-id"), 0o700); err != nil {
		t.Fatalf("mkdir non-run entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write loose file: %v", err)
	}
	acceptedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	path := createTerminalSnapshot(t, root, "20260901T120000Z-4", acceptedAt, []string{"acme/a"})

	runs, err := ListRuns(root)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want exactly the one real run", runs)
	}
	if runs[0].RunID != "20260901T120000Z-4" || runs[0].Path != path {
		t.Fatalf("runs[0] = %+v, want run 20260901T120000Z-4 at %s", runs[0], path)
	}
}

// TestListRunsNewestFirst requires that several runs list newest first by
// run id, the same ordering runlog.List produces.
func TestListRunsNewestFirst(t *testing.T) {
	root := t.TempDir()
	for _, runID := range []string{
		"20260901T120000Z-1",
		"20260901T130000Z-2",
		"20260901T110000Z-3",
	} {
		acceptedAt, err := runlog.ParseRunID(runID)
		if err != nil {
			t.Fatalf("ParseRunID(%s): %v", runID, err)
		}
		createTerminalSnapshot(t, root, runID, acceptedAt, []string{"acme/a"})
	}

	runs, err := ListRuns(root)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	got := make([]string, len(runs))
	for i, run := range runs {
		got[i] = run.RunID
	}
	want := []string{"20260901T130000Z-2", "20260901T120000Z-1", "20260901T110000Z-3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
