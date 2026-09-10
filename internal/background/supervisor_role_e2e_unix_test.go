//go:build darwin || linux

package background

import (
	"context"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/admission"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// e2eTerminalDeadline bounds the wait for the detached supervisor to make
// the terminal snapshot durable. It covers building the two helper
// binaries' worth of process startup plus one full worker round trip.
const e2eTerminalDeadline = 90 * time.Second

// TestSupervisorRoleRunsAcceptedRunEndToEnd is the one test that proves the
// whole background path against the binary that ships: the parent hands one
// accepted run to a fresh process of the production binary, that process
// dispatches the supervisor role on its own, runs the task through the
// production private worker host against the fake Pi, and writes the
// terminal snapshot after the parent has already returned. Nothing here is
// injected — no test-only child mode, no seam worker — so a break anywhere
// between the role dispatch, the run controller and the terminal snapshot
// shows up as a run that never reaches terminal.
func TestSupervisorRoleRunsAcceptedRunEndToEnd(t *testing.T) {
	setupFakePiEnv(t, happyPathScript("end-to-end answer"))

	acceptedAt := time.Now().UTC().Truncate(time.Second)
	req := supervisorStartRequest{
		runID:            runlog.RunID(acceptedAt),
		acceptedAt:       acceptedAt,
		workspace:        t.TempDir(),
		tasks:            []run.Task{{Prompt: "run the end-to-end task", Model: "acme/m-1"}},
		executionTimeout: 5 * time.Minute,
		backgroundRoot:   t.TempDir(),
		admissionRoot:    t.TempDir(),
		maxModelWorkers:  1,
		piExecutable:     fakePiBin(t),
	}

	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := startSupervisorHandoffWithProcess(ctx, piWorkerBin(t), req, start)
	if err != nil {
		t.Fatalf("accepted handoff against the production binary: %v", err)
	}
	if !result.accepted {
		t.Fatal("handoff reported a non-accepted result")
	}

	store, err := NewStore(req.backgroundRoot)
	if err != nil {
		t.Fatalf("construct store: %v", err)
	}

	// The parent returned while the run was still in flight, so the only
	// honest way to observe the run is to read the durable snapshot until
	// it turns terminal. The last state seen is reported on failure: a run
	// stuck at accepted and one stuck at running are different defects.
	var snap Snapshot
	deadline := time.Now().Add(e2eTerminalDeadline)
	for {
		snap, err = store.Load(req.runID)
		if err != nil {
			t.Fatalf("load snapshot: %v", err)
		}
		if snap.Terminal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never became terminal within %s; last state = %q, worker states = %v",
				e2eTerminalDeadline, snap.State, workerStates(snap))
		}
		time.Sleep(50 * time.Millisecond)
	}

	if snap.State != RunCompleted {
		t.Fatalf("terminal state = %q, want %q; snapshot = %+v", snap.State, RunCompleted, snap)
	}
	if len(snap.Workers) != 1 {
		t.Fatalf("workers = %d, want 1", len(snap.Workers))
	}
	worker := snap.Workers[0]
	if worker.State != WorkerCompleted {
		t.Fatalf("worker state = %q, want %q", worker.State, WorkerCompleted)
	}
	if worker.Result == nil {
		t.Fatal("terminal worker carries no result")
	}
	if worker.Result.Explanation != "end-to-end answer" {
		t.Fatalf("worker explanation = %q, want the fake Pi answer", worker.Result.Explanation)
	}
	if worker.Result.Status != pi.StatusCompleted {
		t.Fatalf("worker result status = %q, want %q", worker.Result.Status, pi.StatusCompleted)
	}
	// The worker really ran in its own process: the supervisor observed a
	// launch whose identity is neither empty nor the supervisor's own.
	if worker.Process == nil || worker.Process.PID <= 0 {
		t.Fatalf("worker process identity = %+v, want the observed worker-host process", worker.Process)
	}
	if worker.Process.PID == snap.Supervisor.PID {
		t.Fatalf("worker process pid equals the supervisor pid %d", snap.Supervisor.PID)
	}

	// The supervisor exits on its own once the run is over, and it exits
	// successfully: a role that failed would report a role exit code.
	status := reapHandoffChild(t, pid, 20*time.Second)
	assertVoluntaryExit(t, status)
	assertRoleChildGone(t, pid)

	// Nothing is left holding admission: a lease this still-live parent's
	// child never released would survive reconciliation.
	gate, err := admission.Open(req.admissionRoot, req.maxModelWorkers)
	if err != nil {
		t.Fatalf("open admission gate for assertion: %v", err)
	}
	if err := gate.Reconcile(); err != nil {
		t.Fatalf("reconcile admission gate: %v", err)
	}
	if st := readAdmissionState(t, req.admissionRoot); len(st.Tickets) != 0 {
		t.Errorf("admission gate holds live leases after the run: %+v", st.Tickets)
	}
}

// workerStates lists the per-worker states of a snapshot for a failure
// message.
func workerStates(snap Snapshot) []WorkerState {
	states := make([]WorkerState, 0, len(snap.Workers))
	for _, worker := range snap.Workers {
		states = append(states, worker.State)
	}
	return states
}
