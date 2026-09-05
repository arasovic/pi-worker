//go:build darwin || linux

package admission

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperSubprocess is an env-gated helper that runs Gate Acquire/Release
// as a real child process. It is never called directly by a test.
//
// Required env vars:
//
//	PI_WORKER_ADMISSION_TEST_HELPER=subprocess  – activates this code path
//	PI_WORKER_ADMISSION_TEST_ROOT               – shared admission root directory
//	PI_WORKER_ADMISSION_TEST_MAX_LIVE           – maxLive for Gate.Open
//	PI_WORKER_ADMISSION_TEST_RUN_ID             – RunID for Acquire
//	PI_WORKER_ADMISSION_TEST_WORKER_ID          – WorkerID for Acquire
//	PI_WORKER_ADMISSION_TEST_ACQUIRE_TIMEOUT    – optional, seconds (default 10)
//
// Protocol: after acquiring a lease the helper writes "acquired\n" to stdout
// and blocks reading stdin. When stdin is closed or a byte arrives, it
// releases the lease and exits 0. Any error exits nonzero without leaking
// the child.
func TestHelperSubprocess(t *testing.T) {
	if os.Getenv("PI_WORKER_ADMISSION_TEST_HELPER") != "subprocess" {
		return
	}

	root := os.Getenv("PI_WORKER_ADMISSION_TEST_ROOT")
	maxLive := envInt("PI_WORKER_ADMISSION_TEST_MAX_LIVE", 1)
	runID := os.Getenv("PI_WORKER_ADMISSION_TEST_RUN_ID")
	workerID := envInt("PI_WORKER_ADMISSION_TEST_WORKER_ID", 1)
	timeoutSec := envInt("PI_WORKER_ADMISSION_TEST_ACQUIRE_TIMEOUT", 10)

	g, err := Open(root, maxLive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	ticket, err := g.Enqueue(Request{RunID: runID, WorkerID: workerID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "enqueue: %v\n", err)
		os.Exit(1)
	}
	lease, err := ticket.Wait(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wait: %v\n", err)
		os.Exit(1)
	}

	// Signal acquisition.
	if _, err := fmt.Fprintln(os.Stdout, "acquired"); err != nil {
		fmt.Fprintf(os.Stderr, "write acquired: %v\n", err)
		_ = lease.Release()
		os.Exit(1)
	}

	// Wait for parent signal (EOF or byte).
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)

	if err := lease.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "release: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func envInt(key string, def int) int {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			return def
		}
	}
	if n == 0 {
		return def
	}
	return n
}

// lockedBuffer is a bytes.Buffer guarded by a per-method lock so that
// concurrent Write calls (from the OS pipe goroutine) and String reads
// (from the test goroutine) never race.
type lockedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (lb *lockedBuffer) Write(p []byte) (n int, err error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.data = append(lb.data, p...)
	return len(p), nil
}

func (lb *lockedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return string(lb.data)
}

// helperProc manages one helper subprocess.
type helperProc struct {
	t        *testing.T
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	lines    chan string // lines read from stdout (single reader goroutine)
	stderr   lockedBuffer
	waitErr  error
	waitDone chan struct{}
}

// startHelper launches the test binary as a helper subprocess.
func startHelper(t *testing.T, root, runID string, workerID, maxLive int) *helperProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSubprocess", "-test.timeout=30s")
	cmd.Env = envWith(os.Environ(),
		"PI_WORKER_ADMISSION_TEST_HELPER=subprocess",
		"PI_WORKER_ADMISSION_TEST_ROOT="+root,
		"PI_WORKER_ADMISSION_TEST_MAX_LIVE="+fmt.Sprint(maxLive),
		"PI_WORKER_ADMISSION_TEST_RUN_ID="+runID,
		"PI_WORKER_ADMISSION_TEST_WORKER_ID="+fmt.Sprint(workerID),
	)

	stdinW, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	h := &helperProc{
		t:        t,
		cmd:      cmd,
		stdin:    stdinW,
		lines:    make(chan string, 8),
		waitDone: make(chan struct{}),
	}

	cmd.Stderr = &h.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	// Single reader goroutine: reads lines from stdout, sends to channel.
	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			h.lines <- strings.TrimSpace(scanner.Text())
		}
		close(h.lines)
	}()

	// Collect Wait exactly once.
	go func() {
		h.waitErr = cmd.Wait()
		close(h.waitDone)
	}()

	// Cleanup: close stdin, kill only if still running, wait exactly once.
	t.Cleanup(func() {
		stdinW.Close()
		select {
		case <-h.waitDone:
		case <-time.After(3 * time.Second):
			h.cmd.Process.Kill()
			<-h.waitDone
		}
	})

	return h
}

// envWith returns a copy of env with the given overrides appended.
// Later entries win for duplicate keys.
func envWith(env []string, overrides ...string) []string {
	out := make([]string, len(env), len(env)+len(overrides))
	copy(out, env)
	out = append(out, overrides...)
	return out
}

// stderrString returns the helper's captured stderr. The lockedBuffer
// handles synchronization internally.
func (h *helperProc) stderrString() string {
	return h.stderr.String()
}

// acquireWait reads stdout lines until it sees "acquired" or the deadline.
// Fails the test on timeout or unexpected output.
func (h *helperProc) acquireWait(timeout time.Duration) string {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			h.t.Fatalf("helper did not report acquired within %v; stderr: %s", timeout, h.stderrString())
		}

		select {
		case line, ok := <-h.lines:
			if !ok {
				// stdout closed (process exited) before "acquired".
				<-h.waitDone
				h.t.Fatalf("helper stdout closed before acquired; wait err=%v (stderr: %v)", h.waitErr, h.stderrString())
			}
			if line == "acquired" {
				return line
			}
			h.t.Fatalf("unexpected helper output: %q", line)
		case <-h.waitDone:
			h.t.Fatalf("helper exited before reporting acquired; wait err=%v", h.waitErr)
		case <-time.After(200 * time.Millisecond):
			// Still waiting; loop to check deadline.
		}
	}
}

// assertNotAcquired asserts the helper does not write "acquired" (or any
// stdout line) within timeout. It selects directly on h.lines so an early
// line already buffered in the channel is still detected, and on h.waitDone
// so a helper exit also fails the assertion. Only the timeout succeeds. It
// leaves no goroutine behind.
func (h *helperProc) assertNotAcquired(timeout time.Duration) {
	h.t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-h.lines:
			if !ok {
				// stdout closed (process exited) before the timeout.
				<-h.waitDone
				h.t.Fatalf("helper stdout closed within %v without acquired; wait err=%v (stderr: %v)", timeout, h.waitErr, h.stderrString())
			}
			h.t.Fatalf("helper reported %q within %v without the lease holder releasing (stderr: %v)", line, timeout, h.stderrString())
		case <-h.waitDone:
			h.t.Fatalf("helper exited within %v without acquired; wait err=%v (stderr: %v)", timeout, h.waitErr, h.stderrString())
		case <-timer.C:
			return
		}
	}
}

// signalExit closes stdin and waits for the process to exit cleanly.
func (h *helperProc) signalExit() {
	h.t.Helper()
	h.stdin.Close()
	select {
	case <-h.waitDone:
		if h.waitErr != nil {
			h.t.Fatalf("helper exit error: %v", h.waitErr)
		}
	case <-time.After(5 * time.Second):
		h.cmd.Process.Kill()
		<-h.waitDone
		h.t.Fatalf("helper did not exit after stdin close")
	}
}

// readStatePoll polls the state file until the condition is met or deadline.
func readStatePoll(t *testing.T, root string, timeout time.Duration, cond func(state) bool) state {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := readStateForTest(t, root)
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v; last state: %+v", timeout, st)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForPIDGone polls until the given PID is confirmed absent from the
// process table, or until the deadline. This is necessary after Kill+Wait
// because some OSes (notably macOS) may not have fully reaped the PID
// from the process table by the time Wait returns.
func waitForPIDGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		exists, err := pidExists(pid)
		if err == nil && !exists {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID %d still present after %v (err=%v)", pid, timeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- Tests ---

// TestSubprocessFIFOOrdering verifies FIFO ordering with maxLive=1 using
// real subprocesses. Process A acquires; B and C queue behind it in order.
// A release grants B; B release grants C.
func TestSubprocessFIFOOrdering(t *testing.T) {
	root := t.TempDir()

	// Initialize state by opening the gate.
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = g

	// Process A acquires.
	procA := startHelper(t, root, "runA", 10, 1)
	procA.acquireWait(5 * time.Second)

	// Process B starts and should queue (A holds the lease).
	procB := startHelper(t, root, "runB", 20, 1)

	// Wait until B is queued.
	readStatePoll(t, root, 5*time.Second, func(st state) bool {
		return len(st.Tickets) == 2 && st.Tickets[1].State == ticketQueued
	})

	// Process C starts and should queue behind B.
	procC := startHelper(t, root, "runC", 30, 1)

	// Wait until C is queued.
	readStatePoll(t, root, 5*time.Second, func(st state) bool {
		return len(st.Tickets) == 3 && st.Tickets[2].State == ticketQueued
	})

	// While A holds, neither B nor C reports acquired. The assertions select
	// on the stdout lines channels directly, so an early "acquired" line
	// buffered before the assertion would still fail the test.
	procB.assertNotAcquired(1 * time.Second)
	procC.assertNotAcquired(1 * time.Second)

	// Release A → B should acquire; C remains queued.
	procA.signalExit()

	procB.acquireWait(5 * time.Second)

	// Verify C is still queued while B holds.
	readStatePoll(t, root, 3*time.Second, func(st state) bool {
		for _, tk := range st.Tickets {
			if tk.RunID == "runC" {
				return tk.State == ticketQueued
			}
		}
		return false
	})

	// Release B → C should acquire.
	procB.signalExit()

	procC.acquireWait(5 * time.Second)

	// Release C.
	procC.signalExit()

	// Final state: no tickets, nextSequence preserved.
	finalState := readStatePoll(t, root, 3*time.Second, func(st state) bool {
		return len(st.Tickets) == 0
	})
	if finalState.NextSequence != 4 { // A=1, B=2, C=3, next=4
		t.Fatalf("final NextSequence = %d, want 4", finalState.NextSequence)
	}
}

// TestSubprocessStaleLeasedOwner verifies that when a leased owner is
// hard-killed without Release, a new process on the same root reaps the
// stale lease and acquires.
func TestSubprocessStaleLeasedOwner(t *testing.T) {
	root := t.TempDir()

	// Initialize state.
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = g

	// Process A acquires.
	procA := startHelper(t, root, "runA", 10, 1)
	procA.acquireWait(5 * time.Second)

	// Hard-kill A without Release.
	aPID := procA.cmd.Process.Pid
	procA.cmd.Process.Kill()
	<-procA.waitDone
	waitForPIDGone(t, aPID, 3*time.Second)

	// Process B on the same root must reap A and acquire.
	procB := startHelper(t, root, "runB", 20, 1)
	procB.acquireWait(5 * time.Second)

	// Release B and verify clean final state.
	procB.signalExit()

	finalState := readStatePoll(t, root, 3*time.Second, func(st state) bool {
		return len(st.Tickets) == 0
	})
	if finalState.NextSequence != 3 { // A=1, B=2, next=3
		t.Fatalf("final NextSequence = %d, want 3", finalState.NextSequence)
	}
}

// TestSubprocessStaleQueuedOwner verifies that when a queued owner is
// hard-killed before acquiring, a subsequent releaser reaps the stale
// queued ticket and grants the next waiter.
func TestSubprocessStaleQueuedOwner(t *testing.T) {
	root := t.TempDir()

	// Initialize state.
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = g

	// Process A acquires.
	procA := startHelper(t, root, "runA", 10, 1)
	procA.acquireWait(5 * time.Second)

	// Process B starts and queues.
	procB := startHelper(t, root, "runB", 20, 1)
	readStatePoll(t, root, 5*time.Second, func(st state) bool {
		return len(st.Tickets) == 2 && st.Tickets[1].State == ticketQueued
	})

	// Hard-kill B before it acquires.
	bPID := procB.cmd.Process.Pid
	procB.cmd.Process.Kill()
	<-procB.waitDone
	waitForPIDGone(t, bPID, 3*time.Second)

	// Process C queues behind B's stale ticket (B should now be reaped).
	// B's ticket was already reaped, so state has A(leased) + C(queued) = 2 tickets.
	procC := startHelper(t, root, "runC", 30, 1)
	readStatePoll(t, root, 5*time.Second, func(st state) bool {
		return len(st.Tickets) == 2 && st.Tickets[1].RunID == "runC" && st.Tickets[1].State == ticketQueued
	})

	// Release A → C must reap B and acquire.
	procA.signalExit()

	procC.acquireWait(5 * time.Second)

	// Release C and verify clean final state.
	procC.signalExit()

	finalState := readStatePoll(t, root, 3*time.Second, func(st state) bool {
		return len(st.Tickets) == 0
	})
	if finalState.NextSequence != 4 { // A=1, B=2, C=3, next=4
		t.Fatalf("final NextSequence = %d, want 4", finalState.NextSequence)
	}
}
