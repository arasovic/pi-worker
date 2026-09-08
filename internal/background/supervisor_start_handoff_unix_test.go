//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// supervisorHandoffChildEnv is the environment variable that switches a
// spawned role-process test child from its echo loop into the real
// child-side supervisor start exchange (receiveSupervisorStart). The
// value is a Go duration: how long an accepted child keeps running after
// the exchange before exiting, so accepted-handoff tests can observe the
// detached supervisor as a live process. Unset or empty keeps the echo
// behavior every earlier role-process test relies on; TestMain reads this
// variable.
const supervisorHandoffChildEnv = "PI_WORKER_BACKGROUND_TEST_SUPERVISOR_HANDOFF_EXCHANGE"

// supervisorHandoffBindMismatchChildEnv is the environment variable that
// switches a spawned role-process test child out of its echo loop into
// the opt-in bind-mismatch mode: after durably accepting the one start
// request, the child answers with one complete strictly valid accepted
// Snapshot for the same request and the exact child PID in which exactly
// one Snapshot-represented field (workspace) is deliberately mismatched,
// then stays alive. The value is a Go duration bounding how long the
// child keeps running after its reply, so the starter's bind-failure
// Close kills and reaps a still-live child while the mode stays bounded
// even when the starter never closes it. Unset or empty keeps the echo
// behavior; TestMain reads this variable.
const supervisorHandoffBindMismatchChildEnv = "PI_WORKER_BACKGROUND_TEST_SUPERVISOR_HANDOFF_BIND_MISMATCH"

// supervisorHandoffPartialFrameChildEnv is the environment variable that
// switches a spawned role-process test child out of its echo loop into
// the opt-in partial-frame mode: after durably accepting the one start
// request, the child writes a valid frame length announcing the full
// accepted reply, follows it with only part of the reply payload, closes
// its response writer to produce EOF, and stays alive. The value is a Go
// duration bounding how long the child keeps running after its damaged
// frame, so the starter's read-failure Close kills and reaps a still-live
// child while the mode stays bounded even when the starter never closes
// it. Unset or empty keeps the echo behavior; TestMain reads this
// variable.
const supervisorHandoffPartialFrameChildEnv = "PI_WORKER_BACKGROUND_TEST_SUPERVISOR_HANDOFF_PARTIAL_FRAME"

// handoffOutcome carries one return of the starter-side handoff across a
// goroutine boundary.
type handoffOutcome struct {
	result supervisorStartHandoffResult
	err    error
}

// waitHandoffOutcome blocks on done no longer than timeout and fails the
// test when the handoff does not finish in time.
func waitHandoffOutcome(t *testing.T, done <-chan handoffOutcome, timeout time.Duration) handoffOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(timeout):
		t.Fatal("handoff did not finish within the bounded wait")
		return handoffOutcome{} // unreachable
	}
}

// waitHandoffSpawn blocks until the process seam reports that the child
// was spawned, failing after a bounded wait.
func waitHandoffSpawn(t *testing.T, spawned <-chan struct{}) {
	t.Helper()
	select {
	case <-spawned:
	case <-time.After(10 * time.Second):
		t.Fatal("role process was not spawned within the bounded wait")
	}
}

// captureHandoffStart wraps startRoleProcess so tests observe the exact
// spawned roleProcess and child PID the handoff drives, and receive a
// signal when the child has been spawned. The registered cleanup handles
// a child the handoff released (Detach never waits and never kills): it
// polls Wait4 for a bounded time and kills the still-running exact PID
// only when the test already failed, so assertion failures cannot leak
// the child. A child Close already killed and reaped has a non-nil
// ProcessState and is skipped.
func captureHandoffStart(t *testing.T, proc **roleProcess, pid *int) (supervisorStartProcessFunc, <-chan struct{}) {
	t.Helper()
	*proc = nil
	*pid = 0
	spawned := make(chan struct{}, 1)
	t.Cleanup(func() {
		p := *proc
		if p == nil || p.cmd.ProcessState != nil {
			return
		}
		var status unix.WaitStatus
		deadline := time.Now().Add(3 * time.Second)
		for {
			wpid, err := unix.Wait4(*pid, &status, unix.WNOHANG, nil)
			switch {
			case err == nil && wpid == *pid:
				return // reaped by the test body or on its own
			case err != nil && errors.Is(err, unix.ECHILD):
				return // already reaped by the test body
			case time.Now().After(deadline):
				if killErr := unix.Kill(*pid, unix.SIGKILL); killErr != nil && !errors.Is(killErr, unix.ESRCH) {
					t.Errorf("cleanup kill child %d: %v", *pid, killErr)
				}
				if _, waitErr := unix.Wait4(*pid, &status, 0, nil); waitErr != nil && !errors.Is(waitErr, unix.ECHILD) {
					t.Errorf("cleanup wait child %d: %v", *pid, waitErr)
				}
				return
			default:
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	return func(executable string, r role) (*roleProcess, error) {
		p, err := startRoleProcess(executable, r)
		if err != nil {
			return nil, err
		}
		*proc = p
		*pid = p.cmd.Process.Pid
		spawned <- struct{}{}
		return p, nil
	}, spawned
}

// reapHandoffChild reaps the exact child PID, waiting up to timeout for
// the child to exit on its own; a child still running at the deadline is
// killed with SIGKILL and reaped. The returned wait status describes the
// exit.
func reapHandoffChild(t *testing.T, pid int, timeout time.Duration) unix.WaitStatus {
	t.Helper()
	var status unix.WaitStatus
	deadline := time.Now().Add(timeout)
	for {
		wpid, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
		switch {
		case err == nil && wpid == pid:
			return status
		case err != nil && errors.Is(err, unix.ECHILD):
			t.Fatalf("child %d was already reaped outside this test", pid)
		case time.Now().After(deadline):
			if killErr := unix.Kill(pid, unix.SIGKILL); killErr != nil && !errors.Is(killErr, unix.ESRCH) {
				t.Fatalf("kill child %d: %v", pid, killErr)
			}
			if _, waitErr := unix.Wait4(pid, &status, 0, nil); waitErr != nil && !errors.Is(waitErr, unix.ECHILD) {
				t.Fatalf("wait child %d: %v", pid, waitErr)
			}
			return status
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// assertVoluntaryExit requires that the reaped child exited on its own
// with code 0 rather than being killed by a signal: a handoff that
// accepted must never kill its supervisor.
func assertVoluntaryExit(t *testing.T, status unix.WaitStatus) {
	t.Helper()
	if status.Signaled() {
		t.Fatalf("child was killed by signal %v: the handoff must never kill an accepted supervisor", status.Signal())
	}
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("child exited with status %v, want voluntary exit code 0", status)
	}
}

// requireNoHandoffLeak fails when goroutines or file descriptors grew
// across one handoff: no goroutine or pipe end of a finished handoff may
// survive.
func requireNoHandoffLeak(t *testing.T, fdsBefore, gosBefore int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for runtime.NumGoroutine() > gosBefore && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > gosBefore {
		t.Fatalf("goroutine growth across handoff: before %d, after %d", gosBefore, got)
	}
	if fdsBefore > 0 {
		if after := countFDs(t); after > fdsBefore {
			t.Fatalf("fd growth across handoff: before %d, after %d", fdsBefore, after)
		}
	}
}

// writeRoleProcessQuickExitScript writes a temporary executable shell
// script that ignores its role argument and exits immediately with code
// 7, so the spawned child dies before the handshake can complete.
func writeRoleProcessQuickExitScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "role-process-quick-exit.sh")
	body := "#!/bin/sh\nexit 7\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write quick-exit role process script: %v", err)
	}
	return script
}

// writeRoleProcessReadThenExitScript writes a temporary executable shell
// script that drains the request pipe on fd 3 — the one request frame —
// and exits with code 7 without ever answering. The drain ends only at
// EOF, so the child stays alive until the starter closes its request
// writer; only then does the reply read fail, deterministically after
// the Send succeeded.
func writeRoleProcessReadThenExitScript(t *testing.T) string {
	t.Helper()
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Fatalf("exec.LookPath(cat): %v", err)
	}
	script := filepath.Join(t.TempDir(), "role-process-read-then-exit.sh")
	body := "#!/bin/sh\n" +
		"# Ignore the role argument and drain the request pipe on fd 3 to EOF.\n" +
		catPath + " <&3 >/dev/null\n" +
		"exit 7\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write read-then-exit role process script: %v", err)
	}
	return script
}

// sabotageSupervisorStartCloseRequest installs the private close-request
// probe so the handoff's CloseRequest fails right after its Send
// succeeded: the probe pre-closes the parent request writer behind
// CloseRequest's back, producing the write-success/close-failure case no
// concrete production seam can create. The probe fires synchronously
// only after the child was spawned and the frame was sent, so the
// captured process is always set when it runs.
func sabotageSupervisorStartCloseRequest(t *testing.T, proc **roleProcess) {
	t.Helper()
	supervisorStartCloseRequestProbe = func() {
		p := *proc
		if p == nil {
			t.Error("close request probe fired before the process was spawned")
			return
		}
		if err := p.requestWriter.Close(); err != nil {
			t.Errorf("preclose request writer behind CloseRequest: %v", err)
		}
	}
	t.Cleanup(func() { supervisorStartCloseRequestProbe = nil })
}

// TestStartSupervisorHandoffAcceptedDetachesLiveSupervisor runs one full
// accepted handoff against a real spawned supervisor child and proves:
// the reply is bound to the request and to the exact child PID; the
// accepted Snapshot and the admission tickets were already durable when
// the handoff returned; no payload appears in argv, environment, or
// stdio; the process handle was released and both parent pipe ends are
// closed; the exact child PID remains alive after the handoff and then
// exits voluntarily with code 0 (never killed); and no goroutine or file
// descriptor is left behind.
func TestStartSupervisorHandoffAcceptedDetachesLiveSupervisor(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "1s")
	exe := testExe(t)
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, exe, req, start)
	if err != nil {
		t.Fatalf("accepted handoff: %v", err)
	}
	if !result.accepted {
		t.Fatal("accepted handoff reported a non-accepted result")
	}
	snap := result.snapshot

	// The reply snapshot is bound to the request and to the exact spawned
	// child: the handoff already enforced the full binding, and the spot
	// checks make the bound identity visible.
	if snap.RunID != req.runID || snap.Workspace != req.workspace {
		t.Fatalf("snapshot identity mismatch: runId=%q workspace=%q", snap.RunID, snap.Workspace)
	}
	if snap.Supervisor.PID != pid {
		t.Fatalf("snapshot supervisor pid %d != spawned child pid %d", snap.Supervisor.PID, pid)
	}
	if len(snap.Workers) != len(req.tasks) {
		t.Fatalf("snapshot workers = %d, want %d", len(snap.Workers), len(req.tasks))
	}
	wantTimeout := req.executionTimeout.String()
	for i, worker := range snap.Workers {
		if worker.WorkerID != i+1 || worker.ExecutionTimeout != wantTimeout {
			t.Fatalf("worker[%d] mismatch: id=%d timeout=%q", i, worker.WorkerID, worker.ExecutionTimeout)
		}
	}
	if snap.Worktree == nil || *snap.Worktree != *req.worktree {
		t.Fatalf("snapshot worktree mismatch: got %+v want %+v", snap.Worktree, req.worktree)
	}

	// The child writes the accepted reply only after its acceptance is
	// durable, so both the Snapshot and the ticket batch are observable
	// when the handoff returns.
	snapPath := filepath.Join(backgroundRoot, req.runID, "snapshot.json")
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("accepted snapshot not durable when the handoff returned: %v", err)
	}
	store, err := NewStore(backgroundRoot)
	if err != nil {
		t.Fatalf("construct store for reload: %v", err)
	}
	loaded, err := store.Load(req.runID)
	if err != nil {
		t.Fatalf("reload accepted snapshot from disk: %v", err)
	}
	if !equalSnapshots(loaded, snap) {
		t.Fatalf("persisted snapshot differs from the accepted reply snapshot:\n got: %+v\nwant: %+v", loaded, snap)
	}
	disk, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	requireNoDataBytesLeak(t, disk)
	st := readAdmissionState(t, admissionRoot)
	if len(st.Tickets) != len(req.tasks) {
		t.Fatalf("durable tickets = %d, want %d: %+v", len(st.Tickets), len(req.tasks), st.Tickets)
	}
	for i, tk := range st.Tickets {
		if tk.RunID != req.runID || tk.WorkerID != i+1 || tk.State != "queued" {
			t.Errorf("ticket[%d] mismatch: %+v", i, tk)
		}
		if tk.OwnerPID != pid {
			t.Errorf("ticket[%d] owner pid %d != spawned supervisor pid %d", i, tk.OwnerPID, pid)
		}
	}

	// No payload appears in argv, environment, or stdio: the child was
	// started with only the hidden role token in argv, inherits the
	// environment untouched, and never receives stdout or stderr.
	if got := proc.cmd.Args; len(got) != 2 || got[0] != exe || got[1] != string(roleSupervisor) {
		t.Fatalf("cmd.Args = %q, want [%q, %q]", got, exe, roleSupervisor)
	}
	if proc.cmd.Env != nil {
		t.Fatalf("cmd.Env = %v, want nil: the child inherits the parent environment untouched", proc.cmd.Env)
	}
	if len(proc.cmd.ExtraFiles) != 2 {
		t.Fatalf("ExtraFiles length = %d, want 2 (request reader, response writer)", len(proc.cmd.ExtraFiles))
	}
	if proc.cmd.Stdout != nil || proc.cmd.Stderr != nil {
		t.Fatalf("child stdout/stderr are connected: %v/%v, want the null device", proc.cmd.Stdout, proc.cmd.Stderr)
	}

	// The accepted supervisor was detached: the os.Process handle is
	// released, Detach never waited, and both parent pipe ends are
	// closed.
	assertRoleProcessReleased(t, proc)
	if proc.cmd.ProcessState != nil {
		t.Fatal("cmd.ProcessState set: Detach must not call Wait")
	}
	assertRoleFileClosed(t, proc.requestWriter, "request writer")
	assertRoleFileClosed(t, proc.responseReader, "response reader")

	// The exact child PID is still alive right after the handoff, and it
	// exits voluntarily (code 0) once its bounded hold expires — the
	// handoff never killed it.
	assertRoleChildAlive(t, pid)
	status := reapHandoffChild(t, pid, 4*time.Second)
	assertVoluntaryExit(t, status)
	assertRoleChildGone(t, pid)

	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffRejectionReturnsBoundedReason makes the
// spawned child reject the start request: the request's run directory
// already exists as a plain file, so the child's Store.Create fails after
// its tickets were prepared, and the child rolls the tickets back and
// answers with one complete rejection. The starter must report
// accepted=false with a bounded rejection error naming the cause, close
// and reap the child, and leave the child's rollback untouched on disk.
func TestStartSupervisorHandoffRejectionReturnsBoundedReason(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "1s")
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	runDir := filepath.Join(backgroundRoot, req.runID)
	marker := []byte("occupied run directory entry")
	if err := os.WriteFile(runDir, marker, 0o600); err != nil {
		t.Fatalf("write occupied run dir entry: %v", err)
	}
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if result.accepted {
		t.Fatal("rejected handoff reported acceptance")
	}
	if err == nil {
		t.Fatal("rejected handoff returned no error")
	}
	if !strings.Contains(err.Error(), "supervisor rejected the start request") {
		t.Fatalf("error %q does not report the rejection", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error %q does not carry the child's rejection cause", err)
	}
	// The rejection error is bounded even though the reason describes an
	// arbitrary prepare failure.
	if len(err.Error()) > maxSupervisorStartRejectionReason+256 {
		t.Fatalf("rejection error is not bounded: %d bytes", len(err.Error()))
	}

	// The child was closed and reaped.
	assertRoleChildGone(t, pid)

	// The child rolled back before rejecting: the occupied entry is the
	// only content under the background root, byte-identical, and no
	// durable ticket remains.
	if names := dirEntryNames(t, backgroundRoot); len(names) != 1 || names[0] != req.runID {
		t.Fatalf("background root content = %v, want only the occupied run dir entry %q", names, req.runID)
	}
	got, rerr := os.ReadFile(runDir)
	if rerr != nil {
		t.Fatalf("read occupied run dir entry: %v", rerr)
	}
	if string(got) != string(marker) {
		t.Fatalf("occupied run dir entry modified by the rejection: %q", got)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("durable tickets left after the rejection: %+v", st.Tickets)
	}

	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffMalformedReplyClosesChild runs the handoff
// against the echo-loop child, which answers the request frame by echoing
// it back: a document that is never a valid start reply. The strict
// decode must fail, the child must be closed and reaped, and neither
// state root may gain any content.
func TestStartSupervisorHandoffMalformedReplyClosesChild(t *testing.T) {
	// No exchange env: the spawned child keeps its echo-loop behavior.
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if result.accepted {
		t.Fatal("malformed reply reported acceptance")
	}
	if err == nil {
		t.Fatal("malformed reply returned no error")
	}
	if !strings.Contains(err.Error(), "decode supervisor start reply") {
		t.Fatalf("error %q does not report the strict decode failure", err)
	}
	assertRoleChildGone(t, pid)
	requireDirEmpty(t, backgroundRoot)
	requireDirEmpty(t, admissionRoot)
	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffBindMismatchClosesChild runs the handoff
// against the opt-in bind-mismatch test child: after reading the one
// request the child durably accepts it exactly as the production exchange
// would, then answers with one complete strictly valid accepted Snapshot
// for the same request and the exact child PID in which exactly one
// Snapshot-represented field — workspace — is deliberately mismatched,
// and stays alive. The strict decode must succeed and the bind must fail
// on exactly that single field: the handoff reports accepted=false with
// an error naming the bind mismatch, closes and reaps the still-live
// child, and leaves the child's genuine durable acceptance untouched on
// disk. The child mode is bounded: its hold expires on its own, and the
// capture cleanup kills and reaps an orphaned child after a bounded wait
// even when an assertion fails.
func TestStartSupervisorHandoffBindMismatchClosesChild(t *testing.T) {
	t.Setenv(supervisorHandoffBindMismatchChildEnv, "30s")
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if result.accepted {
		t.Fatal("bind-mismatch reply reported acceptance")
	}
	if err == nil {
		t.Fatal("bind-mismatch reply returned no error")
	}
	if !strings.Contains(err.Error(), "bind accepted reply") {
		t.Fatalf("error %q does not report the bind failure", err)
	}
	want := fmt.Sprintf("workspace %q does not match request workspace %q",
		supervisorHandoffTamperedWorkspace, req.workspace)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not identify the single mismatched workspace field: %q", err, want)
	}
	// Exactly one Snapshot-represented field mismatched: every other bind
	// check stays silent, so the reply was bound to the request and to
	// the exact child PID except for the one deliberate deviation.
	for _, other := range []string{
		"does not match request runId",
		"does not match request acceptedAt",
		"does not match the initial accepted time",
		"worktree does not match",
		"workers for",
		"task projection",
		"executionTimeout",
		"does not match the spawned supervisor pid",
	} {
		if strings.Contains(err.Error(), other) {
			t.Fatalf("error %q reports an additional bind mismatch (%q)", err, other)
		}
	}

	// The starter closed and reaped the still-live child: the handoff
	// must never accept a supervisor whose reply fails the bind.
	assertRoleChildGone(t, pid)

	// The child's durable acceptance is the genuine one — the reply
	// deviated only on the wire. The killed child never rolled it back
	// and the starter never touched child state.
	store, sErr := NewStore(backgroundRoot)
	if sErr != nil {
		t.Fatalf("construct store over the child's durable acceptance: %v", sErr)
	}
	loaded, lErr := store.Load(req.runID)
	if lErr != nil {
		t.Fatalf("reload the child's durable snapshot: %v", lErr)
	}
	if bindErr := bindSupervisorStartAccepted(req, loaded, pid); bindErr != nil {
		t.Fatalf("durable snapshot does not bind cleanly to the request and child: %v", bindErr)
	}
	st := readAdmissionState(t, admissionRoot)
	if len(st.Tickets) != len(req.tasks) {
		t.Fatalf("durable tickets = %d, want %d: %+v", len(st.Tickets), len(req.tasks), st.Tickets)
	}
	for i, tk := range st.Tickets {
		if tk.RunID != req.runID || tk.WorkerID != i+1 || tk.State != "queued" || tk.OwnerPID != pid {
			t.Errorf("ticket[%d] mismatch: %+v", i, tk)
		}
	}

	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffPartialFrameClosesChild runs the handoff
// against the opt-in partial-frame test child: after durably accepting
// the one request, the child writes a valid frame length announcing the
// full accepted reply, follows it with only the leading half of the reply
// payload, and closes its response writer so the starter reads EOF in the
// middle of the announced payload. The reply read must fail as a
// partial-frame read — never a decode — and the handoff reports
// accepted=false with an error naming the partial read, closes and reaps
// the still-live child, and leaves the child's durable acceptance on
// disk, proving the child had genuinely accepted before the frame was cut
// off on the wire. The child mode is bounded: its hold expires on its
// own, and the capture cleanup kills and reaps an orphaned child after a
// bounded wait even when an assertion fails.
func TestStartSupervisorHandoffPartialFrameClosesChild(t *testing.T) {
	t.Setenv(supervisorHandoffPartialFrameChildEnv, "30s")
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if result.accepted {
		t.Fatal("partial reply frame reported acceptance")
	}
	if err == nil {
		t.Fatal("partial reply frame returned no error")
	}
	if !strings.Contains(err.Error(), "read reply frame") ||
		!strings.Contains(err.Error(), "read payload") ||
		!strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("error %q does not identify the partial-frame read failure", err)
	}
	if strings.Contains(err.Error(), "decode reply frame") {
		t.Fatalf("error %q reports a decode failure: the partial frame must fail at the read", err)
	}

	// The starter closed and reaped the still-live child.
	assertRoleChildGone(t, pid)

	// The child's acceptance was already durable when the frame was cut:
	// the damaged frame is a deliberate post-acceptance wire truncation,
	// and the killed child never rolled the acceptance back.
	snapPath := filepath.Join(backgroundRoot, req.runID, "snapshot.json")
	if _, statErr := os.Stat(snapPath); statErr != nil {
		t.Fatalf("child acceptance not durable when the partial frame was sent: %v", statErr)
	}
	st := readAdmissionState(t, admissionRoot)
	if len(st.Tickets) != len(req.tasks) {
		t.Fatalf("durable tickets = %d, want %d: %+v", len(st.Tickets), len(req.tasks), st.Tickets)
	}
	for i, tk := range st.Tickets {
		if tk.RunID != req.runID || tk.WorkerID != i+1 || tk.State != "queued" || tk.OwnerPID != pid {
			t.Errorf("ticket[%d] mismatch: %+v", i, tk)
		}
	}

	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffEarlyExitClosesChild runs the handoff against
// a child that exits immediately after spawn: the send or the reply read
// fails, and the handoff must report non-accepted, close and reap the
// already-dying child, and leave both state roots empty.
func TestStartSupervisorHandoffEarlyExitClosesChild(t *testing.T) {
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	exe := writeRoleProcessQuickExitScript(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, exe, req, start)
	if result.accepted {
		t.Fatal("early-exiting child reported acceptance")
	}
	if err == nil {
		t.Fatal("early-exiting child returned no error")
	}
	assertRoleChildGone(t, pid)
	requireDirEmpty(t, backgroundRoot)
	requireDirEmpty(t, admissionRoot)
	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffBlockedSendCancelClosesAndReaps starts the
// handoff against a child that never reads its request pipe and sends a
// request payload far beyond any pipe capacity, so the Send blocks. The
// cancellation must close and reap the child, unblock and drain the
// in-flight send, and report the cancellation with accepted=false.
func TestStartSupervisorHandoffBlockedSendCancelClosesAndReaps(t *testing.T) {
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	req.tasks[0].Prompt = strings.Repeat("π", 1<<20) // 1 MiB: beyond any pipe buffer
	exe := writeRoleProcessSleepScript(t)
	var proc *roleProcess
	var pid int
	start, spawned := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan handoffOutcome, 1)
	go func() {
		result, err := startSupervisorHandoffWithProcess(ctx, exe, req, start)
		done <- handoffOutcome{result: result, err: err}
	}()
	waitHandoffSpawn(t, spawned)
	// Settle past process startup so the oversized send is blocked inside
	// the pipe before the cancellation lands.
	time.Sleep(100 * time.Millisecond)
	cancel()

	out := waitHandoffOutcome(t, done, 20*time.Second)
	if out.result.accepted {
		t.Fatal("canceled blocked-send handoff reported acceptance")
	}
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("error %v does not carry the cancellation", out.err)
	}
	assertRoleChildGone(t, pid)
	requireDirEmpty(t, backgroundRoot)
	requireDirEmpty(t, admissionRoot)
	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffBlockedReceiveCancelClosesAndReaps starts the
// handoff against a child that never answers, so the Receive blocks after
// the request was sent. The cancellation must close and reap the child,
// unblock and drain the in-flight receive, and report the cancellation
// with accepted=false.
func TestStartSupervisorHandoffBlockedReceiveCancelClosesAndReaps(t *testing.T) {
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	exe := writeRoleProcessSleepScript(t)
	var proc *roleProcess
	var pid int
	start, spawned := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan handoffOutcome, 1)
	go func() {
		result, err := startSupervisorHandoffWithProcess(ctx, exe, req, start)
		done <- handoffOutcome{result: result, err: err}
	}()
	waitHandoffSpawn(t, spawned)
	// Settle past the request send so the reply read is blocked before
	// the cancellation lands.
	time.Sleep(150 * time.Millisecond)
	cancel()

	out := waitHandoffOutcome(t, done, 20*time.Second)
	if out.result.accepted {
		t.Fatal("canceled blocked-receive handoff reported acceptance")
	}
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("error %v does not carry the cancellation", out.err)
	}
	assertRoleChildGone(t, pid)
	requireDirEmpty(t, backgroundRoot)
	requireDirEmpty(t, admissionRoot)
	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffDeadlineClosesAndReaps starts the handoff
// against a child that never answers under a bounded deadline: the
// deadline must close and reap the child and report accepted=false with
// the deadline error.
func TestStartSupervisorHandoffDeadlineClosesAndReaps(t *testing.T) {
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	exe := writeRoleProcessSleepScript(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, exe, req, start)
	if result.accepted {
		t.Fatal("deadline handoff reported acceptance")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v does not carry the deadline", err)
	}
	if proc != nil {
		assertRoleChildGone(t, pid)
	}
	requireDirEmpty(t, backgroundRoot)
	requireDirEmpty(t, admissionRoot)
	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffCanceledContextNeverStarts verifies that a
// context canceled before the call fails the handoff before any process
// is created: the process starter is never consulted.
func TestStartSupervisorHandoffCanceledContextNeverStarts(t *testing.T) {
	calls := 0
	start := func(string, role) (*roleProcess, error) {
		calls++
		return nil, errors.New("process starter must not be consulted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), validStartRequest(), start)
	if result.accepted {
		t.Fatal("canceled handoff reported acceptance")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not carry the cancellation", err)
	}
	if calls != 0 {
		t.Fatalf("process starter consulted %d times by a canceled handoff", calls)
	}
}

// TestStartSupervisorHandoffEncodeFailureStartsNothing verifies that an
// invalid request fails at encode time, before any process is created.
func TestStartSupervisorHandoffEncodeFailureStartsNothing(t *testing.T) {
	req := validStartRequest()
	req.runID = "bogus"
	calls := 0
	start := func(string, role) (*roleProcess, error) {
		calls++
		return nil, errors.New("process starter must not be consulted")
	}

	result, err := startSupervisorHandoffWithProcess(context.Background(), testExe(t), req, start)
	if result.accepted {
		t.Fatal("invalid request reported acceptance")
	}
	if err == nil {
		t.Fatal("invalid request returned no error")
	}
	if !strings.Contains(err.Error(), "encode supervisor start request") {
		t.Fatalf("error %q does not report the request validation failure", err)
	}
	if calls != 0 {
		t.Fatalf("process starter consulted %d times by an invalid request", calls)
	}
}

// TestStartSupervisorHandoffStartFailureReportsError verifies that a
// failed process start reports the failure with accepted=false.
func TestStartSupervisorHandoffStartFailureReportsError(t *testing.T) {
	injected := errors.New("injected start failure")
	start := func(string, role) (*roleProcess, error) {
		return nil, injected
	}

	result, err := startSupervisorHandoffWithProcess(context.Background(), testExe(t), validStartRequest(), start)
	if result.accepted {
		t.Fatal("failed start reported acceptance")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("error %v does not carry the injected start failure", err)
	}
	if !strings.Contains(err.Error(), "start role process") {
		t.Fatalf("error %q does not name the failed process start", err)
	}
}

// TestStartSupervisorHandoffDetachDiagnosticKeepsAcceptedSupervisor
// sabotages the parent request writer exactly at the after-bind
// linearization point, so Detach records a close diagnostic. The handoff
// must still report accepted=true with the accepted snapshot and the
// diagnostic, release the process handle, and never kill or roll back the
// accepted supervisor.
func TestStartSupervisorHandoffDetachDiagnosticKeepsAcceptedSupervisor(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "1s")
	req, _, _ := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	supervisorStartCancelProbe = func(phase supervisorStartCancelPhase) {
		if phase == supervisorStartCancelAfterBind {
			// Pre-close the parent response reader behind Detach's back:
			// the request writer was already closed by the handshake, so
			// this is the one close Detach must still perform.
			if err := (*proc).responseReader.Close(); err != nil {
				t.Errorf("preclose response reader: %v", err)
			}
		}
	}
	defer func() { supervisorStartCancelProbe = nil }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if !result.accepted {
		t.Fatal("detach diagnostic rolled back the acceptance")
	}
	if err == nil {
		t.Fatal("detach diagnostic handoff returned no error")
	}
	if !strings.Contains(err.Error(), "role process detach") ||
		!strings.Contains(err.Error(), "detach accepted supervisor") {
		t.Fatalf("error %q does not carry the detach diagnostic", err)
	}
	if result.snapshot.RunID != req.runID {
		t.Fatalf("accepted snapshot lost across the detach diagnostic: runId=%q", result.snapshot.RunID)
	}

	// The accepted supervisor was released, stays alive, and later exits
	// voluntarily: the diagnostic never killed it.
	assertRoleProcessReleased(t, proc)
	assertRoleChildAlive(t, pid)
	status := reapHandoffChild(t, pid, 4*time.Second)
	assertVoluntaryExit(t, status)
	assertRoleChildGone(t, pid)
}

// TestStartSupervisorHandoffOversizedRequestNeverStarts verifies that an
// encoded request larger than the private frame limit is rejected before
// any context check and before any process creation: the error names the
// encoded byte count and the limit, and the process starter is never
// consulted even under an already-canceled context, proving the size
// check precedes both the context check and the process start.
func TestStartSupervisorHandoffOversizedRequestNeverStarts(t *testing.T) {
	req := validStartRequest()
	req.tasks[0].Prompt = strings.Repeat("x", privateFrameLimit) // encoded JSON exceeds the limit
	payload, err := encodeSupervisorStartRequest(req)
	if err != nil {
		t.Fatalf("encode oversized request: %v", err)
	}
	if len(payload) <= privateFrameLimit {
		t.Fatalf("oversized fixture encodes to %d bytes, want more than the %d byte limit", len(payload), privateFrameLimit)
	}
	calls := 0
	start := func(string, role) (*roleProcess, error) {
		calls++
		return nil, errors.New("process starter must not be consulted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if result.accepted {
		t.Fatal("oversized request reported acceptance")
	}
	if err == nil {
		t.Fatal("oversized request returned no error")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error %v reports the cancellation instead of the oversize rejection: the size check must precede the context check", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("payload is %d bytes", len(payload))) {
		t.Fatalf("error %q does not name the encoded byte count %d", err, len(payload))
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("frame limit of %d bytes", privateFrameLimit)) {
		t.Fatalf("error %q does not name the private frame limit %d", err, privateFrameLimit)
	}
	if calls != 0 {
		t.Fatalf("process starter consulted %d times by an oversized request", calls)
	}
}

// TestStartSupervisorHandoffCloseRequestDiagnosticStillAcceptsAndDetaches
// injects a CloseRequest failure after the request frame was sent
// successfully. The handoff must preserve the wrapped diagnostic, still
// read, decode, and bind the one reply, detach the live accepted
// supervisor — never calling Close because of the diagnostic — and
// return accepted=true with the accepted snapshot plus the close
// diagnostic joined with the Detach diagnostic.
func TestStartSupervisorHandoffCloseRequestDiagnosticStillAcceptsAndDetaches(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "1s")
	req, _, _ := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	sabotageSupervisorStartCloseRequest(t, &proc)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if !result.accepted {
		t.Fatal("close request diagnostic rolled back the acceptance")
	}
	if err == nil {
		t.Fatal("close request diagnostic handoff returned no error")
	}
	if !strings.Contains(err.Error(), "close request writer") ||
		!strings.Contains(err.Error(), "file already closed") {
		t.Fatalf("error %q does not carry the wrapped close request diagnostic", err)
	}
	if !strings.Contains(err.Error(), "detach accepted supervisor") {
		t.Fatalf("error %q does not carry the joined detach diagnostic", err)
	}
	if result.snapshot.RunID != req.runID || result.snapshot.Supervisor.PID != pid {
		t.Fatalf("accepted snapshot not bound to the request and child: runId=%q pid=%d",
			result.snapshot.RunID, result.snapshot.Supervisor.PID)
	}

	// The accepted supervisor was detached, not killed: released handle,
	// live exact child, voluntary exit.
	assertRoleProcessReleased(t, proc)
	assertRoleChildAlive(t, pid)
	status := reapHandoffChild(t, pid, 4*time.Second)
	assertVoluntaryExit(t, status)
	assertRoleChildGone(t, pid)

	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffCloseRequestDiagnosticJoinsRejection injects
// the CloseRequest failure into a handoff the child rejects: the returned
// non-accepted error must carry the rejection and the joined close
// request diagnostic, the child must be closed and reaped, and the
// child's rollback must stay untouched on disk.
func TestStartSupervisorHandoffCloseRequestDiagnosticJoinsRejection(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "1s")
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	runDir := filepath.Join(backgroundRoot, req.runID)
	marker := []byte("occupied run directory entry")
	if err := os.WriteFile(runDir, marker, 0o600); err != nil {
		t.Fatalf("write occupied run dir entry: %v", err)
	}
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	sabotageSupervisorStartCloseRequest(t, &proc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if result.accepted {
		t.Fatal("rejected handoff reported acceptance")
	}
	if err == nil {
		t.Fatal("rejected handoff returned no error")
	}
	if !strings.Contains(err.Error(), "supervisor rejected the start request") {
		t.Fatalf("error %q does not report the rejection", err)
	}
	if !strings.Contains(err.Error(), "close request writer") {
		t.Fatalf("error %q does not join the close request diagnostic", err)
	}
	assertRoleChildGone(t, pid)

	// The child rolled back before rejecting: the occupied entry is the
	// only content under the background root, and no durable ticket
	// remains.
	if names := dirEntryNames(t, backgroundRoot); len(names) != 1 || names[0] != req.runID {
		t.Fatalf("background root content = %v, want only the occupied run dir entry %q", names, req.runID)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("durable tickets left after the rejection: %+v", st.Tickets)
	}
}

// TestStartSupervisorHandoffCloseRequestDiagnosticJoinsReadFailure
// injects the CloseRequest failure into a handoff against a child that
// drains the one request frame and exits without ever answering: the
// reply read fails, and the returned non-accepted error must join the
// close request diagnostic while the child is closed and reaped.
func TestStartSupervisorHandoffCloseRequestDiagnosticJoinsReadFailure(t *testing.T) {
	req := validStartRequest()
	exe := writeRoleProcessReadThenExitScript(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	sabotageSupervisorStartCloseRequest(t, &proc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, exe, req, start)
	if result.accepted {
		t.Fatal("read failure reported acceptance")
	}
	if err == nil {
		t.Fatal("read failure returned no error")
	}
	if !strings.Contains(err.Error(), "read reply frame") {
		t.Fatalf("error %q does not report the failed reply read", err)
	}
	if !strings.Contains(err.Error(), "close request writer") {
		t.Fatalf("error %q does not join the close request diagnostic", err)
	}
	assertRoleChildGone(t, pid)
}

// TestStartSupervisorHandoffCloseRequestDiagnosticJoinsDecodeFailure
// injects the CloseRequest failure into a handoff whose reply is the
// echo-loop child's request echo — never a valid start reply. The strict
// decode failure must join the close request diagnostic, and the child
// must be closed and reaped.
func TestStartSupervisorHandoffCloseRequestDiagnosticJoinsDecodeFailure(t *testing.T) {
	req, _, _ := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	sabotageSupervisorStartCloseRequest(t, &proc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if result.accepted {
		t.Fatal("malformed reply reported acceptance")
	}
	if err == nil {
		t.Fatal("malformed reply returned no error")
	}
	if !strings.Contains(err.Error(), "decode supervisor start reply") {
		t.Fatalf("error %q does not report the strict decode failure", err)
	}
	if !strings.Contains(err.Error(), "close request writer") {
		t.Fatalf("error %q does not join the close request diagnostic", err)
	}
	assertRoleChildGone(t, pid)
}

// TestStartSupervisorHandoffCloseRequestDiagnosticJoinsCancelBeforeDecode
// injects the CloseRequest failure and fires the handshake cancellation
// at the before-decode linearization point: the complete reply frame has
// arrived but nothing is decoded. The cancellation must still win —
// accepted=false carrying the cancellation — and the returned error must
// join the close request diagnostic while the child is closed and
// reaped.
func TestStartSupervisorHandoffCloseRequestDiagnosticJoinsCancelBeforeDecode(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "1s")
	req, _, _ := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	sabotageSupervisorStartCloseRequest(t, &proc)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	supervisorStartCancelProbe = func(phase supervisorStartCancelPhase) {
		if phase == supervisorStartCancelBeforeDecode {
			cancel()
		}
	}
	defer func() { supervisorStartCancelProbe = nil }()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if result.accepted {
		t.Fatal("cancellation before decoding reported acceptance")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not carry the cancellation", err)
	}
	if !strings.Contains(err.Error(), "close request writer") {
		t.Fatalf("error %q does not join the close request diagnostic", err)
	}
	assertRoleChildGone(t, pid)
}

// TestBindSupervisorStartAccepted verifies the binding of an accepted
// reply snapshot to the sent request and to the exact spawned supervisor
// PID: an all-matching snapshot binds cleanly, while every single-field
// deviation — and a supervisor PID deviation — is reported as a protocol
// failure. The accepted-time comparison tolerates exactly the whole-second
// UTC normalization NewSnapshot applies.
func TestBindSupervisorStartAccepted(t *testing.T) {
	const supervisorPID = 4242
	req := validStartRequest()
	base, err := NewSnapshot(req.runID, req.acceptedAt, req.workspace,
		ProcessIdentity{PID: supervisorPID, CreateTime: 7}, req.tasks, req.executionTimeout, req.worktree)
	if err != nil {
		t.Fatalf("build baseline snapshot: %v", err)
	}

	if err := bindSupervisorStartAccepted(req, base, supervisorPID); err != nil {
		t.Fatalf("all-matching snapshot failed binding: %v", err)
	}

	cases := []struct {
		name string
		want string // substring of the binding error
		mut  func(*Snapshot)
	}{
		{"runId", "runId", func(s *Snapshot) { s.RunID = "20250102T101113Z-4242" }},
		{"acceptedAt", "acceptedAt", func(s *Snapshot) { s.AcceptedAt = s.AcceptedAt.Add(time.Second) }},
		{"updatedAt", "updatedAt", func(s *Snapshot) { s.UpdatedAt = s.UpdatedAt.Add(time.Second) }},
		{"workspace", "workspace", func(s *Snapshot) { s.Workspace = "other-workspace" }},
		{"worktree nil", "worktree", func(s *Snapshot) { s.Worktree = nil }},
		{"worktree field", "worktree", func(s *Snapshot) { s.Worktree.Name = "issue-999" }},
		{"worker count", "workers", func(s *Snapshot) { s.Workers = s.Workers[:1] }},
		{"task projection", "task projection", func(s *Snapshot) { s.Workers[0].Task.Prompt = "tampered prompt" }},
		{"task data metadata", "task projection", func(s *Snapshot) { s.Workers[0].Task.Data[0].Path = "tampered/path.bin" }},
		{"executionTimeout", "executionTimeout", func(s *Snapshot) { s.Workers[0].ExecutionTimeout = "1h" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := base
			tc.mut(&snap)
			err := bindSupervisorStartAccepted(req, snap, supervisorPID)
			if err == nil {
				t.Fatalf("binding accepted a mismatched snapshot (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("binding error %q does not name the %s mismatch", err, tc.name)
			}
		})
	}

	t.Run("supervisor pid", func(t *testing.T) {
		err := bindSupervisorStartAccepted(req, base, supervisorPID+1)
		if err == nil {
			t.Fatal("binding accepted a snapshot for a different supervisor pid")
		}
		if !strings.Contains(err.Error(), "supervisor pid") {
			t.Fatalf("binding error %q does not name the supervisor pid mismatch", err)
		}
	})

	t.Run("sub-second acceptedAt normalizes", func(t *testing.T) {
		// NewSnapshot truncates acceptedAt to whole seconds in UTC; the
		// binding compares the same normalized instant, so a request whose
		// acceptedAt carries sub-second precision still binds.
		sub := validStartRequest()
		sub.acceptedAt = time.Date(2025, 1, 2, 10, 11, 12, 500_000_000, time.UTC)
		snap, err := NewSnapshot(sub.runID, sub.acceptedAt, sub.workspace,
			ProcessIdentity{PID: supervisorPID, CreateTime: 7}, sub.tasks, sub.executionTimeout, sub.worktree)
		if err != nil {
			t.Fatalf("build sub-second snapshot: %v", err)
		}
		if err := bindSupervisorStartAccepted(sub, snap, supervisorPID); err != nil {
			t.Fatalf("sub-second acceptedAt failed binding: %v", err)
		}
	})

	t.Run("request without worktree", func(t *testing.T) {
		plain := minimalStartRequest()
		snap, err := NewSnapshot(plain.runID, plain.acceptedAt, plain.workspace,
			ProcessIdentity{PID: supervisorPID, CreateTime: 7}, plain.tasks, plain.executionTimeout, nil)
		if err != nil {
			t.Fatalf("build worktree-less snapshot: %v", err)
		}
		if err := bindSupervisorStartAccepted(plain, snap, supervisorPID); err != nil {
			t.Fatalf("worktree-less request failed binding: %v", err)
		}
	})
}

// TestBoundSupervisorStartRejection verifies the rejection reason bound:
// text at or under the cap is verbatim, longer text is truncated at the
// cap on a UTF-8 character boundary without ever splitting a rune.
func TestBoundSupervisorStartRejection(t *testing.T) {
	short := "a bounded rejection reason"
	if got := boundSupervisorStartRejection(short); got != short {
		t.Fatalf("short reason %q was rewritten to %q", short, got)
	}

	long := strings.Repeat("界", 5000) // 15000 bytes, well over the cap
	got := boundSupervisorStartRejection(long)
	if len(got) > maxSupervisorStartRejectionReason {
		t.Fatalf("bounded reason length %d exceeds the cap %d", len(got), maxSupervisorStartRejectionReason)
	}
	if !utf8.ValidString(got) {
		t.Fatal("bounded reason is not valid UTF-8")
	}
	if !strings.HasPrefix(long, got) {
		t.Fatal("bounded reason is not a prefix of the original reason")
	}

	// 4098 bytes of a 3-byte rune: the 4096-byte cut lands inside a rune
	// and must back off to the last complete character (4095 bytes).
	midRune := strings.Repeat("界", 1366)
	got = boundSupervisorStartRejection(midRune)
	if !utf8.ValidString(got) {
		t.Fatal("mid-rune cut produced invalid UTF-8")
	}
	if len(got) != 4095 {
		t.Fatalf("mid-rune cut length = %d, want 4095", len(got))
	}
}
