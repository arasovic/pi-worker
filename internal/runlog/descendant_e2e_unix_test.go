//go:build darwin || linux

package runlog

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/run"
)

// TestRecorderRecordsDetachedDescendantThatSurvives is the end-to-end
// guard for the descendant sweeper: a real worker spawns a real
// descendant in its own session, the recorder records that descendant
// while the run is alive, and after the worker is SIGKILLed the record
// still names the survivor, so Leftovers reports it. Without the sweeper
// the descendant survives but the record never names it, and the final
// assertion is where the guard fails.
func TestRecorderRecordsDetachedDescendantThatSurvives(t *testing.T) {
	oldInterval := descendantSweepInterval
	descendantSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { descendantSweepInterval = oldInterval })

	binDir := t.TempDir()
	bin := filepath.Join(binDir, "fakepi")
	build := exec.Command("go", "build", "-o", bin, "github.com/arasovic/pi-worker/internal/testutil/fakepi")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fakepi: %v\n%s", err, out)
	}

	pidFile := filepath.Join(t.TempDir(), "detached.pid")
	cmd := exec.Command(bin, "--mode", "rpc", "--tools", "read")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	cmd.Env = append(os.Environ(), "FAKEPI_SPAWN_DETACH_PIDFILE="+pidFile, "FAKEPI_HOLD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fakepi: %v", err)
	}

	dir := t.TempDir()
	recorder, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	recorder.WorkerProcess(time.Now(), 1, cmd.Process.Pid)

	detachedPID := pollPIDFile(t, pidFile)
	t.Cleanup(func() {
		_ = syscall.Kill(detachedPID, syscall.SIGKILL)
	})

	// Poll the record for the descendant line. Do not fail inside the
	// poll: the SIGKILL below is what the guard needs to reach, and the
	// single failure point is the Leftovers assertion after Finish.
	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		var content []byte
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".jsonl") {
				content, _ = os.ReadFile(filepath.Join(dir, entry.Name()))
			}
		}
		if bytes.Contains(content, []byte(`"event":"descendant"`)) {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Logf("record never named the detached descendant %d before the deadline", detachedPID)
	}

	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
	if err := cmd.Wait(); err == nil {
		t.Fatal("Wait after SIGKILL returned nil")
	}

	result := run.Result{SchemaVersion: 1, Status: "completed", Outcome: "completed"}
	if err := recorder.Finish(time.Now(), &result, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 1 {
		t.Fatalf("leftovers = %+v, want exactly one entry", leftovers)
	}
	if !containsPID(leftovers[0].PIDs, detachedPID) {
		t.Fatalf("leftover pids = %v, want the detached descendant %d", leftovers[0].PIDs, detachedPID)
	}
}

// pollPIDFile polls path until it holds a positive pid.
func pollPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %s never became readable: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// containsPID reports whether pids contains pid.
func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}
