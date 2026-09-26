package background

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
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
	t.Run("both undeclared is legal", func(t *testing.T) {
		first := fixtureTask()
		second := fixtureTask()
		// Neither task declared: the all-or-none rule leaves the run legal,
		// exactly as it is in the foreground.
		first.Writes = run.WriteDeclaration{}
		second.Writes = run.WriteDeclaration{}

		snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
			ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{first, second}, time.Minute, nil)
		if err != nil {
			t.Fatalf("NewSnapshot with no declaration on either worker: %v", err)
		}
		if len(snap.Workers) != 2 {
			t.Fatalf("workers = %d, want 2", len(snap.Workers))
		}
		for _, w := range snap.Workers {
			if w.Task.WritesDeclared {
				t.Errorf("worker[%d]: writesDeclared = true, want false", w.WorkerID)
			}
		}
	})

	t.Run("partial declaration is refused", func(t *testing.T) {
		first := fixtureTask()
		second := fixtureTask()
		// One declared and one did not: the declaration must be all-or-none.
		second.Writes = run.WriteDeclaration{}

		_, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
			ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{first, second}, time.Minute, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "worker[2]: declared no writes while another worker declared: the declaration is all-or-none") {
			t.Errorf("error = %q; want the all-or-none message naming worker 2", err.Error())
		}
	})
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

// --- worker activity validation --------------------------------------------------

// TestValidateWorker_ActivityRules verifies the optional activity rule: a
// present Activity needs a start, a real UTC event time, and a
// non-negative count, while a valid one on a started worker is accepted.
func TestValidateWorker_ActivityRules(t *testing.T) {
	snap := oneTaskSnapshot(t, fixtureTask())
	base := snap.Workers[0]
	base.State = WorkerCompleted
	started := fixtureTime.Add(time.Minute)
	base.StartedAt = &started
	finished := fixtureTime.Add(2 * time.Minute)
	base.FinishedAt = &finished
	base.Activity = &pi.Activity{LastEventAt: started, LastTool: "bash", ToolCalls: 2}
	updated := fixtureTime.Add(3 * time.Minute)

	if errs := validateWorker(base, fixtureTime, updated); len(errs) != 0 {
		t.Fatalf("valid activity rejected: %v", errs)
	}

	withoutStart := base
	withoutStart.StartedAt = nil
	if errs := validateWorker(withoutStart, fixtureTime, updated); !containsSubstring(errs, "activity requires startedAt") {
		t.Fatalf("activity without startedAt errors = %v, want the startedAt rule", errs)
	}

	zeroEventAt := base
	zeroEventAt.Activity = &pi.Activity{ToolCalls: 1}
	if errs := validateWorker(zeroEventAt, fixtureTime, updated); !containsSubstring(errs, "activity lastEventAt must not be zero") {
		t.Fatalf("activity with zero lastEventAt errors = %v, want the non-zero rule", errs)
	}

	nonUTC := base
	spread := *base.Activity
	spread.LastEventAt = spread.LastEventAt.In(time.FixedZone("zero", 0))
	nonUTC.Activity = &spread
	if errs := validateWorker(nonUTC, fixtureTime, updated); !containsSubstring(errs, "activity lastEventAt must be in UTC") {
		t.Fatalf("activity with non-UTC lastEventAt errors = %v, want the UTC rule", errs)
	}

	negative := base
	badCount := *base.Activity
	badCount.ToolCalls = -1
	negative.Activity = &badCount
	if errs := validateWorker(negative, fixtureTime, updated); !containsSubstring(errs, "activity toolCalls must be >= 0") {
		t.Fatalf("activity with negative toolCalls errors = %v, want the count rule", errs)
	}
}

// containsSubstring reports whether any message contains want.
func containsSubstring(messages []string, want string) bool {
	for _, message := range messages {
		if strings.Contains(message, want) {
			return true
		}
	}
	return false
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
