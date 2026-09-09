//go:build darwin || linux

package background

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

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

	// Let both goroutines settle inside their blocking calls: Send is
	// stuck writing to the full request pipe holding sendMu, Receive is
	// stuck reading the empty response pipe holding receiveMu.
	time.Sleep(200 * time.Millisecond)

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

	// Close returned, so the child is gone and the blocked I/O must have
	// unblocked with errors; collect both through bounded waits.
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
