//go:build darwin || linux

package pi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// gatedStdout wraps the child stdout reader so Client.Close provably closes
// stdout before anything else: Close first closes the underlying reader, then
// opens the gate that unblocks the fixture root. With the reader closed
// before the gate opens, the root's printf hits a real broken pipe and the
// root exits on its own, before any Process.Close lineage snapshot can run.
type gatedStdout struct {
	io.ReadCloser
	proc *Process
	gate string
}

// Close closes the child stdout reader first, then opens the gate, then waits
// boundedly for the root to be reaped, so a client-first teardown
// deterministically reaches Process.Close only after the root is already
// gone. A reader close failure other than already-closed is surfaced through
// Client.Close.
func (w *gatedStdout) Close() error {
	if err := w.ReadCloser.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	if err := os.WriteFile(w.gate, []byte("open"), 0o600); err != nil {
		return fmt.Errorf("open cleanup-order gate: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for w.proc.Running() {
		if time.Now().After(deadline) {
			return fmt.Errorf("root still running 10s after stdout close and gate open")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// cleanupOrderFixture bundles one started fixture: a real Process running the
// fixture script, a real Client over its stdin/stdout, and the exact pids of
// the root shell and its sleep descendant.
type cleanupOrderFixture struct {
	proc          *Process
	client        *Client
	rootPID       int
	descendantPID int
}

// startCleanupOrderFixture launches the shared fixture: the root shell script
// starts one bounded sleep descendant, records its pid, waits on a gate file,
// then printf-writes to stdout and blocks on stdin. A single resource cleanup
// is registered immediately after NewProcess (Close is safe before Start): it
// calls closeProcessThenClient FIRST, so the Process lineage snapshot and
// cleanup always precede the Client reader close even if setup fails, and
// then falls back to an exact-pid kill of the descendant once its pid is
// known. The root itself is owned by Process.Close.
func startCleanupOrderFixture(t *testing.T) cleanupOrderFixture {
	t.Helper()
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	pidFile := filepath.Join(dir, "descendant.pid")

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("look up sh: %v", err)
	}
	body := "#!" + shPath + "\n" +
		"sleep 120 &\n" +
		"echo $! > \"$FIXTURE_PIDFILE\"\n" +
		"while [ ! -f \"$FIXTURE_GATE\" ]; do sleep 0.05; done\n" +
		"printf 'fixture-ready\\n'\n" +
		"read fixture_line || exit 0\n"
	scriptPath := filepath.Join(dir, "fixture.sh")
	if err := os.WriteFile(scriptPath, []byte(body), 0o700); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}
	t.Setenv("FIXTURE_PIDFILE", pidFile)
	t.Setenv("FIXTURE_GATE", gate)

	proc, err := NewProcess(scriptPath, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	var client *Client
	descendantPID := 0
	// Single resource cleanup registered immediately after NewProcess: it
	// runs closeProcessThenClient FIRST — Process before Client, both
	// nil-guarded — so the root is still live for the lineage snapshot even
	// when a later setup step fails, and then falls back to an exact-pid
	// kill of the descendant once its pid is known. The root itself is
	// owned by Process.Close; the fixture never signals it directly.
	t.Cleanup(func() {
		closeProcessThenClient(proc, client)
		if descendantPID > 0 {
			if processAlive(descendantPID) {
				_ = syscall.Kill(descendantPID, syscall.SIGKILL)
			}
			waitProcessGone(t, descendantPID)
		}
	})
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	rootPID := proc.Pid()

	stdout := &gatedStdout{ReadCloser: proc.Stdout(), proc: proc, gate: gate}
	client = NewClient(proc.Stdin(), stdout, nil, nil)

	descendantPID = readPIDFile(t, pidFile)
	if !processAlive(descendantPID) {
		t.Fatalf("descendant %d is not alive after start", descendantPID)
	}
	return cleanupOrderFixture{proc: proc, client: client, rootPID: rootPID, descendantPID: descendantPID}
}

// TestWorkerCleanupOrderProcessBeforeClient is the shipped regression for the
// worker's Process+Client teardown order: closeProcessThenClient must close
// the Process BEFORE the Client. Client.Close closes the child stdout pipe;
// if that happens first, the gated root writes, exits on the broken pipe, and
// is reaped before Process.Close can snapshot its lineage, so the sleep
// descendant escapes cleanup. With the corrected order the root is still live
// when Process.Close snapshots and reaps the tree, and both the root and the
// descendant are gone once the helper returns.
func TestWorkerCleanupOrderProcessBeforeClient(t *testing.T) {
	// Shorten Process.Close's bounded kill so the teardown is quick; the
	// previous value is restored for the rest of the package.
	original := processCloseGrace
	processCloseGrace = 250 * time.Millisecond
	t.Cleanup(func() { processCloseGrace = original })

	fixture := startCleanupOrderFixture(t)

	// The exact production teardown for one Process+Client pair.
	closeProcessThenClient(fixture.proc, fixture.client)

	waitErr := fixture.proc.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("root %d wait = %v, want a reaped *exec.ExitError", fixture.rootPID, waitErr)
	}
	status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		t.Fatalf("root %d exited %v, want a signal exit from Process.Close's containment kill", fixture.rootPID, waitErr)
	}
	waitProcessGone(t, fixture.rootPID)
	waitProcessGone(t, fixture.descendantPID)
}
