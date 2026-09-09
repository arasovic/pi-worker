//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/arasovic/pi-worker/internal/pi"
)

// errWorkerHostKillFailure is the error the adapter child's failing kill
// seam returns while the seam's fail flag is set.
var errWorkerHostKillFailure = errors.New("forced kill failure")

// failingWorkerHostKill is the state of one installed kill-failure seam:
// the adapter's own child observed through workerHostStartProcess, whose
// per-instance killProcess seam fails while fail is set. pid and fail are
// atomic because the seam goroutine writes them while the test goroutine
// reads them, and the cleanup reads the pid even when the adapter never
// returned.
type failingWorkerHostKill struct {
	fail    atomic.Bool
	pid     atomic.Int64
	spawned chan struct{} // buffered; one signal once the child is recorded
}

// installFailingWorkerHostKill installs the workerHostStartProcess seam so
// a test reaches the *roleProcess the adapter spawns itself: the child's
// killProcess seam fails while the returned state's fail flag is set, so
// the adapter's bounded fallback kill runs against a host that stays
// alive. The fail flag is the only thing a test flips afterwards, so no
// field the seam reads is ever rewritten while goroutines are running.
// Cleanup restores the seam, flips the flag off, SIGKILLs the exact child
// pid and reaps it, and verifies the pid is gone: a host the seam kept
// alive on purpose must never outlive the test.
func installFailingWorkerHostKill(t *testing.T) *failingWorkerHostKill {
	t.Helper()
	seam := &failingWorkerHostKill{spawned: make(chan struct{}, 1)}
	seam.fail.Store(true)

	oldStart := workerHostStartProcess
	workerHostStartProcess = func(executable string, r role) (*roleProcess, error) {
		p, err := oldStart(executable, r)
		if err != nil || p == nil || p.cmd == nil || p.cmd.Process == nil {
			return p, err
		}
		seam.pid.Store(int64(p.cmd.Process.Pid))
		p.killProcess = func() error {
			if seam.fail.Load() {
				return errWorkerHostKillFailure
			}
			return p.cmd.Process.Kill()
		}
		select {
		case seam.spawned <- struct{}{}:
		default:
		}
		return p, nil
	}

	t.Cleanup(func() {
		workerHostStartProcess = oldStart
		// The kill must never fail again: the adapter's own deferred Close
		// and the raw syscalls below really end the child from here on.
		seam.fail.Store(false)
		pid := int(seam.pid.Load())
		if pid == 0 {
			return
		}
		// Raw syscalls reach the exact child whether or not the adapter
		// already reaped it, and they never go through the seam.
		if killErr := unix.Kill(pid, unix.SIGKILL); killErr != nil && !errors.Is(killErr, unix.ESRCH) {
			t.Errorf("cleanup kill worker host %d: %v", pid, killErr)
		}
		if _, waitErr := unix.Wait4(pid, nil, 0, nil); waitErr != nil && !errors.Is(waitErr, unix.ECHILD) {
			t.Errorf("cleanup reap worker host %d: %v", pid, waitErr)
		}
		waitProcessGone(t, pid)
	})
	return seam
}

// waitWorkerHostSpawn waits for the seam to record the adapter's child, so
// the recorded pid is visible to the test goroutine.
func waitWorkerHostSpawn(t *testing.T, seam *failingWorkerHostKill) {
	t.Helper()
	select {
	case <-seam.spawned:
	case <-time.After(10 * time.Second):
		t.Fatal("the adapter's worker host child was not spawned within the bounded wait")
	}
}

// requireUnkilledHostResult asserts one result reporting a fallback kill
// that failed: the host could not be killed and may still be running, with
// its Pi cleanup outcome uncertain — never a completed force-kill.
func requireUnkilledHostResult(t *testing.T, result pi.WorkerResult) {
	t.Helper()
	for _, want := range []string{"could not be killed", "may still be running", "uncertain"} {
		if !strings.Contains(result.Error, want) {
			t.Fatalf("error %q does not report %q", result.Error, want)
		}
	}
	if strings.Contains(result.Error, "force-killed") {
		t.Fatalf("result %q claims a completed force-kill the failed kill never performed", result.Error)
	}
}

// TestWorkerHostAdapterDrainTimerFailedKillReportsUncertainty mirrors
// TestWorkerHostAdapterBoundedFallbackKillReportsUncertainty with the
// fallback kill failing: a stuck host ignores ownership EOF, the drain
// timer fires at the shortened cleanup bound, and the kill the adapter
// performs cannot remove the host. The host is then still alive and still
// holds its response writer open, so the in-flight Receive can only be
// released by the parent closing its own read end: Run must return within
// the bound and report that the host could not be killed and may still be
// running instead of claiming a completed force-kill. Before the fix this
// branch drained the in-flight receive unconditionally and blocked on a
// pipe the live host never writes, until the test deadline.
func TestWorkerHostAdapterDrainTimerFailedKillReportsUncertainty(t *testing.T) {
	oldBound := workerHostCleanupBound
	workerHostCleanupBound = 400 * time.Millisecond
	t.Cleanup(func() { workerHostCleanupBound = oldBound })

	seam := installFailingWorkerHostKill(t)

	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	t.Setenv(workerHostStuckEnv, "1")
	stuckPIDPath := filepath.Join(t.TempDir(), "stuck-host.pid")
	t.Setenv(workerHostStuckPIDFileEnv, stuckPIDPath)

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	runDone := make(chan pi.WorkerResult, 1)
	go func() {
		runDone <- a.Run(ctx, adapterWorkerRequest(t.TempDir()))
	}()
	waitWorkerHostSpawn(t, seam)
	stuckPID := readPIDFile(t, stuckPIDPath)
	if hostPID := int(seam.pid.Load()); stuckPID != hostPID {
		t.Fatalf("stuck host pid = %d, want the adapter's own child %d", stuckPID, hostPID)
	}
	if !processAlive(stuckPID) {
		t.Fatalf("stuck host %d is not alive after start", stuckPID)
	}
	// No kill cleanup is registered here on purpose: the seam cleanup
	// already SIGKILLs this exact pid, reaps it and verifies it is gone.
	// A body cleanup that only killed would leave a zombie the adapter
	// never reaped — its own Close went through the failing seam.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case result := <-runDone:
		if result.Status != pi.StatusCancelled {
			t.Fatalf("result = %+v, want cancelled", result)
		}
		requireUnkilledHostResult(t, result)
	case <-time.After(15 * time.Second):
		t.Fatal("adapter Run did not return after the bounded fallback with a failed kill")
	}
}

// TestWorkerHostAdapterReceiveEOFFailedKillReportsUncertainty runs a host
// that closes its response writer and then lives forever, so the adapter's
// first Receive returns EOF at once and execute takes the receive-error
// path into reapWorkerHost with the child still alive: the cleanup bound
// expires and the fallback kill fails. reapWorkerHost must report the host
// as unkilled immediately instead of waiting for an exit only the still
// running child can produce, so Run returns within the bound and states
// that the host could not be killed and may still be running. The context
// is deliberately never cancelled here — a pre-cancelled context would
// race the phase-1 select between ctx.Done and the completed send, and the
// drain-timer branch above already covers cancellation.
func TestWorkerHostAdapterReceiveEOFFailedKillReportsUncertainty(t *testing.T) {
	oldBound := workerHostCleanupBound
	workerHostCleanupBound = 400 * time.Millisecond
	t.Cleanup(func() { workerHostCleanupBound = oldBound })

	seam := installFailingWorkerHostKill(t)

	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	t.Setenv(workerHostClosedResponseEnv, "1")
	hostPIDPath := filepath.Join(t.TempDir(), "live-host.pid")
	t.Setenv(workerHostStuckPIDFileEnv, hostPIDPath)

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	defer cancel()
	runDone := make(chan pi.WorkerResult, 1)
	go func() {
		runDone <- a.Run(ctx, adapterWorkerRequest(t.TempDir()))
	}()
	waitWorkerHostSpawn(t, seam)
	hostPID := int(seam.pid.Load())
	if pidFile := readPIDFile(t, hostPIDPath); pidFile != hostPID {
		t.Fatalf("recorded host pid = %d, want the adapter's own child %d", pidFile, hostPID)
	}
	if !processAlive(hostPID) {
		t.Fatalf("live host %d is not alive after closing its response writer", hostPID)
	}
	// No kill cleanup is registered here on purpose: the seam cleanup
	// already SIGKILLs this exact pid, reaps it and verifies it is gone.

	select {
	case result := <-runDone:
		if result.Status != pi.StatusError {
			t.Fatalf("result = %+v, want the receive-error status", result)
		}
		if !strings.Contains(result.Error, "read worker host response") {
			t.Fatalf("error %q does not report the response EOF that started the reap", result.Error)
		}
		requireUnkilledHostResult(t, result)
	case <-time.After(15 * time.Second):
		t.Fatal("adapter Run did not return after the bounded reap with a failed kill")
	}
}
