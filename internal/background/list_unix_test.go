//go:build darwin || linux

package background

import (
	"os/exec"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
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
	if err := store.Create(snap); err != nil {
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
