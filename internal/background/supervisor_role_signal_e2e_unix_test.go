//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/admission"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
	"golang.org/x/sys/unix"
)

// TestSupervisorRoleSIGTERMCancelsAcceptedRun proves that the detached
// production supervisor turns a shutdown signal into the same orderly
// cancellation as a foreground run: its worker is torn down, its terminal
// snapshot is durable, and its prepared admission lease is released.
func TestSupervisorRoleSIGTERMCancelsAcceptedRun(t *testing.T) {
	runSupervisorRoleSignalCase(t, false)
}

// TestSupervisorRoleTwoSIGTERMsDoesNotInterruptTerminalWrite proves that a
// second shutdown signal cannot restore the default disposition while the
// accepted run is finishing its terminal snapshot.
func TestSupervisorRoleTwoSIGTERMsDoesNotInterruptTerminalWrite(t *testing.T) {
	runSupervisorRoleSignalCase(t, true)
}

func runSupervisorRoleSignalCase(t *testing.T, secondSignal bool) {
	t.Helper()
	setupFakePiEnv(t, inFlightScript("must not complete"))

	acceptedAt := time.Now().UTC().Truncate(time.Second)
	req := supervisorStartRequest{
		runID:            runlog.RunID(acceptedAt),
		acceptedAt:       acceptedAt,
		workspace:        t.TempDir(),
		tasks:            []run.Task{{Prompt: "run until shutdown", Model: "acme/m-1"}},
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
	deadline := time.Now().Add(e2eTerminalDeadline)
	for {
		snap, loadErr := store.Load(req.runID)
		if loadErr != nil {
			t.Fatalf("load running snapshot: %v", loadErr)
		}
		if snap.State == RunRunning && !snap.Terminal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never became running within %s; last state = %q, worker states = %v", e2eTerminalDeadline, snap.State, workerStates(snap))
		}
		time.Sleep(25 * time.Millisecond)
	}

	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		t.Fatalf("send first SIGTERM to supervisor %d: %v", pid, err)
	}
	if secondSignal {
		// Keep the second signal close to the first. It may race the
		// supervisor's quick exit on a platform where cancellation cleanup
		// finishes first; ESRCH still means the supervisor completed its
		// orderly path before the second delivery.
		time.Sleep(100 * time.Millisecond)
		if err := unix.Kill(pid, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
			t.Fatalf("send second SIGTERM to supervisor %d: %v", pid, err)
		}
	}

	var snap Snapshot
	for {
		snap, err = store.Load(req.runID)
		if err != nil {
			t.Fatalf("load terminal snapshot: %v", err)
		}
		if snap.Terminal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never became terminal within %s; last state = %q, worker states = %v", e2eTerminalDeadline, snap.State, workerStates(snap))
		}
		time.Sleep(50 * time.Millisecond)
	}

	if snap.State != RunCancelled {
		t.Fatalf("terminal state = %q, want %q; snapshot = %+v", snap.State, RunCancelled, snap)
	}
	if len(snap.Workers) != 1 {
		t.Fatalf("workers = %d, want 1", len(snap.Workers))
	}
	worker := snap.Workers[0]
	if worker.State != WorkerCancelled {
		t.Fatalf("worker state = %q, want %q", worker.State, WorkerCancelled)
	}
	if worker.FinishedAt == nil {
		t.Fatal("cancelled terminal worker has no finishedAt")
	}

	status := reapHandoffChild(t, pid, 20*time.Second)
	assertVoluntaryExit(t, status)
	assertRoleChildGone(t, pid)

	gate, err := admission.Open(req.admissionRoot, req.maxModelWorkers)
	if err != nil {
		t.Fatalf("open admission gate for assertion: %v", err)
	}
	if err := gate.Reconcile(); err != nil {
		t.Fatalf("reconcile admission gate: %v", err)
	}
	if st := readAdmissionState(t, req.admissionRoot); len(st.Tickets) != 0 {
		t.Errorf("admission gate holds live leases after cancellation: %+v", st.Tickets)
	}
}
