//go:build darwin || linux

package background

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// roleDetachSleepSeconds bounds the sleep exec'd by the role-process
// detach test child: long enough that no test can outlive it, short
// enough that a child orphaned by a hard-killed test binary still
// terminates on its own.
const roleDetachSleepSeconds = "120"

// writeRoleProcessSleepScript writes a temporary executable shell script
// that ignores its role argument (argv[1] passed by startRoleProcess)
// and execs a bounded sleep. Because the script execs, the spawned child
// is exactly the sleep process and has no surviving descendant.
func writeRoleProcessSleepScript(t *testing.T) string {
	t.Helper()
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("exec.LookPath(sleep): %v", err)
	}
	script := filepath.Join(t.TempDir(), "role-process-sleep.sh")
	body := "#!/bin/sh\n" +
		"# Ignore the role argument passed by startRoleProcess.\n" +
		"exec " + sleepPath + " " + roleDetachSleepSeconds + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write role process sleep script: %v", err)
	}
	return script
}

// startRoleSleepProcess starts the bounded-sleep child under role r and
// captures its exact PID before any Detach call can release the Go
// handle. Cleanups run last-added first: the Close cleanup registered
// last runs first, then the raw-PID fallback registered second, then
// the verification registered first. The Close cleanup calls Close
// only when closeDone is not already closed: a test body that already
// completed the shared Close/Detach lifecycle asserted the cached
// result, so Close is skipped and its expected error is not reported
// again. When that cleanup Close runs, it closes every parent pipe end
// and kills and reaps the child, so the raw-PID fallback then sees
// cmd.ProcessState set and does nothing; otherwise the fallback kills
// and reaps the still-unreaped exact child PID — a Detach never waits
// and leaves one — and the final verification checks that the exact
// PID is gone, so assertion-failure paths cannot leak the child.
func startRoleSleepProcess(t *testing.T, r role) (*roleProcess, int) {
	t.Helper()
	p, err := startRoleProcess(writeRoleProcessSleepScript(t), r)
	if err != nil {
		t.Fatalf("startRoleProcess(%q): %v", r, err)
	}
	pid := p.cmd.Process.Pid

	t.Cleanup(func() {
		if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
			t.Errorf("role process %d still alive after cleanup", pid)
		}
	})
	t.Cleanup(func() {
		if p.cmd.ProcessState != nil {
			return // cmd.Wait may have reaped it in the test body or cleanup Close
		}
		if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			t.Errorf("cleanup kill role process %d: %v", pid, err)
		}
		if _, err := unix.Wait4(pid, nil, 0, nil); err != nil && !errors.Is(err, unix.ECHILD) {
			t.Errorf("cleanup reap role process %d: %v", pid, err)
		}
	})
	t.Cleanup(func() {
		// Registered last so it runs before the raw exact-PID fallback
		// above. When the test body already completed the shared
		// Close/Detach lifecycle, closeDone is closed, so this cleanup
		// skips Close and therefore does not report the cached result
		// again; a Detach never kills the child, so a detached child
		// remains alive for the exact-PID fallback to kill and reap.
		select {
		case <-p.closeDone:
		default:
			if err := p.Close(); err != nil {
				t.Errorf("cleanup close role process: %v", err)
			}
		}
	})
	return p, pid
}

// assertRoleChildAlive requires that the exact child PID still exists.
func assertRoleChildAlive(t *testing.T, pid int) {
	t.Helper()
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatalf("role process %d is not alive: %v", pid, err)
	}
}

// assertRoleChildGone requires that the exact child PID no longer
// exists.
func assertRoleChildGone(t *testing.T, pid int) {
	t.Helper()
	if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("role process %d still exists: %v", pid, err)
	}
}

// assertRoleProcessReleased verifies that the os.Process handle was
// released: on Unix Release marks the Process released and resets its
// Pid to -1.
func assertRoleProcessReleased(t *testing.T, p *roleProcess) {
	t.Helper()
	if p.cmd.Process.Pid != -1 {
		t.Fatalf("Process.Pid = %d, want -1 after Release", p.cmd.Process.Pid)
	}
}

// assertRoleFileClosed requires that the parent-side pipe end is closed.
func assertRoleFileClosed(t *testing.T, f *os.File, what string) {
	t.Helper()
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("%s: got %v, want os.ErrClosed", what, err)
	}
}

// assertRoleFileOpen requires that the parent-side pipe end is still
// open.
func assertRoleFileOpen(t *testing.T, f *os.File, what string) {
	t.Helper()
	if _, err := f.Stat(); err != nil {
		t.Fatalf("%s: got %v, want open file", what, err)
	}
}

// TestRoleProcessDetachStoredStartRole verifies that the role stored on
// the roleProcess is exactly the validated role passed to
// startRoleProcess, for both roles.
func TestRoleProcessDetachStoredStartRole(t *testing.T) {
	for _, r := range []role{roleSupervisor, roleWorkerHost} {
		t.Run(string(r), func(t *testing.T) {
			p, _ := startRoleSleepProcess(t, r)
			if p.role != r {
				t.Fatalf("stored role = %q, want %q", p.role, r)
			}
			if !validRole(p.role) {
				t.Fatalf("stored role %q is not a validated role", p.role)
			}
		})
	}
}

// TestRoleProcessSupervisorDetach verifies a successful supervisor
// Detach: it closes the parent request writer and response reader,
// releases the os.Process handle, returns while the exact child PID is
// still alive, and never calls Wait.
func TestRoleProcessSupervisorDetach(t *testing.T) {
	p, pid := startRoleSleepProcess(t, roleSupervisor)

	assertRoleChildAlive(t, pid)

	if err := p.Detach(); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	// Detach returned while the exact child is still running.
	assertRoleChildAlive(t, pid)

	// Both parent pipe ends are closed...
	assertRoleFileClosed(t, p.requestWriter, "request writer")
	assertRoleFileClosed(t, p.responseReader, "response reader")
	if !p.requestClosed || !p.respClosed {
		t.Fatal("Detach did not mark the request writer and response reader closed")
	}
	if p.requestCloseErr != nil || p.respCloseErr != nil {
		t.Fatalf("Detach close errors: request %v, response %v", p.requestCloseErr, p.respCloseErr)
	}
	if err := p.Send([]byte("post-detach")); !errors.Is(err, errRoleRequestClosed) {
		t.Fatalf("Send after Detach: got %v, want errRoleRequestClosed", err)
	}

	// ...and the os.Process handle was released.
	assertRoleProcessReleased(t, p)

	// Detach never waits: no ProcessState, no completed wait, no error.
	if p.cmd.ProcessState != nil {
		t.Fatalf("cmd.ProcessState = %v, want nil: Detach must not call Wait", p.cmd.ProcessState)
	}
	select {
	case <-p.done:
		t.Fatal("wait done channel closed: Detach must not call Wait")
	default:
	}
	if p.waitErr != nil {
		t.Fatalf("waitErr = %v, want nil", p.waitErr)
	}
}

// TestRoleProcessDetachRepeatedReturnsCachedResult verifies Detach
// idempotence: every repeated call returns the cached nil result and
// leaves the still-running child untouched.
func TestRoleProcessDetachRepeatedReturnsCachedResult(t *testing.T) {
	p, pid := startRoleSleepProcess(t, roleSupervisor)

	for i := 0; i < 3; i++ {
		if err := p.Detach(); err != nil {
			t.Fatalf("Detach call %d: %v", i+1, err)
		}
		assertRoleChildAlive(t, pid)
	}
	if p.cmd.ProcessState != nil {
		t.Fatal("cmd.ProcessState set: Detach must not call Wait")
	}
}

// TestRoleProcessCloseAfterDetachReturnsCachedResult verifies that a
// deferred Close after a successful Detach observes the completed detach
// lifecycle, returns the cached nil result, and never kills the
// still-live child.
func TestRoleProcessCloseAfterDetachReturnsCachedResult(t *testing.T) {
	p, pid := startRoleSleepProcess(t, roleSupervisor)

	if err := p.Detach(); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	assertRoleChildAlive(t, pid)

	if err := p.Close(); err != nil {
		t.Fatalf("Close after Detach: %v", err)
	}
	assertRoleChildAlive(t, pid)
	if p.cmd.ProcessState != nil {
		t.Fatal("Close after Detach killed and waited the child")
	}
	assertRoleProcessReleased(t, p)

	// A later Close keeps returning the cached detach result.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close after Detach: %v", err)
	}
	assertRoleChildAlive(t, pid)
}

// TestRoleProcessDetachPreclosedParentHandle verifies that a preclosed
// parent handle makes Detach return a close diagnostic while it still
// attempts the other close and Process.Release, and the accepted child
// remains alive. Repeated Detach and a later Close both return the
// cached diagnostic and never kill the child.
func TestRoleProcessDetachPreclosedParentHandle(t *testing.T) {
	p, pid := startRoleSleepProcess(t, roleSupervisor)

	// Preclose the parent request writer behind Detach's back: the
	// requestClosed flag stays false, so Detach attempts the close, gets
	// os.ErrClosed, and must record the diagnostic.
	if err := p.requestWriter.Close(); err != nil {
		t.Fatalf("preclose request writer: %v", err)
	}

	detachErr := p.Detach()
	if detachErr == nil {
		t.Fatal("Detach: got nil, want close diagnostic")
	}
	if !strings.Contains(detachErr.Error(), "role process detach") ||
		!strings.Contains(detachErr.Error(), "close role process request writer") {
		t.Fatalf("Detach error %q lacks the request-writer close diagnostic", detachErr)
	}
	if !p.requestClosed {
		t.Fatal("requestClosed not set after the attempted close")
	}
	if p.requestCloseErr == nil {
		t.Fatal("requestCloseErr is nil, want cached close error")
	}

	// The other close was still attempted and succeeded...
	assertRoleFileClosed(t, p.responseReader, "response reader")
	if p.respCloseErr != nil {
		t.Fatalf("response close error: %v", p.respCloseErr)
	}

	// ...Process.Release still ran, and the accepted child is alive.
	assertRoleProcessReleased(t, p)
	assertRoleChildAlive(t, pid)

	// Repeated Detach and Close both return the cached diagnostic.
	for i, again := range []func() error{p.Detach, p.Close} {
		err := again()
		if err == nil {
			t.Fatalf("call %d: got nil, want cached detach diagnostic", i)
		}
		if err.Error() != detachErr.Error() {
			t.Fatalf("call %d: got %q, want cached %q", i, err, detachErr)
		}
		assertRoleChildAlive(t, pid)
	}
}

// TestRoleProcessWorkerHostDetachRejected verifies that Detach on a
// worker-host is rejected before any lifecycle mutation, and that the
// process's normal Close afterwards still kills and reaps cleanly.
func TestRoleProcessWorkerHostDetachRejected(t *testing.T) {
	p, pid := startRoleSleepProcess(t, roleWorkerHost)

	detachErr := p.Detach()
	if detachErr == nil {
		t.Fatal("Detach: got nil, want role rejection")
	}
	want := fmt.Sprintf("only %q may detach, process role is %q", roleSupervisor, roleWorkerHost)
	if !strings.Contains(detachErr.Error(), want) {
		t.Fatalf("Detach error %q does not reject with %q", detachErr, want)
	}

	// The rejection happens before any lifecycle mutation: no completed
	// close lifecycle, no closed pipe ends, no released process handle,
	// and no Wait.
	select {
	case <-p.closeDone:
		t.Fatal("close lifecycle completed: rejection must not mutate lifecycle state")
	default:
	}
	if p.closeErr != nil {
		t.Fatalf("closeErr = %v, want nil", p.closeErr)
	}
	if p.requestClosed || p.respClosed {
		t.Fatal("pipe ends closed by rejected Detach")
	}
	assertRoleFileOpen(t, p.requestWriter, "request writer")
	assertRoleFileOpen(t, p.responseReader, "response reader")
	if p.cmd.Process.Pid != pid || p.cmd.ProcessState != nil {
		t.Fatalf("rejected Detach mutated the process handle: pid %d, state %v", p.cmd.Process.Pid, p.cmd.ProcessState)
	}

	// The normal Close still kills and reaps the worker-host cleanly.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertRoleProcessReaped(t, p)
	assertRoleChildGone(t, pid)
}

// TestRoleProcessDetachNilSafe verifies that Detach on a nil receiver is
// safe and returns nil.
func TestRoleProcessDetachNilSafe(t *testing.T) {
	var p *roleProcess
	if err := p.Detach(); err != nil {
		t.Fatalf("nil Detach: %v", err)
	}
}
