//go:build darwin || linux

package background

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/run"
	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

// sessionStarterEnv switches TestSessionStarterHelper into the starter it
// plays for TestSupervisorIgnoresInterruptToStarterProcessGroup; each value
// names what the starter needs, separated by sessionStarterSep.
const (
	sessionStarterEnv = "PI_WORKER_TEST_SESSION_STARTER"
	sessionStarterSep = "\x1f"
	sessionRunIDLine  = "runid="
)

// TestSupervisorRunsInItsOwnSession requires the detached supervisor to lead
// a new session apart from the command that started it, and the worker host
// it starts to stay in that session.
func TestSupervisorRunsInItsOwnSession(t *testing.T) {
	_, root, _, started := startManagedRun(t, inFlightScript("session answer"))
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var snap Snapshot
	deadline := time.Now().Add(e2eTerminalDeadline)
	for {
		snap, err = store.Load(started.RunID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(snap.Workers) == 1 && snap.Workers[0].State == WorkerRunning && snap.Workers[0].Process != nil {
			break
		}
		if snap.Terminal || time.Now().After(deadline) {
			t.Fatalf("worker never observed running; last snapshot = %+v", snap)
		}
		time.Sleep(20 * time.Millisecond)
	}

	supervisorPID := snap.Supervisor.PID
	callerSID, err := unix.Getsid(0)
	if err != nil {
		t.Fatalf("Getsid(caller): %v", err)
	}
	supervisorSID, err := unix.Getsid(supervisorPID)
	if err != nil {
		t.Fatalf("Getsid(supervisor %d): %v", supervisorPID, err)
	}
	if supervisorSID != supervisorPID || supervisorSID == callerSID {
		t.Fatalf("supervisor %d session = %d, caller session = %d; want the supervisor to lead its own session", supervisorPID, supervisorSID, callerSID)
	}

	// The recorded worker process is Pi; its parent is the worker host.
	piPID := snap.Workers[0].Process.PID
	pi, err := process.NewProcess(int32(piPID))
	if err != nil {
		t.Fatalf("inspect pi %d: %v", piPID, err)
	}
	hostPID32, err := pi.Ppid()
	if err != nil {
		t.Fatalf("parent of pi %d: %v", piPID, err)
	}
	hostPID := int(hostPID32)
	if hostPID == supervisorPID || hostPID == os.Getpid() || hostPID <= 1 {
		t.Fatalf("parent of pi %d is %d, want a worker host distinct from supervisor %d and caller %d", piPID, hostPID, supervisorPID, os.Getpid())
	}
	hostSID, err := unix.Getsid(hostPID)
	if err != nil {
		t.Fatalf("Getsid(worker host %d): %v", hostPID, err)
	}
	if hostSID != supervisorPID {
		t.Fatalf("worker host %d session = %d, want the supervisor's session %d (caller session %d)", hostPID, hostSID, supervisorPID, callerSID)
	}
}

// TestSupervisorIgnoresInterruptToStarterProcessGroup is a terminal Ctrl-C
// in miniature: the starting command runs in its own process group, and a
// SIGINT to that group after acceptance must leave the run to finish as
// completed.
func TestSupervisorIgnoresInterruptToStarterProcessGroup(t *testing.T) {
	setupFakePiEnv(t, inFlightScript("interrupt answer"))
	m, root, admissionRoot := newTestManager(t)
	fields := []string{root, admissionRoot, t.TempDir(), fakePiBin(t), piWorkerBin(t)}

	starter := exec.Command(os.Args[0], "-test.run=^TestSessionStarterHelper$")
	starter.Env = append(os.Environ(), sessionStarterEnv+"="+strings.Join(fields, sessionStarterSep))
	starter.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := starter.StdoutPipe()
	if err != nil {
		t.Fatalf("starter stdout: %v", err)
	}
	var stderr strings.Builder
	starter.Stderr = &stderr
	if err := starter.Start(); err != nil {
		t.Fatalf("start starter: %v", err)
	}
	starterPGID := starter.Process.Pid
	exited := make(chan error, 1)
	t.Cleanup(func() {
		if err := unix.Kill(-starterPGID, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			t.Errorf("kill starter group %d: %v", starterPGID, err)
		}
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Errorf("starter %d did not exit", starterPGID)
		}
	})

	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if id, ok := strings.CutPrefix(scanner.Text(), sessionRunIDLine); ok {
				lines <- id
			}
		}
		close(lines)
		exited <- starter.Wait()
	}()
	var runID string
	select {
	case id, ok := <-lines:
		if !ok {
			t.Fatalf("starter exited without accepting a run; stderr = %q", stderr.String())
		}
		runID = id
	case <-time.After(60 * time.Second):
		t.Fatalf("starter did not accept a run in time; stderr = %q", stderr.String())
	}

	if err := unix.Kill(-starterPGID, unix.SIGINT); err != nil {
		t.Fatalf("SIGINT starter group %d: %v", starterPGID, err)
	}

	snap, err := m.Wait(context.Background(), runID, managerTerminalWait)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !snap.Terminal || snap.State != RunCompleted {
		t.Fatalf("run after SIGINT to the starter's group: terminal=%v state=%q, want completed", snap.Terminal, snap.State)
	}
}

// TestSessionStarterHelper is the starting command of
// TestSupervisorIgnoresInterruptToStarterProcessGroup, run in a child
// process. It starts one run, reports its identity, and waits to be
// signalled. Without its environment it does nothing.
func TestSessionStarterHelper(t *testing.T) {
	value := os.Getenv(sessionStarterEnv)
	if value == "" {
		t.Skip("runs only as the starter child of TestSupervisorIgnoresInterruptToStarterProcessGroup")
	}
	fields := strings.Split(value, sessionStarterSep)
	if len(fields) != 5 {
		t.Fatalf("%s carries %d fields, want 5", sessionStarterEnv, len(fields))
	}
	root, admissionRoot, workspace, piBin, roleBin := fields[0], fields[1], fields[2], fields[3], fields[4]
	m, err := NewManager(root, "", admissionRoot, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	opts := StartOptions{
		Tasks:            []run.Task{{Prompt: "run past an interrupt", Model: "acme/m-1"}},
		Workspace:        workspace,
		ExecutionTimeout: 5 * time.Minute,
		PiExecutable:     piBin,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started, err := m.startWithExecutable(ctx, roleBin, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	fmt.Printf("%s%s\n", sessionRunIDLine, started.RunID)
	// A bare select {} would trip the runtime deadlock detector.
	for {
		time.Sleep(24 * time.Hour)
	}
}
