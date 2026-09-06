package background

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// makeRunID returns a valid run ID whose timestamp prefix matches t.
func makeRunID(t time.Time) string { return runlog.RunID(t) }

// fixtureTime is a fixed wall-clock used by every test so that
// ValidateRunID never fights real clock drift.
var fixtureTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fixtureTask produces a minimal task carrying one data file.
func fixtureTask() run.Task {
	return run.Task{
		Prompt:        "do the thing",
		Model:         "fake/model",
		ThinkingLevel: pi.ThinkingLow,
		Writes:        run.WriteDeclaration{Declared: true, Paths: []string{"a.txt"}},
		Data: []run.DataFile{
			{Path: "readme.md", Content: []byte("hello world")},
		},
	}
}

// --- compact constructor tests ---------------------------------------------------

func TestNewSnapshot_ValidAcceptedState_TwoTasks(t *testing.T) {
	secondTask := fixtureTask()
	secondTask.Writes = run.WriteDeclaration{Declared: true, Paths: []string{"b.txt"}}
	tasks := []run.Task{fixtureTask(), secondTask}
	timeout := 2 * time.Minute
	prepared := worktree.Prepared{Name: "test-wt", Path: "/tmp/wt", Branch: "run/test-wt", Head: "abc123"}

	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/workspace", ProcessIdentity{PID: 1, CreateTime: 100}, tasks, timeout, &prepared)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	if snap.SchemaVersion != SchemaVersion {
		t.Errorf("schemaVersion = %d; want %d", snap.SchemaVersion, SchemaVersion)
	}
	if snap.RunID != makeRunID(fixtureTime) {
		t.Errorf("runId = %q; want %q", snap.RunID, makeRunID(fixtureTime))
	}
	if snap.State != RunAccepted {
		t.Errorf("state = %q; want %q", snap.State, RunAccepted)
	}
	if snap.Terminal {
		t.Error("terminal should be false")
	}
	wantUTC := fixtureTime.UTC().Truncate(time.Second)
	if snap.AcceptedAt != wantUTC {
		t.Errorf("acceptedAt = %v; want %v", snap.AcceptedAt, wantUTC)
	}
	if snap.UpdatedAt != wantUTC {
		t.Errorf("updatedAt = %v; want %v", snap.UpdatedAt, wantUTC)
	}
	if snap.Workspace != "/workspace" {
		t.Errorf("workspace = %q; want %q", snap.Workspace, "/workspace")
	}
	if snap.Supervisor.PID != 1 || snap.Supervisor.CreateTime != 100 {
		t.Errorf("supervisor = %+v; want {PID:1,CreateTime:100}", snap.Supervisor)
	}
	if len(snap.Workers) != 2 {
		t.Fatalf("workers count = %d; want 2", len(snap.Workers))
	}
	for i, w := range snap.Workers {
		wantID := i + 1
		if w.WorkerID != wantID {
			t.Errorf("worker[%d].workerId = %d; want %d", i, w.WorkerID, wantID)
		}
		if w.State != WorkerQueued {
			t.Errorf("worker[%d].state = %q; want %q", i, w.State, WorkerQueued)
		}
		expectedDeadline := fixtureTime.Add(queueDuration)
		if w.QueueDeadline != expectedDeadline {
			t.Errorf("worker[%d].queueDeadline = %v; want %v", i, w.QueueDeadline, expectedDeadline)
		}
		if w.ExecutionTimeout != timeout.String() {
			t.Errorf("worker[%d].executionTimeout = %q; want %q", i, w.ExecutionTimeout, timeout.String())
		}
		if w.StartedAt != nil || w.FinishedAt != nil || w.Process != nil || w.Result != nil {
			t.Errorf("worker[%d]: startedAt/finishedAt/process/result must all be nil", i)
		}
	}
	if snap.Status != nil {
		t.Error("status must be absent (nil)")
	}
	if snap.Outcome != nil {
		t.Error("outcome must be absent (nil)")
	}
	if snap.Result != nil {
		t.Error("result must be absent (nil)")
	}
}

func TestNewSnapshot_NilWorktree_RemainsNil(t *testing.T) {
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws", ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if snap.Worktree != nil {
		t.Errorf("worktree = %+v; want nil when prepared=nil", snap.Worktree)
	}
}

func TestNewSnapshot_PreparedIsCopied(t *testing.T) {
	raw := worktree.Prepared{Name: "orig", Path: "/old", Branch: "run/orig", Head: "aaa"}
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws", ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{fixtureTask()}, time.Minute, &raw)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if snap.Worktree == nil {
		t.Fatal("expected non-nil worktree copy")
	}
	if snap.Worktree.Name == raw.Name && snap.Worktree.Path == raw.Path {
		// Verify mutation of caller's input does not propagate into the snapshot.
		raw.Name = "mutated"
		raw.Path = "/mutated"
		if snap.Worktree.Name != "orig" || snap.Worktree.Path != "/old" {
			t.Errorf("snapshot worktree mutated after caller changed input: name=%q path=%q", snap.Worktree.Name, snap.Worktree.Path)
		}
	} else {
		t.Errorf("copy not independent? name=%q path=%q (original name=%q path=%q)", snap.Worktree.Name, snap.Worktree.Path, raw.Name, raw.Path)
	}
}

func TestNewSnapshot_TaskProjection(t *testing.T) {
	// Over-limit UTF-8-safe prompt: build a >4096-byte string of 3-byte "€" sequences.
	big := ""
	for i := 0; i < 2000; i++ {
		big += "\xe2\x82\xac" // €
	}
	task := run.Task{
		Prompt:        big,
		Model:         "big/model",
		ThinkingLevel: pi.ThinkingHigh,
		Writes:        run.WriteDeclaration{Declared: true, Paths: nil},
		Data: []run.DataFile{
			{Path: "data.bin", Content: []byte{0x01, 0x02, 0x03}},
		},
	}
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws", ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{task}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	w := snap.Workers[0]

	if w.Task.Model != "big/model" {
		t.Errorf("model = %q; want %q", w.Task.Model, "big/model")
	}
	if w.Task.ThinkingLevel != string(pi.ThinkingHigh) {
		t.Errorf("thinkingLevel = %q; want %q", w.Task.ThinkingLevel, string(pi.ThinkingHigh))
	}
	wantPromptLen := 4096
	if len(w.Task.Prompt) > wantPromptLen {
		t.Errorf("prompt byte length = %d; at most %d", len(w.Task.Prompt), wantPromptLen)
	}
	if !w.Task.PromptTruncated {
		t.Error("promptTruncated should be true for oversized prompt")
	}
	if !utf8.ValidString(w.Task.Prompt) {
		t.Error("truncated prompt must remain valid UTF-8")
	}

	// Declared-empty writes: WroteDeclaration=true, Writes=nil.
	if !w.Task.WritesDeclared {
		t.Error("writesDeclared should be true")
	}
	if w.Task.Writes != nil {
		t.Errorf("writes = %v; want nil for empty declared set", w.Task.Writes)
	}

	// Data file: path, byte count, SHA-256 recorded; raw content absent.
	if len(w.Task.Data) != 1 {
		t.Fatalf("data length = %d; want 1", len(w.Task.Data))
	}
	df := w.Task.Data[0]
	if df.Path != "data.bin" {
		t.Errorf("data[0].path = %q; want %q", df.Path, "data.bin")
	}
	if df.Bytes != 3 {
		t.Errorf("data[0].bytes = %d; want 3", df.Bytes)
	}
	if df.SHA256 == "" {
		t.Error("data[0].sha256 must be present")
	}
}

func TestNewSnapshot_RawDataAbsentFromJSON(t *testing.T) {
	task := run.Task{
		Prompt: "prompt", Model: "test/model", ThinkingLevel: pi.ThinkingLevel(""),
		Data: []run.DataFile{{Path: "f.txt", Content: []byte("SENSITIVE_CONTENT")}},
	}
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws", ProcessIdentity{PID: 1, CreateTime: 100}, []run.Task{task}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	b, err := json.Marshal(&snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	payload := string(b)
	if contains(payload, "SENSITIVE_CONTENT") {
		t.Error("raw file content must not appear in marshaled Snapshot")
	}
}

// Helper — kept simple instead of importing external packages.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- validation guards ------------------------------------------------------------

func TestNewSnapshot_ValidationGuards(t *testing.T) {
	base := func() NewSnapshotArgs {
		return NewSnapshotArgs{
			RunID:            makeRunID(fixtureTime),
			AcceptedAt:       fixtureTime,
			Workspace:        "/ws",
			Supervisor:       ProcessIdentity{PID: 1, CreateTime: 100},
			Tasks:            []run.Task{fixtureTask()},
			ExecutionTimeout: time.Minute,
			Prepared:         nil,
		}
	}

	tests := []struct {
		name    string
		adjust  func(*NewSnapshotArgs)
		wantErr string
	}{
		{
			name:    "invalid_run_id_format",
			adjust:  func(a *NewSnapshotArgs) { a.RunID = "not-a-run-id" },
			wantErr: "runId",
		},
		{
			name:    "run_id_time_mismatch",
			adjust:  func(a *NewSnapshotArgs) { a.RunID = makeRunID(fixtureTime.Add(1 * time.Hour)) },
			wantErr: "runId: prefix",
		},
		{
			name:    "zero_accepted_at",
			adjust:  func(a *NewSnapshotArgs) { a.AcceptedAt = time.Time{} },
			wantErr: "acceptedAt must not be zero",
		},
		{
			name:    "empty_workspace",
			adjust:  func(a *NewSnapshotArgs) { a.Workspace = "" },
			wantErr: "workspace is required",
		},
		{
			name:    "supervisor_pid_zero",
			adjust:  func(a *NewSnapshotArgs) { a.Supervisor = ProcessIdentity{PID: 0, CreateTime: 100} },
			wantErr: "supervisor: pid must be positive",
		},
		{
			name:    "supervisor_create_time_zero",
			adjust:  func(a *NewSnapshotArgs) { a.Supervisor = ProcessIdentity{PID: 1, CreateTime: 0} },
			wantErr: "supervisor: createTime must be positive",
		},
		{
			name:    "zero_tasks",
			adjust:  func(a *NewSnapshotArgs) { a.Tasks = nil },
			wantErr: "at least one task required",
		},
		{
			name: "too_many_tasks",
			adjust: func(a *NewSnapshotArgs) {
				a.Tasks = make([]run.Task, run.MaxTasks+1)
				for i := range a.Tasks {
					a.Tasks[i] = fixtureTask()
				}
			},
			wantErr: fmt.Sprintf("at most %d tasks supported", run.MaxTasks),
		},
		{
			name:    "non_positive_timeout",
			adjust:  func(a *NewSnapshotArgs) { a.ExecutionTimeout = 0 },
			wantErr: "executionTimeout must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := base()
			tt.adjust(&args)
			_, err := NewSnapshot(args.RunID, args.AcceptedAt, args.Workspace, args.Supervisor, args.Tasks, args.ExecutionTimeout, args.Prepared)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q; want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// --- helper type to keep test tables readable -------------------------------------

type NewSnapshotArgs struct {
	RunID            string
	AcceptedAt       time.Time
	Workspace        string
	Supervisor       ProcessIdentity
	Tasks            []run.Task
	ExecutionTimeout time.Duration
	Prepared         *worktree.Prepared
}

// --- compact regression cases ---------------------------------------------------

func TestValidate_RegressionCases(t *testing.T) {
	makeValid := func() Snapshot {
		t.Helper()
		tasks := []run.Task{{Prompt: "do", Model: "test/model"}}
		snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/ws",
			ProcessIdentity{PID: 1, CreateTime: 100}, tasks, time.Minute, nil)
		if err != nil {
			t.Fatalf("build base snapshot: %v", err)
		}
		return snap
	}

	tests := []struct {
		name    string
		adjust  func(Snapshot) Snapshot
		wantErr string
	}{
		{
			name: "mismatched_decoded_runid_rejected",
			adjust: func(s Snapshot) Snapshot {
				s.RunID = runlog.RunID(fixtureTime.Add(time.Hour))
				s.AcceptedAt = fixtureTime // mismatch with RunID prefix
				return s
			},
			wantErr: "runId",
		},
		{
			name: "non_utc_accepted_at_rejected",
			adjust: func(s Snapshot) Snapshot {
				s.AcceptedAt = fixtureTime.In(time.FixedZone("WEST", -60*60))
				return s
			},
			wantErr: "acceptedAt must be in UTC",
		},
		{
			name: "running_top_result_rejected",
			adjust: func(s Snapshot) Snapshot {
				s.State = RunRunning
				s.Terminal = false
				s.Result = &run.Result{} // must be nil for running
				return s
			},
			wantErr: "running: result must be nil",
		},
		{
			name: "terminal_nil_outcome_no_panic",
			adjust: func(s Snapshot) Snapshot {
				s.State = RunCompleted
				s.Terminal = true
				s.Status = ptrRunStatus(contracts.RunCompleted)
				s.Outcome = nil // will fail validation but must not panic
				s.Result = &run.Result{Outcome: "completed"}
				return s
			},
			wantErr: "outcome",
		},
		{
			name: "unknown_outcome_rejected",
			adjust: func(s Snapshot) Snapshot {
				s.State = RunCompleted
				s.Terminal = true
				s.Status = ptrRunStatus(contracts.RunCompleted)
				fake := contracts.Outcome("bogus")
				s.Outcome = &fake
				s.Result = &run.Result{Outcome: "bogus"}
				return s
			},
			wantErr: "outcome must be a defined non-usage value",
		},
		{
			name: "finished_before_accepted_rejected",
			adjust: func(s Snapshot) Snapshot {
				// finishedAt < acceptedAt is invalid
				early := fixtureTime.Add(-time.Hour)
				w := s.Workers[0]
				w.FinishedAt = &early
				// StartedAt left nil — terminal workers may omit it.
				w.State = WorkerFailed
				s.Workers = []WorkerSnapshot{w}
				s.State = RunCompleted
				s.Terminal = true
				s.Status = ptrRunStatus(contracts.RunCompleted)
				ok := contracts.OutcomeCompleted
				s.Outcome = &ok
				return s
			},
			wantErr: "finishedAt",
		},
		{
			name: "exact_worker_status_mismatch_rejected",
			adjust: func(s Snapshot) Snapshot {
				w := s.Workers[0]
				w.State = WorkerCompleted
				w.FinishedAt = ptrTime(fixtureTime.Add(1 * time.Second))
				w.Result = &pi.WorkerResult{Status: "failed"} // case and state differ
				s.Workers = []WorkerSnapshot{w}
				s.State = RunCompleted
				s.Terminal = true
				s.Status = ptrRunStatus(contracts.RunCompleted)
				ok := contracts.OutcomeCompleted
				s.Outcome = &ok
				return s
			},
			wantErr: "result.status must equal worker state",
		},
		{
			name: "terminal_status_must_equal_state",
			adjust: func(s Snapshot) Snapshot {
				w := s.Workers[0]
				w.State = WorkerCompleted
				w.FinishedAt = ptrTime(fixtureTime.Add(1 * time.Second))
				s.Workers = []WorkerSnapshot{w}
				s.State = RunCompleted
				s.Terminal = true
				s.Status = ptrRunStatus(contracts.RunFailed)
				ok := contracts.OutcomeCompleted
				s.Outcome = &ok
				return s
			},
			wantErr: "status must equal state",
		},
		{
			name: "accepted_non_queued_worker_rejected",
			adjust: func(s Snapshot) Snapshot {
				w := s.Workers[0]
				w.State = WorkerRunning
				s.Workers = []WorkerSnapshot{w}
				return s
			},
			wantErr: "must be queued in accepted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := makeValid()
			s = tt.adjust(s)
			err := s.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q; want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func ptrRunStatus(s contracts.RunStatus) *contracts.RunStatus {
	return &s
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
