package background

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/run"
)

// oneTaskSnapshot builds a valid one-worker Snapshot from a single task,
// returning it for further mutation by callers.
func oneTaskSnapshot(t *testing.T, task run.Task) Snapshot {
	t.Helper()
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{task}, time.Minute, nil)
	if err != nil {
		t.Fatalf("build one-task snapshot: %v", err)
	}
	return snap
}

// --- worker acceptedAt UTC enforcement -------------------------------------------

func TestValidate_WorkerAcceptedAtNonUTC(t *testing.T) {
	snap := oneTaskSnapshot(t, fixtureTask())
	w := snap.Workers[0]
	// Same instant but in zero-offset zone — location is not UTC.
	w.AcceptedAt = w.AcceptedAt.In(time.FixedZone("zero", 0))
	snap.Workers = []WorkerSnapshot{w}
	err := snap.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "acceptedAt must be in UTC") {
		t.Errorf("error = %q; want substring %q", err.Error(), "acceptedAt must be in UTC")
	}
}

// --- multi-worker missing writes declaration -------------------------------------

func TestNewSnapshot_MultiWorkerMissingDeclaration(t *testing.T) {
	first := fixtureTask()
	second := fixtureTask()
	second.Writes = run.WriteDeclaration{Declared: true, Paths: []string{"b.txt"}}
	// Remove Writes entirely so ProjectTasks produces Declared=false / Writes=nil.
	second.Writes = run.WriteDeclaration{}

	_, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{first, second}, time.Minute, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "writesDeclared must be true when multiple workers") {
		t.Errorf("error = %q; want substring %q", err.Error(), "writesDeclared must be true when multiple workers")
	}
}

// --- declared write path validation ----------------------------------------------

func TestNewSnapshot_InvalidWritePaths(t *testing.T) {
	badPaths := []string{
		"   ",      // whitespace-only
		".",        // resolves to .
		"/tmp/x",   // absolute path
		"../x",     // contains .. segment
		"src/../x", // unclean path
	}
	for _, wp := range badPaths {
		task := fixtureTask()
		task.Writes = run.WriteDeclaration{Declared: true, Paths: []string{wp}}
		_, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
			ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{task}, time.Minute, nil)
		if err == nil {
			t.Errorf("path %q: expected error, got nil", wp)
		}
	}
	// Duplicate pair: "x" appears twice — produce via a fresh task.
	task := run.Task{
		Prompt: "dup", Model: "fake/model",
		Writes: run.WriteDeclaration{Declared: true, Paths: []string{"x", "x"}},
	}
	_, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
		ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{task}, time.Minute, nil)
	if err == nil {
		t.Fatal("expected error for duplicate paths, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate write path") {
		t.Errorf("error = %q; want substring %q", err.Error(), "duplicate write path")
	}
}

// --- multi-worker path overlap --------------------------------------------------

func TestNewSnapshot_PathOverlap_TwoWorkers(t *testing.T) {
	goodBase := func(paths []string) run.Task {
		return run.Task{
			Prompt: "overlap", Model: "test/model",
			Writes: run.WriteDeclaration{Declared: true, Paths: paths},
			Data:   []run.DataFile{{Path: "d.txt", Content: []byte("x")}},
		}
	}
	type pair struct {
		pathsA, pathsB []string
		wantOK         bool
		name           string
	}
	cases := []pair{
		{name: "equal_paths", pathsA: []string{"s"}, pathsB: []string{"s"}, wantOK: false},
		{name: "ancestor_child", pathsA: []string{"src"}, pathsB: []string{"src/a"}, wantOK: false},
		{name: "siblings_no_overlap", pathsA: []string{"src/a"}, pathsB: []string{"src/ab"}, wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := goodBase(tc.pathsA)
			b := goodBase(tc.pathsB)
			_, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
				ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{a, b}, time.Minute, nil)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error for overlapping paths, got nil")
				}
				if !strings.Contains(err.Error(), "overlap") {
					t.Errorf("error = %q; want substring %q", err.Error(), "overlap")
				}
			}
		})
	}
}

// --- marshal/unmarshal round-trip with empty writes -----------------------------

func TestValidate_EmptyWritesRoundTrip(t *testing.T) {
	task := run.Task{
		Prompt: "empty-writes", Model: "fake/model",
		Writes: run.WriteDeclaration{Declared: true, Paths: []string{}},
	}
	snap := oneTaskSnapshot(t, task)

	// Marshal then unmarshal directly into Snapshot.
	data, err := json.Marshal(&snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if err := decoded.Validate(); err != nil {
		t.Fatalf("Validate after round-trip: %v", err)
	}
	if !decoded.Workers[0].Task.WritesDeclared {
		t.Error("writesDeclared should remain true after round-trip")
	}
	if len(decoded.Workers[0].Task.Writes) != 0 {
		t.Errorf("Writes should be empty slice, got %v", decoded.Workers[0].Task.Writes)
	}
}
