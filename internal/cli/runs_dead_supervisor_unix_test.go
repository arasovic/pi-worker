//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// TestRunBackgroundReportsAKilledSupervisorAsAnInterruptedRun requires that a
// background run whose supervisor was killed without warning is reported by
// the next start as an interrupted earlier run, naming its record: the record
// the supervisor wrote has no finish line and its process is gone.
func TestRunBackgroundReportsAKilledSupervisorAsAnInterruptedRun(t *testing.T) {
	manager, _ := setupBackgroundRun(t, slowBackgroundScript("never finished", backgroundRunDelayStep))
	recordsDir := t.TempDir()
	withRunlogDir(t, recordsDir)

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
	want := "pi-worker: warning: an earlier run was interrupted: " + filepath.Join(recordsDir, runID+".jsonl") + "\n"
	if !strings.Contains(stderr, want) {
		t.Fatalf("second start stderr = %q, want %q for the run whose supervisor %d was killed", stderr, want, snap.Supervisor.PID)
	}
}

// killSupervisorOnceRunning waits until the run's worker is running, kills the
// run's supervisor with SIGKILL, and waits until the kill is visible.
func killSupervisorOnceRunning(t *testing.T, manager *background.Manager, runID string) background.Snapshot {
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
