//go:build darwin || linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// processGroupMembers lists the processes in process group pgid.
func processGroupMembers(t *testing.T, pgid int) []int {
	t.Helper()
	out, err := exec.Command("pgrep", "-g", strconv.Itoa(pgid)).Output()
	if err != nil {
		t.Fatalf("pgrep -g %d: %v", pgid, err)
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("pgrep -g %d printed %q", pgid, out)
		}
		pids = append(pids, pid)
	}
	return pids
}

// killRunProcesses kills a run's supervisor with the worker host in its
// process group, and the run's Pi in its own group, and requires every one
// of them but the supervisor gone. The supervisor is this process's child,
// so only its reaping, which the command under test does, lets it go.
func killRunProcesses(t *testing.T, supervisorPID, piPID int) {
	t.Helper()
	members := processGroupMembers(t, supervisorPID)
	if len(members) < 2 {
		t.Fatalf("supervisor group %d = %v, want the supervisor and its worker host", supervisorPID, members)
	}
	_ = syscall.Kill(-supervisorPID, syscall.SIGKILL)
	_ = syscall.Kill(-piPID, syscall.SIGKILL)
	for _, pid := range append(members, piPID) {
		if pid != supervisorPID {
			waitForProcessGone(t, "run process", pid, 10*time.Second)
		}
	}
}

// TestRunReportsAKilledSupervisorAtOnce requires that a `run` whose
// supervisor is killed mid-run exits 9 within seconds, with no document. The
// waiting command is the supervisor's parent: unless it reaps the killed
// child, the child stays a zombie that still passes the liveness check and
// the wait never ends.
func TestRunReportsAKilledSupervisorAtOnce(t *testing.T) {
	newGitWorkspace(t)
	manager := useFakePi(t, heldHappyScript("never finished", 60_000))
	pidFile := filepath.Join(t.TempDir(), "pi.pid")
	t.Setenv("FAKEPI_PIDFILE", pidFile)

	r := startInFlightRun(context.Background(), []string{"run", "--json", "--model", "acme/m-1", "--task", "go"})
	runID := r.runID(t)
	waitForRequestLog(t, os.Getenv("FAKEPI_LOG"), "prompt")
	piPID := readPIDFile(t, pidFile)
	snap, err := manager.Status(runID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	supervisorPID := snap.Supervisor.PID
	killRunProcesses(t, supervisorPID, piPID)

	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		// Unblock the command so the test can end, then fail it.
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(supervisorPID, &status, 0, nil)
		<-r.done
		t.Fatalf("run still waiting 10s after its supervisor was killed; stderr = %q", r.stderr.String())
	}
	if r.code != 9 || r.stdout.String() != "" {
		t.Fatalf("run = (%d, %q, %q), want exit 9 and no document", r.code, r.stdout.String(), r.stderr.String())
	}
	if !strings.Contains(r.stderr.String(), "pi-worker: run "+runID+": supervisor is no longer there") {
		t.Fatalf("stderr = %q, want the supervisor-gone line", r.stderr.String())
	}
	waitForProcessGone(t, "supervisor", supervisorPID, 5*time.Second)
}

// TestRunReportsAnUnreadableRunAsAFailure requires that a `run` whose stored
// state can no longer be read exits 9 with no document.
func TestRunReportsAnUnreadableRunAsAFailure(t *testing.T) {
	newGitWorkspace(t)
	manager := useFakePi(t, heldHappyScript("never finished", 60_000))
	pidFile := filepath.Join(t.TempDir(), "pi.pid")
	t.Setenv("FAKEPI_PIDFILE", pidFile)

	r := startInFlightRun(context.Background(), []string{"run", "--json", "--model", "acme/m-1", "--task", "go"})
	runID := r.runID(t)
	waitForRequestLog(t, os.Getenv("FAKEPI_LOG"), "prompt")
	piPID := readPIDFile(t, pidFile)
	snap, err := manager.Status(runID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	t.Cleanup(func() { killRunProcesses(t, snap.Supervisor.PID, piPID) })
	if err := os.RemoveAll(filepath.Dir(manager.DebugLogPath(runID))); err != nil {
		t.Fatalf("remove stored state: %v", err)
	}

	r.finish(t, 10*time.Second)
	if r.code != 9 || r.stdout.String() != "" {
		t.Fatalf("run = (%d, %q, %q), want exit 9 and no document", r.code, r.stdout.String(), r.stderr.String())
	}
	if !strings.Contains(r.stderr.String(), "pi-worker: read run "+runID+": ") {
		t.Fatalf("stderr = %q, want the read failure", r.stderr.String())
	}
}

// setupSupervisedBinary prepares this test to run the built binary as a
// process of its own: a scratch home and configuration tree, a `pi` on PATH
// that is the fake Pi answering s, and a scratch workspace. It returns the
// binary and a manager reading the state that binary writes.
func setupSupervisedBinary(t *testing.T, s *script.Script) (string, *background.Manager) {
	t.Helper()
	bin := piWorkerBinForBackground(t)
	setupFakePiScript(t, s)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	piDir := t.TempDir()
	if err := os.Symlink(fakePiBin, filepath.Join(piDir, "pi")); err != nil {
		t.Fatalf("link fake Pi: %v", err)
	}
	t.Setenv("PATH", piDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(t.TempDir())
	root, err := background.DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	manager, err := background.NewManager(root, t.TempDir(), 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return bin, manager
}

// TestRunKilledWaiterLeavesTheRunGoing requires that killing the waiting
// `run` — its whole process group, as closing its terminal would — leaves
// the run going: it still finishes completed.
func TestRunKilledWaiterLeavesTheRunGoing(t *testing.T) {
	bin, manager := setupSupervisedBinary(t, heldHappyScript("done", 2_000))
	var stderr lockedBuffer
	cmd := exec.Command(bin, "run", "--model", "acme/m-1", "--task", "go")
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start run: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	waitForRequestLog(t, os.Getenv("FAKEPI_LOG"), "prompt")
	match := runLinePattern.FindStringSubmatch(stderr.String())
	if match == nil {
		t.Fatalf("stderr = %q, want the run line before the worker is prompted", stderr.String())
	}
	runID := match[1]
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the waiting command's group: %v", err)
	}
	_ = cmd.Wait()

	snap, err := manager.Wait(context.Background(), runID, 60*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !snap.Terminal || snap.Outcome == nil || *snap.Outcome != contracts.OutcomeCompleted {
		t.Fatalf("run terminal=%v outcome=%v, want terminal completed", snap.Terminal, snap.Outcome)
	}
}

// TestRunNeverReportsANormallyExitingSupervisorAsGone requires that a run
// whose supervisor exits normally is always reported by its stored result:
// the waiting command reaps the supervisor as it exits, and the wait must
// still find the terminal state rather than a supervisor that is gone.
func TestRunNeverReportsANormallyExitingSupervisorAsGone(t *testing.T) {
	bin, _ := setupSupervisedBinary(t, backgroundHappyScript("done"))
	const runs, lanes = 20, 4
	failures := make(chan string, runs)
	lane := make(chan struct{}, lanes)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		lane <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-lane }()
			cmd := exec.Command(bin, "run", "--model", "acme/m-1", "--task", "go")
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if err != nil || strings.Contains(stderr.String(), "supervisor is no longer there") || !strings.Contains(stdout.String(), "outcome=completed") {
				failures <- fmt.Sprintf("run: %v; stdout = %q; stderr = %q", err, stdout.String(), stderr.String())
			}
		}()
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}
