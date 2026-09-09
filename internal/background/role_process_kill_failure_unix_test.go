//go:build darwin || linux

package background

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// waitMutexHeld blocks until mu is held by another goroutine or the bounded
// deadline expires. sync.Mutex.TryLock succeeds only while the mutex is
// free, so a failing TryLock is positive evidence that some other goroutine
// holds it — here, that the blocked Send or Receive is genuinely inside its
// I/O call holding the mutex Close must not wait for.
func waitMutexHeld(t *testing.T, mu *sync.Mutex, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if mu.TryLock() {
			mu.Unlock()
			time.Sleep(time.Millisecond)
			continue
		}
		return
	}
	t.Fatalf("%s was never held: the blocking call was not entered within the deadline", what)
}

// TestRoleProcessCloseKillFailureUnblocksIO proves that Close survives a
// Kill that fails. Close kills the child first so a Send blocked writing
// to a non-reading child releases sendMu before CloseRequest needs it,
// and a Receive blocked reading releases receiveMu before closeResponse
// needs it. That ordering depends on the kill succeeding: if the kill
// fails, the child stays alive, both I/O goroutines stay blocked holding
// their mutexes, and the current Close walks straight into CloseRequest
// and deadlocks forever on sendMu. The stuck child fixture (which reads
// nothing and lives forever) plus a per-instance failing kill seam make
// that failure deterministic; this test fails while the deadlock exists
// and must pass once Close tolerates a failed kill.
func TestRoleProcessCloseKillFailureUnblocksIO(t *testing.T) {
	// The stuck child mode ignores its request pipe, its response pipe,
	// and its ownership pipe, and lives forever, so closing the ownership
	// writer cannot unblock anything. Every other child mode exits on
	// ownership EOF, which would let the blocked I/O return for the
	// wrong reason.
	t.Setenv(workerHostStuckEnv, "1")
	t.Setenv(workerHostStuckPIDFileEnv, filepath.Join(t.TempDir(), "stuck.pid"))

	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	pid := p.cmd.Process.Pid

	// Install the failing kill seam before any goroutine starts and
	// never write to the field again: Close reads it from another
	// goroutine, so a later write would be a data race under -race.
	killErr := errors.New("forced kill failure")
	p.killProcess = func() error { return killErr }

	// Close will not terminate the child on the failing-kill path, so
	// cleanup does it for real: SIGKILL the exact child, then reap it.
	// Wait calls cmd.Wait directly and never touches the seam.
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = p.Wait()
	})

	payload := make([]byte, 1<<20) // 1 MiB

	sendDone := make(chan error, 1)
	receiveDone := make(chan error, 1)
	closeDone := make(chan error, 1)

	go func() { sendDone <- p.Send(payload) }()
	go func() {
		_, err := p.Receive()
		receiveDone <- err
	}()

	// Both goroutines must be genuinely inside their blocking calls before
	// Close runs: Send stuck writing to the full request pipe while holding
	// sendMu, Receive stuck reading the empty response pipe while holding
	// receiveMu. Waiting for the mutexes to be held is the observable
	// condition; a fixed sleep only assumes it, and a Close that starts too
	// early would pass even against the deadlocking implementation.
	waitMutexHeld(t, &p.sendMu, "sendMu")
	waitMutexHeld(t, &p.receiveMu, "receiveMu")

	go func() { closeDone <- p.Close() }()

	select {
	case closeErr := <-closeDone:
		if closeErr == nil {
			t.Fatal("Close: expected non-nil error after a failed Kill, got nil")
		}
		if !errors.Is(closeErr, killErr) {
			t.Fatalf("Close: got %v, want an error wrapping the forced kill failure %v", closeErr, killErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return: deadlocked on sendMu/receiveMu after a failed Kill")
	}

	// Close returned and closed the parent-side pipe ends, so the blocked
	// I/O calls must have returned with errors; collect both through
	// bounded waits.
	var sendErr, receiveErr error
	for i := 0; i < 2; i++ {
		select {
		case e := <-sendDone:
			sendErr = e
		case e := <-receiveDone:
			receiveErr = e
		case <-time.After(10 * time.Second):
			t.Fatalf("blocked I/O goroutine %d did not return after Close", i+1)
		}
	}

	if sendErr == nil {
		t.Fatal("Send: expected non-nil error after Close, got nil")
	}
	if receiveErr == nil {
		t.Fatal("Receive: expected non-nil error after Close, got nil")
	}

	// The request writer must have been marked closed, not merely closed
	// behind the closed flag's back.
	if err := p.Send([]byte("x")); !errors.Is(err, errRoleRequestClosed) {
		t.Fatalf("follow-up Send: got %v, want errRoleRequestClosed", err)
	}

	// The child is alive on purpose on the failing-kill path; asserting
	// reaping here would be wrong. Cleanup SIGKILLs and reaps it.
}

// TestRoleProcessKillAfterDetachReturnsReleasedError proves that Kill on
// a handle released by Detach is not mistaken for the already-done case:
// Process.Kill on a released handle reports a released process, which is
// neither nil nor os.ErrProcessDone, so Kill must return that error
// wrapped and must not call Wait — the wait lifecycle is still open and
// the released child cannot be reaped through this handle. The child here
// is the supervisor echo child, which exits on request-pipe EOF, so the
// short-lived child itself is irrelevant: every assertion is about the
// parent's handle state after Release. Cleanup kills and reaps the exact
// recorded PID with raw syscalls, because the released Go handle makes
// p.Wait() both forbidden and useless.
func TestRoleProcessKillAfterDetachReturnsReleasedError(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleSupervisor)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	pid := p.cmd.Process.Pid

	if err := p.Detach(); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	// The released handle reports the child as gone from Go's point of
	// view; the OS process may still be winding down after the request
	// EOF, so cleanup works on the raw PID. Reap with Wait4 — never with
	// p.Wait(), whose handle was released.
	t.Cleanup(func() {
		if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
			t.Errorf("cleanup kill %d: %v", pid, kerr)
		}
		if _, werr := syscall.Wait4(pid, nil, 0, nil); werr != nil && !errors.Is(werr, syscall.ECHILD) {
			t.Errorf("cleanup reap %d: %v", pid, werr)
		}
	})

	killErr := p.Kill()
	if killErr == nil {
		t.Fatal("Kill on a released handle: got nil, want the released-process error")
	}
	if errors.Is(killErr, os.ErrProcessDone) {
		t.Fatalf("Kill on a released handle: got %v, which must not be os.ErrProcessDone", killErr)
	}

	// Kill must have returned before ever entering Wait: the wait
	// lifecycle is still open, so done is unclosed.
	select {
	case <-p.done:
		t.Fatal("Kill on a released handle entered Wait: done channel is closed")
	default:
	}
}

// TestRoleProcessCloseFailedKillIsCachedAndNotRetried proves that Close
// never retries a failed kill: the first Close performs exactly one kill
// through the seam, caches the failure, and every later Close — serial or
// concurrent — returns that same cached error value without touching the
// seam again. The seam counts every invocation, so a retry would show up
// as a second count.
func TestRoleProcessCloseFailedKillIsCachedAndNotRetried(t *testing.T) {
	// The stuck child ignores every pipe and lives forever, so the kill
	// failure is the only event in this lifecycle.
	t.Setenv(workerHostStuckEnv, "1")
	t.Setenv(workerHostStuckPIDFileEnv, filepath.Join(t.TempDir(), "stuck.pid"))

	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	pid := p.cmd.Process.Pid

	// A counting failing seam, installed before any goroutine runs and
	// never written again; the atomic keeps the post-hoc read honest
	// under -race. Cleanup SIGKILLs the exact child and reaps it.
	var kills atomic.Int64
	killErr := errors.New("forced kill failure")
	p.killProcess = func() error { kills.Add(1); return killErr }
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = p.Wait()
	})

	firstDone := make(chan error, 1)
	go func() { firstDone <- p.Close() }()
	var first error
	select {
	case first = <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("first Close did not return")
	}
	if first == nil {
		t.Fatal("Close: expected non-nil error after a failed Kill, got nil")
	}
	if !errors.Is(first, killErr) {
		t.Fatalf("Close: got %v, want an error wrapping the forced kill failure %v", first, killErr)
	}

	// Two serial and three concurrent repeat Closes must all observe the
	// cached failure without re-killing.
	const extraSerial = 2
	const extraConcurrent = 3
	results := make(chan error, extraSerial+extraConcurrent)
	for i := 0; i < extraSerial; i++ {
		go func() { results <- p.Close() }()
	}
	for i := 0; i < extraConcurrent; i++ {
		go func() { results <- p.Close() }()
	}
	for i := 0; i < extraSerial+extraConcurrent; i++ {
		select {
		case closeErr := <-results:
			if closeErr == nil {
				t.Fatal("repeated Close: got nil, want the cached failure")
			}
			if !errors.Is(closeErr, killErr) {
				t.Fatalf("repeated Close: got %v, want an error wrapping %v", closeErr, killErr)
			}
			if closeErr != first {
				t.Fatalf("repeated Close: got a different error value %p, want the cached first error %p", closeErr, first)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("repeated Close %d did not return", i+1)
		}
	}

	if got := kills.Load(); got != 1 {
		t.Fatalf("kill seam invoked %d times, want exactly 1", got)
	}
}

// TestRoleProcessExplicitKillRetryAfterFailedClose proves the recovery
// path after a failed-killing Close: Close fails and caches its error,
// and a later explicit Kill with the obstacle removed can really kill and
// reap the child — while the cached Close error stays exactly what it
// was, a historical record that a successful retry does not rewrite.
func TestRoleProcessExplicitKillRetryAfterFailedClose(t *testing.T) {
	t.Setenv(workerHostStuckEnv, "1")
	t.Setenv(workerHostStuckPIDFileEnv, filepath.Join(t.TempDir(), "stuck.pid"))

	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	pid := p.cmd.Process.Pid

	// The seam fails only while the flag is set: the Close kill fails,
	// then the flag flips through the atomic inside the closure, so the
	// seam itself is the only writer from then on and no field is ever
	// rewritten after goroutines start.
	var fail atomic.Bool
	fail.Store(true)
	var kills atomic.Int64
	killErr := errors.New("forced kill failure")
	p.killProcess = func() error {
		kills.Add(1)
		if fail.Load() {
			return killErr
		}
		return p.cmd.Process.Kill()
	}
	// Cleanup only reaps if the explicit retry below did not already do
	// it: a reaped child makes Wait a no-op returning the cached error.
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = p.Wait()
	})

	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	var cachedErr error
	select {
	case cachedErr = <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	if cachedErr == nil {
		t.Fatal("Close: expected non-nil error after a failed Kill, got nil")
	}

	// The obstacle is gone: the next Kill really kills and reaps.
	fail.Store(false)
	killDone := make(chan error, 1)
	go func() { killDone <- p.Kill() }()
	select {
	case killErr2 := <-killDone:
		if killErr2 != nil {
			t.Fatalf("explicit Kill retry: got %v, want nil", killErr2)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("explicit Kill retry did not return")
	}

	assertRoleProcessReaped(t, p)

	// A successful retry does not rewrite the cached Close failure.
	retryCloseDone := make(chan error, 1)
	go func() { retryCloseDone <- p.Close() }()
	select {
	case closeErr := <-retryCloseDone:
		if closeErr == nil {
			t.Fatal("Close after successful Kill: got nil, want the cached failure")
		}
		if closeErr != cachedErr {
			t.Fatalf("Close after successful Kill: got a different error value %p, want the cached %p", closeErr, cachedErr)
		}
		if !errors.Is(closeErr, killErr) {
			t.Fatalf("Close after successful Kill: got %v, want an error wrapping %v", closeErr, killErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close after successful Kill did not return")
	}

	// Exactly two kills ran: the failed Close kill and the retry.
	if got := kills.Load(); got != 2 {
		t.Fatalf("kill seam invoked %d times, want exactly 2", got)
	}
}
