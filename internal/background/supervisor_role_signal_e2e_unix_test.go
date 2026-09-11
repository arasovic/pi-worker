//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"path/filepath"
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
	runSupervisorRoleSignalCase(t, "FAKEPI_SPAWN_PIDFILE", false)
}

// TestSupervisorRoleTwoSIGTERMsStillEndsAsCancelled covers the second
// shutdown signal: when it is delivered before the supervisor exits, the run
// still ends as a durable cancelled snapshot. The supervisor may finish its
// orderly path first, in which case the second signal is never delivered and
// this case degrades to the single-signal one.
func TestSupervisorRoleTwoSIGTERMsStillEndsAsCancelled(t *testing.T) {
	runSupervisorRoleSignalCase(t, "FAKEPI_SPAWN_PIDFILE", true)
}

func runSupervisorRoleSignalCase(t *testing.T, descendantEnv string, secondSignal bool) {
	t.Helper()
	setupFakePiEnv(t, inFlightScript("must not complete"))
	piPIDPath := filepath.Join(t.TempDir(), "fakepi.pid")
	descendantPIDPath := filepath.Join(t.TempDir(), "fakepi-descendant.pid")
	t.Setenv("FAKEPI_PIDFILE", piPIDPath)
	t.Setenv(descendantEnv, descendantPIDPath)

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

	piPID := readPIDFile(t, piPIDPath)
	descendantPID := readPIDFile(t, descendantPIDPath)
	if !processAlive(piPID) || !processAlive(descendantPID) {
		t.Fatalf("pi %d / descendant %d are not alive before SIGTERM", piPID, descendantPID)
	}
	// Defensive exact-pid cleanup on failure: no fakepi or descendant may
	// outlive this test.
	t.Cleanup(func() {
		for _, childPID := range []int{descendantPID, piPID} {
			if processAlive(childPID) {
				_ = unix.Kill(childPID, unix.SIGKILL)
			}
		}
		waitProcessGone(t, piPID)
		waitProcessGone(t, descendantPID)
	})

	started := time.Now()
	terminalDeadline := started.Add(e2eTerminalDeadline)
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
		piAlive := processAlive(piPID)
		descendantAlive := processAlive(descendantPID)
		if snap.Terminal && !piAlive && !descendantAlive {
			break
		}
		if time.Now().After(terminalDeadline) {
			t.Fatalf("run did not become terminal and processes did not exit after %s; terminal=%v, state=%q, pi alive=%v (pid %d), descendant alive=%v (pid %d)", time.Since(started), snap.Terminal, snap.State, piAlive, piPID, descendantAlive, descendantPID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	elapsed := time.Since(started)
	if snap.State != RunCancelled {
		t.Fatalf("terminal snapshot after %s has state %q, want %q", elapsed, snap.State, RunCancelled)
	}
	if len(snap.Workers) != 1 {
		t.Fatalf("terminal snapshot after %s has %d workers, want 1", elapsed, len(snap.Workers))
	}
	worker := snap.Workers[0]
	if worker.State != WorkerCancelled {
		t.Fatalf("terminal snapshot after %s has worker state %q, want %q", elapsed, worker.State, WorkerCancelled)
	}
	if worker.FinishedAt == nil {
		t.Fatalf("cancelled terminal worker has no finishedAt after %s", elapsed)
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

// TestSupervisorRoleSIGTERMCleansStdoutHoldingDescendant verifies that a
// descendant holding fake Pi's stdout does not prevent the cancelled run from
// reaching its terminal snapshot and does not survive it.
func TestSupervisorRoleSIGTERMCleansStdoutHoldingDescendant(t *testing.T) {
	runSupervisorRoleSignalCase(t, "FAKEPI_SPAWN_DETACH_STDOUT_PIDFILE", false)
}
