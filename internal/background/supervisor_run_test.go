//go:build darwin || linux

package background

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/admission"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// runningSnapshotDeadline bounds the wait for the observer's running snapshot
// to reach disk while both injected workers are still blocked.
const runningSnapshotDeadline = 5 * time.Second

// terminalSnapshotDeadline bounds the wait for runAcceptedRunWith to return
// once both injected workers have been released. It is long enough to cover
// the run's own verification command.
const terminalSnapshotDeadline = 120 * time.Second

// launchedChildSleepSeconds bounds the life of the child this test launches
// for worker 1: long enough to outlive every assertion made while it stands
// in for a launch, short enough that a child orphaned by a killed test binary
// terminates on its own.
const launchedChildSleepSeconds = "30"

// recordingWorker is the injected pi.Worker for a completed two-task run: it
// answers every task with a completed result and reports the identity of the
// process it "launched" through the request's observer, so the run exercises
// the same launch recording production relies on.
type recordingWorker struct {
	mu    sync.Mutex
	calls []pi.WorkerRequest
}

func (w *recordingWorker) Run(ctx context.Context, req pi.WorkerRequest) pi.WorkerResult {
	if req.OnProcessStart != nil {
		req.OnProcessStart(req.WorkerID, os.Getpid())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, req)
	return pi.WorkerResult{
		Model:         req.Model,
		ThinkingLevel: req.ThinkingLevel,
		Explanation:   "done",
		Status:        pi.StatusCompleted,
	}
}

// workerIDs returns the worker IDs this worker was asked to run.
func (w *recordingWorker) workerIDs() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]int, 0, len(w.calls))
	for _, call := range w.calls {
		ids = append(ids, call.WorkerID)
	}
	return ids
}

// TestRunAcceptedRunWithTwoCompletedTasks drives a two-task accepted run
// through the injected worker and reads the durable snapshot back: the run is
// terminal and completed, its outcome is the word the same result decides,
// its status, outcome and result agree, and both workers carry their own
// completed results.
func TestRunAcceptedRunWithTwoCompletedTasks(t *testing.T) {
	req, _, _ := newStartRequestWithTempRoots(t)
	if len(req.tasks) != 2 {
		t.Fatalf("test setup: validStartRequest must carry two tasks, got %d", len(req.tasks))
	}
	// A run ID and acceptance instant from this second, so the terminal
	// snapshot's finishedAt values fall after the accepted snapshot's
	// acceptedAt on their own rather than on elapsed test time.
	acceptedAt := time.Now().UTC().Truncate(time.Second)
	req.runID = runlog.RunID(acceptedAt)
	req.acceptedAt = acceptedAt
	// An existing directory the run's git inspection can reach.
	req.workspace = t.TempDir()

	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepareSupervisorStart: %v", err)
	}
	stored := supervisorStartResult{request: req, preparation: prep, accepted: true}

	worker := &recordingWorker{}
	if err := runAcceptedRunWith(context.Background(), worker, stored); err != nil {
		t.Fatalf("runAcceptedRunWith: %v", err)
	}

	// Every accepted task ran once, under its own worker ID.
	ids := worker.workerIDs()
	if len(ids) != len(req.tasks) {
		t.Fatalf("worker ran %d tasks, want %d", len(ids), len(req.tasks))
	}
	for i, id := range ids {
		if id != i+1 {
			t.Errorf("worker call %d ran under id %d, want %d", i, id, i+1)
		}
	}

	loaded, err := prep.store.Load(req.runID)
	if err != nil {
		t.Fatalf("reload terminal snapshot: %v", err)
	}

	if !loaded.Terminal {
		t.Errorf("terminal = false, want true")
	}
	if loaded.State != RunCompleted {
		t.Errorf("state = %q, want %q", loaded.State, RunCompleted)
	}
	if loaded.Status == nil {
		t.Fatal("status must not be nil on a terminal snapshot")
	}
	if *loaded.Status != contracts.RunCompleted {
		t.Errorf("status = %q, want %q", *loaded.Status, contracts.RunCompleted)
	}
	if loaded.Outcome == nil {
		t.Fatal("outcome must not be nil on a terminal snapshot")
	}
	if loaded.Result == nil {
		t.Fatal("result must not be nil on a terminal snapshot")
	}
	// The word is derived from the stored result through the same function
	// the CLI answers with, never hard-coded: this run carries a failed
	// verification, and that fact — not the run status alone — names it.
	if want := contracts.RunOutcome(run.RunFailure(*loaded.Result)); *loaded.Outcome != want {
		t.Errorf("outcome = %q, want %q for the stored result", *loaded.Outcome, want)
	}
	if loaded.Result.Status != *loaded.Status {
		t.Errorf("result.status = %q, want %q", loaded.Result.Status, *loaded.Status)
	}
	if loaded.Result.Outcome != *loaded.Outcome {
		t.Errorf("result.outcome = %q, want %q", loaded.Result.Outcome, *loaded.Outcome)
	}

	if len(loaded.Workers) != len(req.tasks) {
		t.Fatalf("workers = %d, want %d", len(loaded.Workers), len(req.tasks))
	}
	for i, w := range loaded.Workers {
		if w.State != WorkerCompleted {
			t.Errorf("worker[%d].state = %q, want %q", i+1, w.State, WorkerCompleted)
		}
		if !w.State.isTerminalWorkerState() {
			t.Errorf("worker[%d].state %q is not terminal", i+1, w.State)
		}
		if w.Result == nil {
			t.Fatalf("worker[%d].result must not be nil", i+1)
		}
		if w.Result.Status != pi.StatusCompleted {
			t.Errorf("worker[%d].result.status = %q, want %q", i+1, w.Result.Status, pi.StatusCompleted)
		}
		if want := req.tasks[i].Model; w.Result.Model != want {
			t.Errorf("worker[%d].result.model = %q, want %q", i+1, w.Result.Model, want)
		}
		if w.StartedAt == nil {
			t.Errorf("worker[%d].startedAt must not be nil", i+1)
		}
		if w.FinishedAt == nil {
			t.Errorf("worker[%d].finishedAt must not be nil", i+1)
		}
	}

	if err := loaded.Validate(); err != nil {
		t.Fatalf("reloaded terminal snapshot failed validation: %v", err)
	}
}

// blockingLaunchWorker is the injected pi.Worker for the running-phase guard:
// worker 1 reports the launch of a real child process and then blocks, worker
// 2 blocks without ever reporting a launch. Both stays in flight at the same
// time, so the snapshot loaded mid-run can only carry what the observer
// witnessed.
type blockingLaunchWorker struct {
	launchPID int
	release   chan struct{}
	releaseOn sync.Once
}

func (w *blockingLaunchWorker) Run(_ context.Context, req pi.WorkerRequest) pi.WorkerResult {
	if req.WorkerID == 1 && req.OnProcessStart != nil {
		req.OnProcessStart(req.WorkerID, w.launchPID)
	}
	<-w.release
	return pi.WorkerResult{
		Model:       req.Model,
		Explanation: "released",
		Status:      pi.StatusCompleted,
	}
}

// releaseWorkers unblocks both tasks; safe to call more than once, so a test
// that fails while both are blocked cannot leave runAcceptedRunWith hanging.
func (w *blockingLaunchWorker) releaseWorkers() {
	w.releaseOn.Do(func() { close(w.release) })
}

// describeSnapshotPhase words a snapshot for a wait that gave up: the failure
// names the last thing the store really held, not just "timeout".
func describeSnapshotPhase(snap Snapshot) string {
	states := make([]string, len(snap.Workers))
	for i, w := range snap.Workers {
		states[i] = string(w.State)
	}
	return fmt.Sprintf("state=%q terminal=%v workers=%v", snap.State, snap.Terminal, states)
}

// waitForRunningSnapshot polls the durable snapshot until the observer has
// made a non-terminal running phase durable, or the bounded deadline expires.
// Store.Load validates what it reads, so a snapshot that reaches disk but
// cannot be validated is reported through the load error.
func waitForRunningSnapshot(t *testing.T, store *Store, runID string) Snapshot {
	t.Helper()
	deadline := time.Now().Add(runningSnapshotDeadline)
	last := "nothing loaded yet"
	for {
		snap, err := store.Load(runID)
		if err != nil {
			last = fmt.Sprintf("load error: %v", err)
		} else {
			last = describeSnapshotPhase(snap)
			if snap.State == RunRunning && !snap.Terminal {
				return snap
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no running snapshot became durable within %v: last saw %s", runningSnapshotDeadline, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunAcceptedRunRecordsLaunchedWorkerWithItsOwnProcess drives a two-task
// accepted run whose first worker reports the launch of a real child process
// and whose second worker is never seen launching, and reads the snapshot the
// observer wrote while both were still in flight: the run is running and
// non-terminal, worker 1 carries its own observed process rather than the
// supervisor's identity, and worker 1's launch left worker 2 untouched.
//
// The observed launch names the child, not this test process: the snapshot's
// supervisor identity is the test process itself, so a recorded launch could
// only be told apart from that identity by a genuinely different process.
func TestRunAcceptedRunRecordsLaunchedWorkerWithItsOwnProcess(t *testing.T) {
	req, _, _ := newStartRequestWithTempRoots(t)
	acceptedAt := time.Now().UTC().Truncate(time.Second)
	req.runID = runlog.RunID(acceptedAt)
	req.acceptedAt = acceptedAt
	req.workspace = t.TempDir()

	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepareSupervisorStart: %v", err)
	}
	stored := supervisorStartResult{request: req, preparation: prep, accepted: true}

	// A real, short-lived child: its identity is the only thing the observer
	// can truthfully record for worker 1, and it is alive when sampled.
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("exec.LookPath(sleep): %v", err)
	}
	child := exec.Command(sleepPath, launchedChildSleepSeconds)
	if err := child.Start(); err != nil {
		t.Fatalf("start launch child: %v", err)
	}
	childPID := child.Process.Pid
	t.Cleanup(func() {
		_ = child.Process.Kill()
		if _, err := child.Process.Wait(); err != nil {
			t.Errorf("reap launch child %d: %v", childPID, err)
		}
	})

	worker := &blockingLaunchWorker{launchPID: childPID, release: make(chan struct{})}
	t.Cleanup(worker.releaseWorkers)
	runDone := make(chan error, 1)
	go func() { runDone <- runAcceptedRunWith(context.Background(), worker, stored) }()

	running := waitForRunningSnapshot(t, prep.store, req.runID)
	if running.Terminal {
		t.Errorf("mid-run terminal = true, want false")
	}
	if running.State != RunRunning {
		t.Errorf("mid-run state = %q, want %q", running.State, RunRunning)
	}
	if len(running.Workers) != len(req.tasks) {
		t.Fatalf("mid-run workers = %d, want %d", len(running.Workers), len(req.tasks))
	}
	if err := running.Validate(); err != nil {
		t.Fatalf("mid-run running snapshot failed validation: %v", err)
	}

	launched := running.Workers[0]
	if launched.State != WorkerRunning {
		t.Errorf("worker 1 state = %q, want %q", launched.State, WorkerRunning)
	}
	if launched.Process == nil {
		t.Fatal("worker 1 process must carry the observed launch")
	}
	if launched.Process.PID != childPID {
		t.Errorf("worker 1 process pid = %d, want the launched child %d", launched.Process.PID, childPID)
	}
	if launched.Process.PID == running.Supervisor.PID {
		t.Errorf("worker 1 process pid = supervisor pid %d; a launch must be recorded with its own identity", launched.Process.PID)
	}
	if launched.Process.CreateTime == running.Supervisor.CreateTime {
		t.Errorf("worker 1 process createTime = supervisor createTime %d; a launch must be recorded with its own identity", launched.Process.CreateTime)
	}
	if launched.StartedAt == nil {
		t.Error("worker 1 startedAt must carry the observed start")
	}

	unobserved := running.Workers[1]
	if unobserved.State != WorkerQueued {
		t.Errorf("worker 2 state = %q, want %q", unobserved.State, WorkerQueued)
	}
	if unobserved.Process != nil {
		t.Errorf("worker 2 process = %+v, want nil for a launch nobody observed", unobserved.Process)
	}
	if unobserved.StartedAt != nil {
		t.Errorf("worker 2 startedAt = %v, want nil for a launch nobody observed", unobserved.StartedAt)
	}

	worker.releaseWorkers()
	var runErr error
	select {
	case runErr = <-runDone:
	case <-time.After(terminalSnapshotDeadline):
		t.Fatalf("runAcceptedRunWith did not return within %v", terminalSnapshotDeadline)
	}
	if runErr != nil {
		t.Fatalf("runAcceptedRunWith: %v", runErr)
	}

	loaded, err := prep.store.Load(req.runID)
	if err != nil {
		t.Fatalf("reload terminal snapshot: %v", err)
	}
	if !loaded.Terminal || loaded.State != RunCompleted {
		t.Fatalf("terminal snapshot = %s, want a terminal completed run", describeSnapshotPhase(loaded))
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("terminal snapshot failed validation: %v", err)
	}
	settled := loaded.Workers[1]
	if settled.State != WorkerCompleted {
		t.Errorf("worker 2 terminal state = %q, want %q", settled.State, WorkerCompleted)
	}
	if settled.StartedAt != nil {
		t.Errorf("worker 2 startedAt = %v, want nil: no start was ever observed", settled.StartedAt)
	}
	if settled.Process != nil {
		t.Errorf("worker 2 process = %+v, want nil: no process was ever observed", settled.Process)
	}
	if launched := loaded.Workers[0]; launched.Process == nil || launched.Process.PID != childPID {
		t.Errorf("worker 1 terminal process = %+v, want the observed launch pid %d", loaded.Workers[0].Process, childPID)
	}
}

// settledStatusWorker is the injected pi.Worker for the lease guard: it answers
// every task with the status the fixture asked for, so one worker can settle
// in failure while the others complete.
type settledStatusWorker struct {
	status func(workerID int) string
}

func (w settledStatusWorker) Run(_ context.Context, req pi.WorkerRequest) pi.WorkerResult {
	status := pi.StatusCompleted
	if w.status != nil {
		status = w.status(req.WorkerID)
	}
	return pi.WorkerResult{Model: req.Model, Explanation: "settled", Status: status}
}

// TestRunAcceptedRunReleasesEveryPreparedLease proves that the admission gate
// holds no live lease once an accepted run is over, on the path where every
// worker completes and on the path where one worker fails. Tickets are
// prepared by prepareSupervisorStart and granted by the controller's admission
// arm, so a lease the controller never released stays durable under this
// process's own identity — a freshly opened Gate, reconciled, keeps it.
func TestRunAcceptedRunReleasesEveryPreparedLease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status func(workerID int) string
	}{
		{name: "every worker completes", status: nil},
		{name: "one worker fails", status: func(workerID int) string {
			if workerID == 1 {
				return pi.StatusFailed
			}
			return pi.StatusCompleted
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _, admissionRoot := newStartRequestWithTempRoots(t)
			acceptedAt := time.Now().UTC().Truncate(time.Second)
			req.runID = runlog.RunID(acceptedAt)
			req.acceptedAt = acceptedAt
			req.workspace = t.TempDir()

			prep, err := prepareSupervisorStart(req)
			if err != nil {
				t.Fatalf("prepareSupervisorStart: %v", err)
			}
			if prepared := readAdmissionState(t, admissionRoot); len(prepared.Tickets) != len(req.tasks) {
				t.Fatalf("prepared tickets = %d, want %d: %+v", len(prepared.Tickets), len(req.tasks), prepared.Tickets)
			}
			stored := supervisorStartResult{request: req, preparation: prep, accepted: true}

			if err := runAcceptedRunWith(context.Background(), settledStatusWorker{status: tc.status}, stored); err != nil {
				t.Fatalf("runAcceptedRunWith: %v", err)
			}
			if _, err := prep.store.Load(req.runID); err != nil {
				t.Fatalf("reload terminal snapshot: %v", err)
			}

			// Observe the gate the way the admission tests do: open it over
			// the same root, reconcile, and read what is left. Reconcile only
			// reaps tickets whose owner is gone, so any ticket this still-live
			// process owns is a lease the run never released.
			gate, err := admission.Open(admissionRoot, req.maxModelWorkers)
			if err != nil {
				t.Fatalf("open admission gate for assertion: %v", err)
			}
			if err := gate.Reconcile(); err != nil {
				t.Fatalf("reconcile admission gate: %v", err)
			}
			if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
				t.Errorf("admission gate holds live leases after the run: %+v", st.Tickets)
			}
		})
	}
}
