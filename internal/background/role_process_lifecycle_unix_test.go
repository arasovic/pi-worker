//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// countFDs snapshots the number of entries in /dev/fd; on systems where
// that directory is unreadable it returns 0 as a sentinel.
func countFDs(t *testing.T) int {
	t.Helper()
	infos, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0
	}
	return len(infos)
}

// assertRoleProcessReaped requires p to be non-nil and verifies that its
// cmd, Process, and ProcessState fields are all non-nil before confirming
// reaping via a zero-signal check.  SIGKILL causes ProcessState.Exited() to
// report false, so we reject by signalling pid 0 and checking for
// os.ErrProcessDone rather than examining Exited().
func assertRoleProcessReaped(t *testing.T, p *roleProcess) {
	t.Helper()
	if p == nil {
		t.Fatal("role process is nil")
	}
	if p.cmd == nil {
		t.Fatal("cmd is nil")
	}
	if p.cmd.Process == nil {
		t.Fatal("Process is nil")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil")
	}
	if err := p.cmd.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("process %d is not reaped: %v", p.cmd.Process.Pid, err)
	}
}

// TestRoleProcessMissingExecutablePath validates that calling
// startRoleProcess with a nonexistent executable thirty-two times
// always yields a nil process and a non-nil error, without leaking
// file descriptors.  When /dev/fd is readable the entry count is
// checked before and after every invocation for both roles:
// roleSupervisor uses two pipes (four ends) and roleWorkerHost adds
// one more (six ends); both paths close everything on Start failure.
func TestRoleProcessMissingExecutablePath(t *testing.T) {
	const rounds = 32

	fdCountBefore := countFDs(t)

	tmpDir := t.TempDir()
	nonexistent := filepath.Join(tmpDir, "nonexistent-executable")

	t.Run("supervisor", func(t *testing.T) {
		for i := 0; i < rounds/2; i++ {
			p, err := startRoleProcess(nonexistent, roleSupervisor)
			if p != nil {
				t.Fatalf("round %d: expected nil process, got %v", i+1, p)
			}
			if err == nil {
				t.Fatalf("round %d: expected non-nil error, got nil", i+1)
			}
			if fdCountBefore > 0 && countFDs(t) != fdCountBefore {
				t.Errorf("round %d: fd leak detected", i+1)
			}
		}
	})

	t.Run("workerHost", func(t *testing.T) {
		for i := 0; i < rounds/2; i++ {
			p, err := startRoleProcess(nonexistent, roleWorkerHost)
			if p != nil {
				t.Fatalf("round %d: expected nil process, got %v", i+1, p)
			}
			if err == nil {
				t.Fatalf("round %d: expected non-nil error, got nil", i+1)
			}
			if fdCountBefore > 0 && countFDs(t) != fdCountBefore {
				t.Errorf("round %d: fd leak detected", i+1)
			}
		}
	})
}

// TestRoleProcessOversized verifies that a payload exceeding the frame
// limit is rejected without any data leaving the parent.  A worker-host
// child is started, its frameLimit is lowered to 8, a 9-byte Send is
// attempted (which must fail with errPrivateFrameTooLarge), and the
// process is cleanly torn down.  Wait confirms ProcessState exited.
func TestRoleProcessOversized(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	p.frameLimit = 8

	if err := p.Send(make([]byte, 9)); err != errPrivateFrameTooLarge {
		t.Fatalf("Send(9): got %v, want errPrivateFrameTooLarge", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resultsCh := make(chan error, 2)
	go func() { resultsCh <- p.Close() }()
	go func() { resultsCh <- p.Wait() }()

	for i := 0; i < 2; i++ {
		select {
		case <-resultsCh:
		case <-ctx.Done():
			t.Fatalf("operation timeout at iteration %d", i+1)
		}
	}

	assertRoleProcessReaped(t, p)
}

// TestRoleProcessEarlyExit exercises the supervisor's ability to shut down
// via the roleProcessExitCanary sentinel frame.  After sending the canary,
// Receive returns io.EOF (the child exits without responding), Wait yields
// an *exec.ExitError with exit code 95, repeated Wait returns the same
// error, and Close remains safe.
func TestRoleProcessEarlyExit(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleSupervisor)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if err := p.Send(roleProcessExitCanary); err != nil {
		t.Fatalf("Send canary: %v", err)
	}

	_, err = p.Receive()
	if err != io.EOF {
		t.Fatalf("Receive: got %v, want io.EOF", err)
	}

	waitErr := p.Wait()
	ee, ok := waitErr.(*exec.ExitError)
	if !ok || ee.ExitCode() != 95 {
		t.Fatalf("Wait: got %T(%v), want *exec.ExitError(95)", waitErr, waitErr)
	}

	repeatedErr := p.Wait()
	if repeatedErr == nil {
		t.Fatal("repeated Wait: got nil, want same *exec.ExitError")
	}
	if rEE, ok := repeatedErr.(*exec.ExitError); !ok || rEE.ExitCode() != 95 {
		t.Fatalf("repeated Wait: %v", repeatedErr)
	}

	assertRoleProcessReaped(t, p)
	if cerr := p.Close(); cerr != nil {
		t.Logf("Close: %v", cerr)
	}
}

// TestRoleProcessBlockedSendCleanup proves that Close can unblock a Send
// stuck writing 1 MiB to a worker-host which never reads from the pipe.
// Both operations are bounded by a 5-second context deadline.  Successful
// cleanup demonstrates that Close kills the child before waiting for
// sendMu release.
func TestRoleProcessBlockedSendCleanup(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sendStart := make(chan struct{})
	sendDone := make(chan error, 1)
	closeDone := make(chan error, 1)

	payload := make([]byte, 1<<20) // 1 MiB

	go func() {
		<-sendStart
		sendDone <- p.Send(payload)
	}()

	go func() {
		<-sendStart
		closeDone <- p.Close()
	}()

	close(sendStart)

	var sendErr, closeErr error
	for i := 0; i < 2; i++ {
		select {
		case e := <-sendDone:
			sendErr = e
		case e := <-closeDone:
			closeErr = e
		case <-ctx.Done():
			t.Fatal("timed out waiting for goroutines")
		}
	}

	if sendErr == nil {
		t.Fatal("Send: expected non-nil error, got nil")
	}
	if closeErr != nil {
		t.Fatalf("Close: unexpected error %v", closeErr)
	}

	assertRoleProcessReaped(t, p)
}

// TestRoleProcessBlockedReceiveCleanup proves that Close can unblock a Receive
// stuck waiting on a worker-host which never writes to its response pipe.
// Both operations are bounded by a 5-second context deadline.  Successful
// cleanup demonstrates that killing and reaping the exact child closes
// its fd4 and releases Receive's receiveMu before Close calls closeResponse.
func TestRoleProcessBlockedReceiveCleanup(t *testing.T) {
	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	receiveStart := make(chan struct{})
	receiveDone := make(chan error, 1)
	closeDone := make(chan error, 1)

	go func() {
		<-receiveStart
		_, err := p.Receive()
		receiveDone <- err
	}()

	go func() {
		<-receiveStart
		closeDone <- p.Close()
	}()

	close(receiveStart)

	var receiveErr, closeErr error
	for i := 0; i < 2; i++ {
		select {
		case e := <-receiveDone:
			receiveErr = e
		case e := <-closeDone:
			closeErr = e
		case <-ctx.Done():
			t.Fatal("timed out waiting for goroutines")
		}
	}

	if closeErr != nil {
		t.Fatalf("Close: unexpected error %v", closeErr)
	}
	if receiveErr == nil {
		t.Fatal("Receive: expected non-nil error, got nil")
	}

	assertRoleProcessReaped(t, p)
}
