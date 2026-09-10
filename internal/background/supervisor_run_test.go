//go:build darwin || linux

package background

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/runlog"
)

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
// terminal and completed, its status, outcome and result agree, and both
// workers carry their own completed results.
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
	if *loaded.Outcome != contracts.OutcomeCompleted {
		t.Errorf("outcome = %q, want %q", *loaded.Outcome, contracts.OutcomeCompleted)
	}
	if loaded.Result == nil {
		t.Fatal("result must not be nil on a terminal snapshot")
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
