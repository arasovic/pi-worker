//go:build darwin || linux

package background

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// startAndStopProcess starts a short-lived process, records its creation
// time while it is alive, then stops and reaps it, so the pid is provably
// dead by the time ListRuns asks. The recorded pair is exactly what a
// snapshot would carry for a supervisor that has since exited.
func startAndStopProcess(t *testing.T) (int, int64) {
	t.Helper()
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	createTime, err := supervisorPidCreateTime(pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("read creation time of %d: %v", pid, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill %d: %v", pid, err)
	}
	// Kill makes Wait report the signal on some platforms; the process
	// being gone is the point, so the reap error is not a failure.
	_ = cmd.Wait()
	return pid, createTime
}

// TestListRunsNonTerminalDeadSupervisorIsInterrupted requires that a
// non-terminal snapshot whose supervisor is provably dead lists as
// interrupted, by the same liveness answer runlog.List uses.
func TestListRunsNonTerminalDeadSupervisorIsInterrupted(t *testing.T) {
	root := t.TempDir()
	pid, createTime := startAndStopProcess(t)
	acceptedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	snap, err := NewSnapshot(
		"20260901T120000Z-2",
		acceptedAt,
		"/test-workspace",
		ProcessIdentity{PID: pid, CreateTime: createTime},
		[]run.Task{{Prompt: "task", Model: "acme/a", ThinkingLevel: pi.ThinkingLow}},
		time.Minute,
		nil,
	)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := createUnlockedSnapshot(store, snap); err != nil {
		t.Fatalf("Create: %v", err)
	}

	runs, err := ListRuns(root)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want one entry", runs)
	}
	if runs[0].Outcome != "interrupted" {
		t.Fatalf("outcome = %q, want interrupted", runs[0].Outcome)
	}
}

// storeLiveSnapshot stores one non-terminal snapshot under root whose
// supervisor is the given identity and returns its run id.
func storeLiveSnapshot(t *testing.T, root string, supervisor ProcessIdentity) string {
	t.Helper()
	snap, err := NewSnapshot(makeRunID(fixtureTime), fixtureTime, "/test-workspace", supervisor, []run.Task{fixtureTask()}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := createUnlockedSnapshot(store, snap); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return snap.RunID
}

// liveIdentity returns this test process's identity: a supervisor the pid
// rule reads as alive.
func liveIdentity(t *testing.T) ProcessIdentity {
	t.Helper()
	created, err := supervisorPidCreateTime(os.Getpid())
	if err != nil {
		t.Fatalf("observe test process: %v", err)
	}
	return ProcessIdentity{PID: os.Getpid(), CreateTime: created}
}

// holdOwnerLock takes runDir's owner lock for the rest of the test.
func holdOwnerLock(t *testing.T, runDir string) {
	t.Helper()
	f, err := runlog.AcquireOwnerLock(runDir)
	if err != nil {
		t.Fatalf("AcquireOwnerLock: %v", err)
	}
	t.Cleanup(func() { f.Close() })
}

// leaveOwnerLockFree creates runDir's owner lock file and holds nothing on
// it: the owner is gone.
func leaveOwnerLockFree(t *testing.T, runDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(runDir, runlog.OwnerLockName), nil, 0o600); err != nil {
		t.Fatalf("write owner lock: %v", err)
	}
}

// TestListRunsHeldOwnerLockIsRunningEvenWithDeadPid requires that a held
// owner lock lists the run as running although its recorded supervisor pid
// is gone, and that the entry's path is the run directory.
func TestListRunsHeldOwnerLockIsRunningEvenWithDeadPid(t *testing.T) {
	root := t.TempDir()
	pid, createTime := startAndStopProcess(t)
	runID := storeLiveSnapshot(t, root, ProcessIdentity{PID: pid, CreateTime: createTime})
	runDir := filepath.Join(root, runID)
	holdOwnerLock(t, runDir)

	runs, err := ListRuns(root)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != "running" {
		t.Fatalf("runs = %+v, want one running entry", runs)
	}
	if runs[0].Path != runDir {
		t.Fatalf("path = %q, want the run directory %q", runs[0].Path, runDir)
	}
}

// TestListRunsFreeOwnerLockIsInterruptedEvenWithLivePid requires that a free
// owner lock lists the run as interrupted although its recorded supervisor
// pid is alive.
func TestListRunsFreeOwnerLockIsInterruptedEvenWithLivePid(t *testing.T) {
	root := t.TempDir()
	runID := storeLiveSnapshot(t, root, liveIdentity(t))
	leaveOwnerLockFree(t, filepath.Join(root, runID))

	runs, err := ListRuns(root)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != "interrupted" {
		t.Fatalf("runs = %+v, want one interrupted entry", runs)
	}
}
