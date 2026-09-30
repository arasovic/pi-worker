//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// TestRunBackgroundReportsAKilledSupervisorAsAnInterruptedRun requires that a
// background run whose supervisor was killed without warning is reported by
// the next start as an interrupted earlier run, naming its run directory: its
// snapshot is not terminal and its owner lock is free.
func TestRunBackgroundReportsAKilledSupervisorAsAnInterruptedRun(t *testing.T) {
	manager, recordsDir := setupBackgroundRun(t, slowBackgroundScript("never finished", backgroundRunDelayStep))

	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--json", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("run --background = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	runID, ok := decodeJSONObject(t, stdout)["runId"].(string)
	if !ok || runID == "" {
		t.Fatalf("accepted run reported no identity: %q", stdout)
	}
	snap := killSupervisorOnceRunning(t, manager, runID)
	// A run id is the start second plus the starting process's pid, and both
	// starts run in this one process: the second start must fall in a later
	// second or it would name the first run.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))

	code, stdout, stderr = runCLI(t, []string{"run", "--background", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("second run --background = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	secondID := runIDFromHumanOutput(t, stdout)
	t.Cleanup(func() {
		if _, err := manager.Wait(context.Background(), secondID, 90*time.Second); err != nil {
			t.Errorf("drain run %s: %v", secondID, err)
		}
	})
	want := "pi-worker: warning: an earlier run was interrupted: " + filepath.Join(recordsDir, runID) + "\n"
	if !strings.Contains(stderr, want) {
		t.Fatalf("second start stderr = %q, want %q for the run whose supervisor %d was killed", stderr, want, snap.Supervisor.PID)
	}
}

// killSupervisorOnceRunning waits until the run's worker is running, kills the
// run's supervisor with SIGKILL, and waits until the kill is visible.
func killSupervisorOnceRunning(t *testing.T, manager *background.Manager, runID string) background.Snapshot {
	t.Helper()
	snap := waitForRunningWorker(t, manager, runID)
	if err := syscall.Kill(snap.Supervisor.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill supervisor %d: %v", snap.Supervisor.PID, err)
	}
	// The supervisor is a child of this test process, released at detach
	// and never reaped: after the kill it lingers as a zombie that still
	// answers the liveness check, so reap it while waiting for the death
	// to become visible.
	pollDeadline := time.Now().Add(10 * time.Second)
	for runlog.ProcessAlive(snap.Supervisor.PID, snap.Supervisor.CreateTime) {
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(snap.Supervisor.PID, &ws, syscall.WNOHANG, nil)
		if time.Now().After(pollDeadline) {
			t.Fatalf("supervisor %d still alive after SIGKILL", snap.Supervisor.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return snap
}

// waitForRunningWorker waits until the run's one worker is running with its
// process recorded, and returns that snapshot.
func waitForRunningWorker(t *testing.T, manager *background.Manager, runID string) background.Snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var snap background.Snapshot
	for {
		var err error
		snap, err = manager.Status(runID)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if len(snap.Workers) == 1 && snap.Workers[0].State == background.WorkerRunning && snap.Workers[0].Process != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker never reached running state; last snapshot = %+v", snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return snap
}

// ownerLockHolders returns the pids that have path open, or nil with ok false
// when lsof is not installed.
func ownerLockHolders(t *testing.T, path string) (pids []int, ok bool) {
	t.Helper()
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil, false
	}
	out, err := exec.Command(lsof, "-t", "--", path).Output()
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && len(out) == 0) {
		t.Fatalf("lsof %s: %v", path, err)
	}
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("lsof pid %q: %v", field, err)
		}
		pids = append(pids, pid)
	}
	return pids, true
}

// TestRunDirectoryOwnerLockEndToEnd drives a real background run and pins the
// run directory layout and its owner lock: <records>/<id>/ holds owner.lock,
// snapshot.json and record.jsonl, the lock is held by the supervisor alone —
// no worker host inherits it — and the older background directory is never
// created. Once the supervisor is killed outright, without being reaped, the
// lock is free at once, runs list reports the run interrupted, runs wait
// exits 9 saying the supervisor is gone, and the worker is cleaned up.
func TestRunDirectoryOwnerLockEndToEnd(t *testing.T) {
	manager, root := setupBackgroundRun(t, slowBackgroundScript("never finished", backgroundRunDelayStep))
	legacyRoot := filepath.Join(t.TempDir(), "background")
	withBackgroundRoot(t, legacyRoot)

	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--json", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("run --background = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	runID, ok := decodeJSONObject(t, stdout)["runId"].(string)
	if !ok || runID == "" {
		t.Fatalf("accepted run reported no identity: %q", stdout)
	}
	snap := waitForRunningWorker(t, manager, runID)
	supervisor := snap.Supervisor.PID
	t.Cleanup(func() {
		_ = syscall.Kill(supervisor, syscall.SIGKILL)
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(supervisor, &ws, 0, nil)
	})

	runDir := filepath.Join(root, runID)
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("read run directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	for _, name := range []string{"owner.lock", "record.jsonl", "snapshot.json"} {
		if !slices.Contains(names, name) {
			t.Fatalf("run directory holds %v, want %s among them", names, name)
		}
	}
	if got := runlog.ProbeOwnerLock(runDir); got != runlog.LockHeld {
		t.Fatalf("owner lock of a running run = %v, want held", got)
	}
	lockPath := filepath.Join(runDir, runlog.OwnerLockName)
	if holders, ok := ownerLockHolders(t, lockPath); ok && !slices.Equal(holders, []int{supervisor}) {
		t.Fatalf("owner lock held open by %v, want only the supervisor %d", holders, supervisor)
	}
	if _, err := os.Lstat(legacyRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("older background directory %s: %v, want it never created", legacyRoot, err)
	}

	if err := syscall.Kill(supervisor, syscall.SIGKILL); err != nil {
		t.Fatalf("kill supervisor %d: %v", supervisor, err)
	}
	// No reaping here: the kernel releases the lock when the process exits,
	// before anyone collects its exit status.
	deadline := time.Now().Add(5 * time.Second)
	for runlog.ProbeOwnerLock(runDir) != runlog.LockFree {
		if time.Now().After(deadline) {
			t.Fatalf("owner lock still %v after SIGKILL of the supervisor", runlog.ProbeOwnerLock(runDir))
		}
		time.Sleep(10 * time.Millisecond)
	}

	code, stdout, stderr = runCLI(t, []string{"runs", "list", "--json"}, "")
	if code != 0 {
		t.Fatalf("runs list = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	var document struct {
		Runs []listedRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode runs list: %v\n%s", err, stdout)
	}
	if len(document.Runs) != 1 || document.Runs[0].RunID != runID || document.Runs[0].Outcome != "interrupted" || document.Runs[0].Path != runDir {
		t.Fatalf("runs list = %+v, want %s interrupted at %s", document.Runs, runID, runDir)
	}

	code, stdout, stderr = runCLI(t, []string{"runs", "wait", runID, "--timeout", "30s"}, "")
	if code != 9 || !strings.Contains(stderr, "pi-worker: runs wait "+runID+": supervisor is no longer there; the run was interrupted and will not finish") {
		t.Fatalf("runs wait = (%d, %q, %q), want exit 9 with the supervisor-is-gone line", code, stdout, stderr)
	}

	worker := snap.Workers[0].Process
	deadline = time.Now().Add(10 * time.Second)
	for runlog.ProcessAlive(worker.PID, worker.CreateTime) {
		if time.Now().After(deadline) {
			t.Fatalf("worker process %d still alive after its supervisor was killed", worker.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if holders, ok := ownerLockHolders(t, lockPath); ok && len(holders) != 0 {
		t.Fatalf("owner lock still open in %v after the supervisor was killed", holders)
	}
}

// TestRunsDeadSupervisorStatusAndWaitReportInterrupted requires that killing
// a run's supervisor makes `runs status` and `runs wait` print the latest
// non-terminal state, say the supervisor is gone, and exit 9 — with the wait
// returning at once instead of running out its bound.
func TestRunsDeadSupervisorStatusAndWaitReportInterrupted(t *testing.T) {
	manager, _ := setupBackgroundRun(t, slowBackgroundScript("never finished", backgroundRunDelayStep))
	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--json", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("run --background = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	runID, ok := decodeJSONObject(t, stdout)["runId"].(string)
	if !ok || runID == "" {
		t.Fatalf("accepted run reported no identity: %q", stdout)
	}

	killSupervisorOnceRunning(t, manager, runID)

	code, stdout, stderr = runCLI(t, []string{"runs", "status", runID, "--json"}, "")
	if code != 9 {
		t.Fatalf("runs status = (%d, %q, %q), want exit 9", code, stdout, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &doc); err != nil {
		t.Fatalf("runs status stdout is not one JSON line: %q: %v", stdout, err)
	}
	if doc["terminal"] != false {
		t.Fatalf("runs status document terminal = %v, want false; stdout = %q", doc["terminal"], stdout)
	}
	if !strings.Contains(stderr, "supervisor is no longer there") {
		t.Fatalf("runs status stderr = %q, want supervisor-is-gone note", stderr)
	}

	started := time.Now()
	code, stdout, stderr = runCLI(t, []string{"runs", "wait", runID, "--timeout", "30s", "--json"}, "")
	elapsed := time.Since(started)
	if code != 9 {
		t.Fatalf("runs wait = (%d, %q, %q), want exit 9, not 7", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "supervisor is no longer there") {
		t.Fatalf("runs wait stderr = %q, want supervisor-is-gone note", stderr)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("runs wait took %v; it must return at once", elapsed)
	}
}
