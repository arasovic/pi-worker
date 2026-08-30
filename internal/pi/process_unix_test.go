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

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
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
	inspectDescendantTargets = func(_ descendantTarget) []descendantTarget {
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

// TestProcessCancellationReleasesStdoutHoldingDescendant is the regression
// for a stdout-holding detached descendant delaying teardown after
// cancellation: the fake Pi spawns a long-lived detached descendant that
// inherits fakepi's fd 1, so the descendant keeps the worker's manual
// stdout pipe open after fakepi itself exits. Cancellation must terminate
// the tree and return from Wait and Close promptly: a descendant holding the
// stdout pipe open must not make teardown hang.
func TestProcessCancellationReleasesStdoutHoldingDescendant(t *testing.T) {
	descendantPidFile := filepath.Join(t.TempDir(), "detached-stdout.pid")
	t.Setenv("FAKEPI_SPAWN_DETACH_STDOUT_PIDFILE", descendantPidFile)

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
	// Cleanup by exact pid even on failure: no fakepi or descendant survives
	// the test.
	t.Cleanup(func() {
		if processAlive(descendantPID) {
			_ = syscall.Kill(descendantPID, syscall.SIGKILL)
		}
		waitProcessGone(t, descendantPID)
		waitProcessGone(t, parentPID)
	})

	cancel()
	started := time.Now()
	if err := proc.Wait(); err == nil {
		t.Fatalf("Wait after cancel returned nil, want kill error")
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("teardown after cancel took %v, want prompt return despite stdout-holding descendant", elapsed)
	}
	waitProcessGone(t, descendantPID)
}

// TestProcessStdoutReadReleasedByTeardownDespiteHoldingDescendant is the
// regression for a read on the child's stdout blocking on a detached
// descendant that inherited the pipe: the manual os.Pipe delivers EOF only
// once every writer has closed its end, and a detached descendant holding
// fakepi's fd 1 is such a writer. Teardown kills the tree, which releases the
// pipe, so a read started while the descendant holds fd 1 must return instead
// of blocking forever.
func TestProcessStdoutReadReleasedByTeardownDespiteHoldingDescendant(t *testing.T) {
	descendantPidFile := filepath.Join(t.TempDir(), "detached-stdout.pid")
	t.Setenv("FAKEPI_SPAWN_DETACH_STDOUT_PIDFILE", descendantPidFile)

	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	descendantPID := readPIDFile(t, descendantPidFile)
	if !processAlive(descendantPID) {
		t.Fatalf("descendant %d is not alive after start", descendantPID)
	}
	// Cleanup by exact pid even on failure: no descendant survives the test.
	t.Cleanup(func() {
		if processAlive(descendantPID) {
			_ = syscall.Kill(descendantPID, syscall.SIGKILL)
		}
		waitProcessGone(t, descendantPID)
	})

	// Start a read before teardown: fakepi has written nothing and the
	// detached descendant still holds fakepi's fd 1, so the read blocks on
	// the manual stdout pipe instead of returning EOF.
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := proc.Stdout().Read(buf)
		readDone <- err
	}()

	// Teardown makes fakepi exit on stdin EOF while the descendant still
	// holds fd 1, then kills the tree and closes the read end, releasing the
	// pipe so the blocked read returns.
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("stdout read blocked after teardown: detached descendant %d kept the stdout pipe open", descendantPID)
	}
	waitProcessGone(t, descendantPID)
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

// TestProcessPidTracksPublishedLifecycle asserts Pid() mirrors
// Running()'s condition: 0 before Start, the child's real number after
// a successful Start, and 0 again after the child is reaped, when the
// number may already have been reused by an unrelated process. The
// expected number never comes from the code under test: the fake writes
// its own process id to the FAKEPI_PIDFILE file, which this test reads
// as the independent source.
func TestProcessPidTracksPublishedLifecycle(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "fakepi.pid")
	t.Setenv("FAKEPI_PIDFILE", pidFile)

	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if got := proc.Pid(); got != 0 {
		t.Fatalf("Pid before Start = %d, want 0", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := proc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if want := readPIDFile(t, pidFile); proc.Pid() != want {
		t.Fatalf("Pid after Start = %d, want %d", proc.Pid(), want)
	}
	cancel()
	if err := proc.Wait(); err == nil {
		t.Fatal("wait after cancellation returned nil")
	}
	if got := proc.Pid(); got != 0 {
		t.Fatalf("Pid after reap = %d, want 0", got)
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := proc.Pid(); got != 0 {
		t.Fatalf("Pid after Close = %d, want 0", got)
	}
}

// TestWorkerOnProcessStartReportsRawWorkerIDExactlyOnce drives a worker
// whose request carries OnProcessStart and asserts the observer is
// called exactly once, with the raw WorkerID — never the debug-label
// normalization that maps a zero identity to worker 1 — and with a
// non-zero pid taken from the independent FAKEPI_PIDFILE record, never
// from the code under test.
func TestWorkerOnProcessStartReportsRawWorkerIDExactlyOnce(t *testing.T) {
	setupFakePiEnv(t, happyPathScript("pid answer"))
	pidFile := filepath.Join(t.TempDir(), "fakepi.pid")
	t.Setenv("FAKEPI_PIDFILE", pidFile)

	var observed []int
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
		// A zero identity: the debug path labels this worker 1, but the
		// observer must receive the raw 0 unchanged.
		OnProcessStart: func(workerID int, pid int) {
			observed = append(observed, workerID, pid)
		},
	})
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed; error = %q", result.Status, result.Error)
	}
	if len(observed) != 2 {
		t.Fatalf("observer calls = %v, want exactly one (workerID, pid) pair", observed)
	}
	if observed[0] != 0 {
		t.Fatalf("observer workerID = %d, want raw 0", observed[0])
	}
	if observed[1] == 0 {
		t.Fatalf("observer pid = 0, want the started child's real pid")
	}
	if want := readPIDFile(t, pidFile); observed[1] != want {
		t.Fatalf("observer pid = %d, want %d", observed[1], want)
	}
}

// TestWorkerOnProcessStartNotCalledWhenStartFails asserts the observer
// is not called when the process fails to start: a process that never
// started has no identity to report.
func TestWorkerOnProcessStartNotCalledWhenStartFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	called := false
	result := New(missing).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
		OnProcessStart: func(workerID int, pid int) {
			called = true
		},
	})
	if result.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable; error = %q", result.Status, result.Error)
	}
	if called {
		t.Fatal("observer called for a process that failed to start")
	}
}
