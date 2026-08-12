//go:build darwin || linux

package pi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"pi-worker/internal/testutil/fakepi/script"
)

// readPIDFile polls path until it holds a positive pid and returns it.
func readPIDFile(t *testing.T, path string) int {
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

// processAlive reports whether a Unix process with the given pid exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// waitProcessGone polls until pid no longer exists. An orphaned descendant
// is reparented and reaped by init, so the poll tolerates a short zombie
// window.
func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d survived cleanup", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProcessTreeTerminatedOnCancellation is the real Unix integration
// regression for inherited process-group cleanup: the fake Pi launches a
// long-lived descendant that retains Pi's group, and cancellation must terminate both
// the parent and the descendant, with the session directory removed.
func TestProcessTreeTerminatedOnCancellation(t *testing.T) {
	descendantPidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv("FAKEPI_SPAWN_PIDFILE", descendantPidFile)

	ctx, cancel := context.WithCancel(context.Background())
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	parentPID := proc.cmd.Process.Pid
	descendantPID := readPIDFile(t, descendantPidFile)
	if !processAlive(descendantPID) {
		t.Fatalf("descendant %d is not alive after start", descendantPID)
	}

	cancel()
	if err := proc.Wait(); err == nil {
		t.Fatalf("Wait after cancel returned nil, want kill error")
	}
	waitProcessGone(t, parentPID)
	waitProcessGone(t, descendantPID)
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(proc.SessionDir()); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after cancellation: %v", err)
	}
}

// TestProcessTreeTerminatedOnForcedClose is the forced-Close regression for
// inherited-group cleanup: a fake Pi that ignores stdin EOF and holds a
// long-lived descendant must lose both processes when Close's grace period
// expires.
func TestProcessTreeTerminatedOnForcedClose(t *testing.T) {
	original := processCloseGrace
	processCloseGrace = 50 * time.Millisecond
	t.Cleanup(func() { processCloseGrace = original })
	t.Setenv("FAKEPI_HOLD", "1")
	descendantPidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv("FAKEPI_SPAWN_PIDFILE", descendantPidFile)

	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	parentPID := proc.cmd.Process.Pid
	descendantPID := readPIDFile(t, descendantPidFile)
	if !processAlive(descendantPID) {
		t.Fatalf("descendant %d is not alive after start", descendantPID)
	}

	started := time.Now()
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("close took %v, want quick kill", elapsed)
	}
	waitProcessGone(t, parentPID)
	waitProcessGone(t, descendantPID)
	if _, err := os.Stat(proc.SessionDir()); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after forced close: %v", err)
	}
}

// TestProcessTreeTerminatedOnNormalChildExit is the real Unix integration
// regression for residual descendants after a normal direct-child exit: fake
// Pi spawns a long-lived descendant and then exits normally on stdin EOF.
// Close must terminate the residual contained tree before releasing the
// containment and returning promptly, with the session directory removed.
func TestProcessTreeTerminatedOnNormalChildExit(t *testing.T) {
	descendantPidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv("FAKEPI_SPAWN_PIDFILE", descendantPidFile)

	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	parentPID := proc.cmd.Process.Pid
	descendantPID := readPIDFile(t, descendantPidFile)
	if !processAlive(descendantPID) {
		t.Fatalf("descendant %d is not alive after start", descendantPID)
	}

	started := time.Now()
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("close took %v, want prompt cleanup of the residual tree", elapsed)
	}
	// The direct child exits normally on stdin EOF: Wait must report the
	// clean exit, not a kill.
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait after normal child exit = %v, want nil", err)
	}
	waitProcessGone(t, parentPID)
	waitProcessGone(t, descendantPID)
	if _, err := os.Stat(proc.SessionDir()); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after normal exit: %v", err)
	}
}

// TestProcessCloseSkipsDescendantSnapshotAfterReapedChild verifies that Close
// does not inspect the process group of a child that has already been
// reaped. This keeps legacy wait-closed PIDs from being interpreted as root
// again and accidentally targeting unrelated descendants.
func TestProcessCloseSkipsDescendantSnapshotAfterReapedChild(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("look up sh: %v", err)
	}
	exitScript := filepath.Join(t.TempDir(), "exit.sh")
	script := "#!" + shPath + "\nexit 0\n"
	if err := os.WriteFile(exitScript, []byte(script), 0o700); err != nil {
		t.Fatalf("write exit script: %v", err)
	}

	originalInspect := inspectDescendantTargets
	inspectCalled := false
	inspectDescendantTargets = func(_ int32) []descendantTarget {
		inspectCalled = true
		return nil
	}
	t.Cleanup(func() {
		inspectDescendantTargets = originalInspect
	})

	proc, err := NewProcess(exitScript, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	proc.mu.Lock()
	reaped := proc.reaped
	proc.mu.Unlock()
	if !reaped {
		t.Fatal("Wait returned before publishing the reaped process identity")
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if inspectCalled {
		t.Fatalf("close inspected descendants after child was already reaped")
	}
}

// TestProcessTreeTerminatedOnNormalChildExitDetachedGroup is the Unix
// integration regression for detached descendants on normal child exit: a
// descendant in a fresh session dies when the direct child exits on stdin EOF.
func TestProcessTreeTerminatedOnNormalChildExitDetachedGroup(t *testing.T) {
	descendantPidFile := filepath.Join(t.TempDir(), "detached.pid")
	t.Setenv("FAKEPI_SPAWN_DETACH_PIDFILE", descendantPidFile)

	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	parentPID := proc.cmd.Process.Pid
	descendantPID := readPIDFile(t, descendantPidFile)
	if !processAlive(descendantPID) {
		t.Fatalf("descendant %d is not alive after start", descendantPID)
	}
	parentPGID, err := syscall.Getpgid(parentPID)
	if err != nil {
		t.Fatalf("getpgid(parent): %v", err)
	}
	descendantPGID, err := syscall.Getpgid(descendantPID)
	if err != nil {
		t.Fatalf("getpgid(descendant): %v", err)
	}
	if descendantPGID == parentPGID {
		t.Fatalf("descendant pgid %d equals parent pgid %d: fixture did not detach", descendantPGID, parentPGID)
	}
	parentSID, err := unix.Getsid(parentPID)
	if err != nil {
		t.Fatalf("getsid(parent): %v", err)
	}
	descendantSID, err := unix.Getsid(descendantPID)
	if err != nil {
		t.Fatalf("getsid(descendant): %v", err)
	}
	if descendantSID == parentSID {
		t.Fatalf("descendant sid %d equals parent sid %d: fixture did not detach session", descendantSID, parentSID)
	}

	started := time.Now()
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("close took %v, want prompt cleanup of detached descendants", elapsed)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait after normal detached child exit = %v, want nil", err)
	}
	waitProcessGone(t, parentPID)
	waitProcessGone(t, descendantPID)
	if _, err := os.Stat(proc.SessionDir()); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after normal detached exit: %v", err)
	}
}

// TestProcessWaitReturnsPromptlyDespiteDetachedStderrDescendant is the
// regression for Process.Wait hanging on an exec stderr copy goroutine:
// the fake Pi spawns a long-lived detached descendant whose stderr is
// explicitly fakepi's stderr, so the descendant inherits and holds fakepi's
// fd 2 after fakepi itself exits. The production invariant is that
// cmd.Stderr stays nil: os/exec then connects child fd 2 directly to
// os.DevNull, so no stderr pipe or copy goroutine exists and Wait returns
// promptly once fakepi exits, independent of the detached descendant. The
// descendant is killed by exact pid in cleanup even on failure, and cleanup
// waits for it to disappear.
func TestProcessWaitReturnsPromptlyDespiteDetachedStderrDescendant(t *testing.T) {
	descendantPidFile := filepath.Join(t.TempDir(), "detached-stderr.pid")
	t.Setenv("FAKEPI_SPAWN_DETACH_STDERR_PIDFILE", descendantPidFile)

	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	parentPID := proc.cmd.Process.Pid
	descendantPID := readPIDFile(t, descendantPidFile)
	if !processAlive(descendantPID) {
		t.Fatalf("descendant %d is not alive after start", descendantPID)
	}
	// Cleanup by exact pid even on failure: no fakepi or descendant survives
	// the test.
	t.Cleanup(func() {
		if processAlive(descendantPID) {
			_ = syscall.Kill(descendantPID, syscall.SIGKILL)
		}
		waitProcessGone(t, descendantPID)
		waitProcessGone(t, parentPID)
	})

	// Close drives the real lifecycle: stdin EOF makes fakepi exit normally
	// while the detached descendant keeps the inherited stderr fd open. It
	// runs in the background; only Wait promptness is asserted here.
	closeDone := make(chan error, 1)
	go func() { closeDone <- proc.Close() }()

	waitDone := make(chan error, 1)
	go func() { waitDone <- proc.Wait() }()

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait after normal child exit = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return within 2s of fakepi exit: detached stderr descendant %d still holds fd 2 open", descendantPID)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Close did not finish within 5s after Wait returned")
	}
}

// TestProcessTreeTerminatedOnCancellationDetachedGroup is the real Unix
// integration regression for the observed production leak: Pi's built-in
// bash tool starts commands in a different process group, so a group-only
// kill leaves them running. The fake Pi spawns a long-lived descendant in
// its own session (and process group), outside the inherited boundary;
// cancellation must terminate both Pi and that detached descendant, and
// remove the session directory.
func TestProcessTreeTerminatedOnCancellationDetachedGroup(t *testing.T) {
	descendantPidFile := filepath.Join(t.TempDir(), "detached.pid")
	t.Setenv("FAKEPI_SPAWN_DETACH_PIDFILE", descendantPidFile)

	ctx, cancel := context.WithCancel(context.Background())
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	parentPID := proc.cmd.Process.Pid
	descendantPID := readPIDFile(t, descendantPidFile)
	if !processAlive(descendantPID) {
		t.Fatalf("descendant %d is not alive after start", descendantPID)
	}
	// Prove the descendant really left the inherited boundary: it must not
	// share Pi's process group, so a group-only kill cannot reach it.
	parentPGID, err := syscall.Getpgid(parentPID)
	if err != nil {
		t.Fatalf("getpgid(parent): %v", err)
	}
	descendantPGID, err := syscall.Getpgid(descendantPID)
	if err != nil {
		t.Fatalf("getpgid(descendant): %v", err)
	}
	if descendantPGID == parentPGID {
		t.Fatalf("descendant pgid %d equals parent pgid %d: fixture did not detach", descendantPGID, parentPGID)
	}

	cancel()
	if err := proc.Wait(); err == nil {
		t.Fatalf("Wait after cancel returned nil, want kill error")
	}
	waitProcessGone(t, parentPID)
	waitProcessGone(t, descendantPID)
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(proc.SessionDir()); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after cancellation: %v", err)
	}
}

// TestWorkerTimeoutTerminatesDescendantTree is the real worker-level
// timeout regression for inherited-group cleanup: a fake Pi that holds a
// long-lived descendant while sleeping through the run deadline must lose
// both processes, and the worker must remove the session directory and
// report timed-out.
func TestWorkerTimeoutTerminatesDescendantTree(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{SleepMS: 10000},
		},
	}}
	setupFakePiEnv(t, scriptConfig)
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	pidFile := filepath.Join(t.TempDir(), "fakepi.pid")
	t.Setenv("FAKEPI_PIDFILE", pidFile)
	descendantPidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv("FAKEPI_SPAWN_PIDFILE", descendantPidFile)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result := New(fakePiBin).Run(ctx, WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})
	if result.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed-out; error = %q", result.Status, result.Error)
	}
	if result.Error == "" {
		t.Fatalf("timed-out result must carry an error message")
	}

	parentPID := readPIDFile(t, pidFile)
	descendantPID := readPIDFile(t, descendantPidFile)
	waitProcessGone(t, parentPID)
	waitProcessGone(t, descendantPID)
	if _, err := os.Stat(sessionDirFromMeta(t, metaPath)); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after timeout: %v", err)
	}
}
