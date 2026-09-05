//go:build darwin || linux

package admission

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
)

// TestHelperSubprocessReconcile is an env-gated helper that acquires a
// lease, enqueues one queued ticket, prints a single "ready" line only
// after both tickets are durable, and then blocks until killed. It is
// never called directly by a test.
//
// Required env vars:
//
//	PI_WORKER_ADMISSION_TEST_HELPER=subprocess-reconcile
//	PI_WORKER_ADMISSION_TEST_ROOT               – shared admission root directory
//	PI_WORKER_ADMISSION_TEST_MAX_LIVE           – maxLive for Gate.Open
//	PI_WORKER_ADMISSION_TEST_RUN_ID_ACQUIRED    – RunID for the leased ticket
//	PI_WORKER_ADMISSION_TEST_RUN_ID_QUEUED      – RunID for the queued ticket
//	PI_WORKER_ADMISSION_TEST_WORKER_ID_ACQUIRED – WorkerID for the leased ticket
//	PI_WORKER_ADMISSION_TEST_WORKER_ID_QUEUED   – WorkerID for the queued ticket
//
// Protocol: acquire one lease, enqueue one queued ticket, print "ready",
// then block reading stdin until killed. The parent hard-kills the child,
// reaps the PID, and calls Reconcile.
func TestHelperSubprocessReconcile(t *testing.T) {
	if os.Getenv("PI_WORKER_ADMISSION_TEST_HELPER") != "subprocess-reconcile" {
		return
	}

	root := os.Getenv("PI_WORKER_ADMISSION_TEST_ROOT")
	maxLive := envInt("PI_WORKER_ADMISSION_TEST_MAX_LIVE", 1)
	runIDAcq := os.Getenv("PI_WORKER_ADMISSION_TEST_RUN_ID_ACQUIRED")
	runIDQ := os.Getenv("PI_WORKER_ADMISSION_TEST_RUN_ID_QUEUED")
	widAcq := envInt("PI_WORKER_ADMISSION_TEST_WORKER_ID_ACQUIRED", 1)
	widQ := envInt("PI_WORKER_ADMISSION_TEST_WORKER_ID_QUEUED", 2)

	g, err := Open(root, maxLive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Acquire a lease: Enqueue persists the queued ticket, Wait persists the
	// granted lease, so both states are durable before "ready" is printed.
	leaseTicket, err := g.Enqueue(Request{RunID: runIDAcq, WorkerID: widAcq})
	if err != nil {
		fmt.Fprintf(os.Stderr, "enqueue1: %v\n", err)
		os.Exit(1)
	}
	lease, err := leaseTicket.Wait(ctx)
	if err != nil {
		_ = leaseTicket.Cancel()
		fmt.Fprintf(os.Stderr, "wait1: %v\n", err)
		os.Exit(1)
	}
	_ = lease // held but never released until killed

	// Enqueue a second ticket that stays queued (lease held above).
	queuedTicket, err := g.Enqueue(Request{RunID: runIDQ, WorkerID: widQ})
	if err != nil {
		fmt.Fprintf(os.Stderr, "enqueue2: %v\n", err)
		os.Exit(1)
	}
	_ = queuedTicket

	// Both tickets are durable. Signal the parent and block until killed.
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		fmt.Fprintf(os.Stderr, "write ready: %v\n", err)
		os.Exit(1)
	}
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	os.Exit(0)
}

// TestSubprocessReconcileRemovesStaleQueuedAndLeased verifies that when a
// child process owns both a leased and a queued ticket and is killed and
// reaped, the parent's Reconcile removes both stale tickets without
// enqueuing any probe (nextSequence unchanged).
func TestSubprocessReconcileRemovesStaleQueuedAndLeased(t *testing.T) {
	root := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSubprocessReconcile", "-test.timeout=30s")
	cmd.Env = envWith(os.Environ(),
		"PI_WORKER_ADMISSION_TEST_HELPER=subprocess-reconcile",
		"PI_WORKER_ADMISSION_TEST_ROOT="+root,
		"PI_WORKER_ADMISSION_TEST_MAX_LIVE=1",
		"PI_WORKER_ADMISSION_TEST_RUN_ID_ACQUIRED=run-acq",
		"PI_WORKER_ADMISSION_TEST_RUN_ID_QUEUED=run-q",
		"PI_WORKER_ADMISSION_TEST_WORKER_ID_ACQUIRED=1",
		"PI_WORKER_ADMISSION_TEST_WORKER_ID_QUEUED=2",
	)

	// The parent keeps the stdin pipe open so the child blocks after
	// "ready" instead of seeing EOF and exiting on its own.
	stdinW, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderrBuf lockedBuffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	// The sole cmd.Wait caller; the closed channel broadcasts completion so
	// both the test body and the cleanup can observe it without consuming.
	waitDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(waitDone)
	}()

	// Registered immediately after Start: if the test fails before the
	// intentional hard kill, kill and reap the child so neither the process
	// nor the wait goroutine survives the assertion.
	t.Cleanup(func() {
		stdinW.Close()
		select {
		case <-waitDone:
			return
		case <-time.After(3 * time.Second):
		}
		cmd.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(3 * time.Second):
			t.Error("helper did not exit after kill in cleanup")
		}
	})

	// Read the single "ready" line with a bounded wait.
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			lines <- strings.TrimSpace(scanner.Text())
		}
		close(lines)
	}()

	select {
	case line, ok := <-lines:
		if !ok {
			<-waitDone
			t.Fatalf("helper stdout closed before ready; wait err=%v (stderr: %s)", waitErr, stderrBuf.String())
		}
		if line != "ready" {
			t.Fatalf("unexpected helper output: %q", line)
		}
	case <-waitDone:
		t.Fatalf("helper exited before ready; wait err=%v (stderr: %s)", waitErr, stderrBuf.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for ready; stderr: %s", stderrBuf.String())
	}

	// Both tickets are durable: one leased (seq 1) and one queued (seq 2).
	st := readStateForTest(t, root)
	assertTicketCount(t, st, 2)
	assertNextSequence(t, st, 3)
	if st.Tickets[0].RunID != "run-acq" || st.Tickets[0].State != ticketLeased {
		t.Errorf("ticket[0] = {RunID=%q State=%q}, want run-acq/%s",
			st.Tickets[0].RunID, st.Tickets[0].State, ticketLeased)
	}
	if st.Tickets[1].RunID != "run-q" || st.Tickets[1].State != ticketQueued {
		t.Errorf("ticket[1] = {RunID=%q State=%q}, want run-q/%s",
			st.Tickets[1].RunID, st.Tickets[1].State, ticketQueued)
	}

	// Intentional hard kill: Kill must succeed and Wait must report the
	// expected signalled (non-zero) termination.
	childPID := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("helper did not exit after kill")
	}
	if waitErr == nil {
		t.Fatal("helper Wait = nil after kill, want signalled termination")
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("helper Wait error = %v, want *exec.ExitError", waitErr)
	}
	ws, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("helper termination = %v, want signalled by SIGKILL", exitErr.ProcessState)
	}
	// Prove the PID is gone before Reconcile must reap its tickets.
	waitForPIDGone(t, childPID, 3*time.Second)

	// Parent Reconcile removes both stale tickets without enqueuing any
	// probe: zero tickets left and nextSequence unchanged.
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open for Reconcile: %v", err)
	}
	if err := g.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
	assertNextSequence(t, st, 3)
}
