//go:build darwin || linux

package background

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/admission"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// managerTerminalWait bounds every wait for a started run to finish.
const managerTerminalWait = 90 * time.Second

// inFlightMS is how long the fake Pi holds one worker before it settles, so a
// test that has to observe a run while it is still going has a window that
// does not depend on machine speed.
const inFlightMS = 2000

// newTestManager returns a Manager over fresh roots and the roots themselves.
func newTestManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	root, admissionRoot := t.TempDir(), t.TempDir()
	m, err := NewManager(root, admissionRoot, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, root, admissionRoot
}

// managerStartOptions is one task run through the fake Pi in a scratch
// workspace, with no private checkout.
func managerStartOptions(t *testing.T) StartOptions {
	t.Helper()
	return StartOptions{
		Tasks:            []run.Task{{Prompt: "run the managed task", Model: "acme/m-1"}},
		Workspace:        t.TempDir(),
		ExecutionTimeout: 5 * time.Minute,
		PiExecutable:     fakePiBin(t),
	}
}

// inFlightScript is the happy path with the answer held back, so the run stays
// in flight long enough to be observed before it settles on its own.
func inFlightScript(finalText string) *script.Script {
	s := happyPathScript(finalText)
	prompt := s.Triggers["prompt"]
	held := make([]script.Step, 0, len(prompt)+1)
	held = append(held, prompt[0], prompt[1])
	held = append(held, script.Step{SleepMS: inFlightMS, Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"held"}]}}`)})
	held = append(held, prompt[3:]...)
	s.Triggers["prompt"] = held
	return s
}

// startManagedRun starts one run and returns the manager, its roots and the
// started run. It fails the test when the start is not accepted.
func startManagedRun(t *testing.T, scriptConfig *script.Script) (*Manager, string, string, StartedRun) {
	t.Helper()
	setupFakePiEnv(t, scriptConfig)
	m, root, admissionRoot := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The Manager spawns the role executable it resolves for itself, which
	// under `go test` is the test binary rather than the shipped program.
	started, err := m.startWithExecutable(ctx, piWorkerBin(t), managerStartOptions(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.RunID == "" || started.Snapshot.RunID != started.RunID {
		t.Fatalf("started run identity = %q, snapshot says %q", started.RunID, started.Snapshot.RunID)
	}
	if started.Snapshot.Terminal {
		t.Fatal("Start returned a terminal snapshot: it must return while the run is still going")
	}
	t.Cleanup(func() { waitForTerminal(t, m, started.RunID) })
	return m, root, admissionRoot, started
}

// waitForTerminal drains one started run so no test leaves a supervisor
// writing into a directory the test framework is removing.
func waitForTerminal(t *testing.T, m *Manager, runID string) {
	t.Helper()
	if _, err := m.Wait(context.Background(), runID, managerTerminalWait); err != nil {
		t.Errorf("drain run %s: %v", runID, err)
	}
}

// TestManagerStartedRunReachesTerminalAndWaitReturnsIt is the whole path in
// one test: a run started through the Manager runs to completion in a
// detached supervisor, and Wait returns exactly the terminal snapshot the
// supervisor wrote.
func TestManagerStartedRunReachesTerminalAndWaitReturnsIt(t *testing.T) {
	m, _, admissionRoot, started := startManagedRun(t, happyPathScript("managed answer"))

	snap, err := m.Wait(context.Background(), started.RunID, managerTerminalWait)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !snap.Terminal || snap.State != RunCompleted {
		t.Fatalf("waited snapshot: terminal=%v state=%q, want a completed terminal run", snap.Terminal, snap.State)
	}
	if len(snap.Workers) != 1 || snap.Workers[0].State != WorkerCompleted {
		t.Fatalf("workers = %+v, want one completed worker", snap.Workers)
	}
	if snap.Workers[0].Result == nil || snap.Workers[0].Result.Explanation != "managed answer" {
		t.Fatalf("worker result = %+v, want the fake Pi answer", snap.Workers[0].Result)
	}

	// Wait returns the snapshot that is on disk, not a reconstruction of it.
	stored, err := m.Status(started.RunID)
	if err != nil {
		t.Fatalf("Status after Wait: %v", err)
	}
	if !equalSnapshots(stored, snap) {
		t.Fatalf("Wait returned a snapshot the store does not hold:\n got: %+v\nwant: %+v", snap, stored)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Errorf("admission gate holds live leases after the run: %+v", st.Tickets)
	}
}

// TestManagerWaitTimeoutReturnsTheLatestNonTerminalSnapshot requires that a
// wait which ran out reports the run as it stands rather than as a failure,
// and that the run it gave up on keeps going and finishes on its own.
func TestManagerWaitTimeoutReturnsTheLatestNonTerminalSnapshot(t *testing.T) {
	m, _, _, started := startManagedRun(t, inFlightScript("held answer"))

	snap, err := m.Wait(context.Background(), started.RunID, 150*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v, want the deadline that ended the wait", err)
	}
	if snap.Terminal {
		t.Fatalf("Wait reported a terminal snapshot after its own timeout: %+v", snap)
	}
	if snap.RunID != started.RunID {
		t.Fatalf("Wait reported run %q, want the run it was waiting on %q", snap.RunID, started.RunID)
	}

	// Nothing was cancelled: the run the wait gave up on still finishes.
	final, err := m.Wait(context.Background(), started.RunID, managerTerminalWait)
	if err != nil {
		t.Fatalf("second Wait: %v", err)
	}
	if !final.Terminal || final.State != RunCompleted {
		t.Fatalf("run after an expired wait: terminal=%v state=%q, want it to have finished", final.Terminal, final.State)
	}
}

// TestManagerStatusAnswersWhileTheRunIsInFlight requires that Status is a
// read and nothing else: it answers about a run that is still going, and it
// answers immediately.
func TestManagerStatusAnswersWhileTheRunIsInFlight(t *testing.T) {
	m, _, _, started := startManagedRun(t, inFlightScript("held answer"))

	before := time.Now()
	snap, err := m.Status(started.RunID)
	elapsed := time.Since(before)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if snap.Terminal {
		t.Fatalf("Status reported a terminal snapshot while the run was held in flight: %+v", snap)
	}
	if elapsed > time.Second {
		t.Fatalf("Status took %s: it must read once and answer, never wait for the run", elapsed)
	}
}

// TestManagerUnacceptedStartLeavesNothing requires that a start the
// supervisor refuses leaves no run state and no private checkout: the
// snapshot root here is a plain file, so the child's own Store.Create fails
// after its tickets were prepared, and everything prepared for the run has to
// be given back.
func TestManagerUnacceptedStartLeavesNothing(t *testing.T) {
	setupFakePiEnv(t, happyPathScript("never runs"))
	repo := newGitWorkspace(t)
	admissionRoot := t.TempDir()

	// A regular file where the snapshot root belongs: no run directory can be
	// created under it, so the supervisor rejects the start.
	root := filepath.Join(t.TempDir(), "occupied-root")
	if err := os.WriteFile(root, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("write occupied root: %v", err)
	}
	m, err := NewManager(root, admissionRoot, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var removed []worktree.Prepared
	opts := managerStartOptions(t)
	opts.Workspace = repo
	opts.WorktreeName = "manager-reject"
	opts.removeWorktree = func(ctx context.Context, cwd string, expected worktree.Prepared) error {
		removed = append(removed, expected)
		return worktree.RemoveUntouched(ctx, cwd, expected)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started, err := m.startWithExecutable(ctx, piWorkerBin(t), opts)
	if err == nil {
		t.Fatalf("Start was accepted with an unusable snapshot root: %+v", started)
	}

	if len(removed) != 1 {
		t.Fatalf("worktrees given back = %d, want exactly the one the start prepared: %+v", len(removed), removed)
	}
	if _, statErr := os.Stat(removed[0].Path); !os.IsNotExist(statErr) {
		t.Fatalf("private checkout %q survives a start that was not accepted: %v", removed[0].Path, statErr)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("admission tickets survive a start that was not accepted: %+v", st.Tickets)
	}
	gate, err := admission.Open(admissionRoot, 2)
	if err != nil {
		t.Fatalf("open admission gate: %v", err)
	}
	if err := gate.Reconcile(); err != nil {
		t.Fatalf("reconcile admission gate: %v", err)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("admission gate holds leases after an unaccepted start: %+v", st.Tickets)
	}
}

// newGitWorkspace returns a fresh repository with one commit: the directory a
// managed private checkout can be prepared beside.
func newGitWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write repository file: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "file.txt"},
		{"commit", "-q", "-m", "root"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}
