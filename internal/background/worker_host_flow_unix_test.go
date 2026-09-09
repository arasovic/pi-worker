//go:build darwin || linux

package background

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
	"golang.org/x/sys/unix"
)

// supervisorDriverEvent is one JSON-lines event the flow-test supervisor
// driver appends to its outcome file, so the starter test can observe
// the tree's progress and final outcome durably.
type supervisorDriverEvent struct {
	Type       string           `json:"t"`
	Supervisor int              `json:"supervisorPid,omitempty"`
	WorkerID   int              `json:"workerId,omitempty"`
	PID        int              `json:"pid,omitempty"`
	Result     *pi.WorkerResult `json:"result,omitempty"`
	PiPIDs     []int            `json:"piPids,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// appendSupervisorDriverEvent appends one event as a JSON line to the
// outcome file.
func appendSupervisorDriverEvent(path string, event supervisorDriverEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}

// readSupervisorDriverEvents decodes every JSON line of the outcome
// file; a missing file yields nil.
func readSupervisorDriverEvents(t *testing.T, path string) []supervisorDriverEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var events []supervisorDriverEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var event supervisorDriverEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode driver event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// waitSupervisorDriverEvent polls the outcome file until an event of the
// wanted type appears and returns it.
func waitSupervisorDriverEvent(t *testing.T, path, wantType string) supervisorDriverEvent {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, event := range readSupervisorDriverEvents(t, path) {
			if event.Type == wantType {
				return event
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("driver event %q never appeared in %s; log so far: %v", wantType, path, readSupervisorDriverEvents(t, path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flowStartRequest returns one valid supervisor start request for the
// flow tests: one task for the fakepi catalog, a real workspace, the
// built fakepi as the Pi executable, and fresh temporary state roots.
func flowStartRequest(t *testing.T) (req supervisorStartRequest, backgroundRoot, admissionRoot string) {
	t.Helper()
	req = minimalStartRequest()
	req.tasks = []run.Task{{
		Prompt: "run the focused task",
		Model:  "acme/m-1",
	}}
	req.workspace = t.TempDir()
	req.backgroundRoot = t.TempDir()
	req.admissionRoot = t.TempDir()
	req.piExecutable = fakePiBin(t)
	req.executionTimeout = 5 * time.Minute
	return req, req.backgroundRoot, req.admissionRoot
}

// supervisorDriverExitCode names one driver failure exit code.
const supervisorDriverExitCode = 75

// runSupervisorWorkerDriver is the opt-in TestMain child mode that
// stands in for the future production supervisor dispatch slice: after
// the real child-side supervisor start exchange it consumes its own
// prepared admission ticket (worker 1), runs one real worker-host
// execution through the production adapter, releases the lease, records
// every step as a durable JSON-lines event, holds for the bounded hold,
// and exits 0. It never returns.
func runSupervisorWorkerDriver(pipes *childRolePipes, outcomePath, hold string) {
	exitErr := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "supervisor driver: "+format+"\n", args...)
		os.Exit(supervisorDriverExitCode)
	}

	result, err := receiveSupervisorStart(pipes)
	if err != nil {
		exitErr("exchange: %v", err)
	}
	if !result.accepted || result.preparation == nil {
		exitErr("start request not accepted")
	}
	req := result.request
	appendSupervisorDriverEvent(outcomePath, supervisorDriverEvent{Type: "accepted", Supervisor: os.Getpid()})

	// Consume the already prepared admission ticket for worker 1: the
	// ticket was durably prepared by the exchange under this supervisor's
	// own identity, and the worker host itself never enqueues a ticket.
	ticket := result.preparation.tickets[0]
	queueCtx, queueCancel := context.WithTimeout(context.Background(), 30*time.Second)
	lease, err := ticket.Wait(queueCtx)
	queueCancel()
	if err != nil {
		exitErr("consume prepared ticket: %v", err)
	}
	appendSupervisorDriverEvent(outcomePath, supervisorDriverEvent{Type: "ticket-granted", Supervisor: os.Getpid()})

	// Run exactly one worker-host execution for task 1 through the
	// production adapter, exactly as the future supervisor dispatch will.
	exe, err := os.Executable()
	if err != nil {
		exitErr("resolve executable: %v", err)
	}
	// The spawned worker-host child must dispatch the real handler.
	_ = os.Setenv(workerHostExecuteEnv, "1")
	task := req.tasks[0]
	var piPIDs []int
	adapter := newWorkerHostAdapter(exe, req.piExecutable)
	runCtx, runCancel := context.WithTimeout(context.Background(), req.executionTimeout)
	workerResult := adapter.Run(runCtx, pi.WorkerRequest{
		Model:         task.Model,
		ThinkingLevel: task.ThinkingLevel,
		Prompt:        task.Prompt,
		Workspace:     req.workspace,
		WorkerID:      1,
		OnProcessStart: func(workerID, pid int) {
			piPIDs = append(piPIDs, pid)
			appendSupervisorDriverEvent(outcomePath, supervisorDriverEvent{
				Type: "process-start", Supervisor: os.Getpid(), WorkerID: workerID, PID: pid,
			})
		},
	})
	runCancel()

	releaseErr := lease.Release()
	releaseError := ""
	if releaseErr != nil {
		releaseError = releaseErr.Error()
		appendSupervisorDriverEvent(outcomePath, supervisorDriverEvent{Type: "release-error", Error: releaseError})
	}
	appendSupervisorDriverEvent(outcomePath, supervisorDriverEvent{
		Type: "done", Supervisor: os.Getpid(), Result: &workerResult, PiPIDs: piPIDs, Error: releaseError,
	})

	holdDuration, perr := time.ParseDuration(hold)
	if perr != nil || holdDuration < 0 {
		holdDuration = 0
	}
	time.Sleep(holdDuration)
	os.Exit(0)
}

// supervisorDriverHold is the bounded hold a passing flow-test driver
// sleeps after its done event before exiting 0.
const supervisorDriverHold = 1500 * time.Millisecond

// flowScript drives fakepi through the full worker lifecycle with a
// bounded pause before the prompt settles, so the run stays observable
// for a deterministic window.
func flowScript(finalText string) *script.Script {
	cfg := happyPathScript(finalText)
	cfg.Triggers["prompt"] = []script.Step{
		{SleepMS: 500},
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"agent_start"}`)},
		{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The answer is 42."}]}}`)},
		{Event: json.RawMessage(`{"type":"turn_end","message":{},"toolResults":[]}`)},
		{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
		{Event: json.RawMessage(`{"type":"agent_settled"}`)},
	}
	return cfg
}

// TestWorkerHostFlowStarterExitAllowsExecutionToComplete runs the exact
// production tree starter -> accepted supervisor -> worker-host -> fake
// Pi through test-only role dispatch: the starter's handoff accepts and
// detaches, and the detached supervisor consumes its prepared ticket,
// runs the host to completion, releases the lease, and exits voluntarily
// with code 0. The starter observes durable events and state only.
func TestWorkerHostFlowStarterExitAllowsExecutionToComplete(t *testing.T) {
	req, backgroundRoot, admissionRoot := flowStartRequest(t)
	outcomePath := filepath.Join(t.TempDir(), "driver-outcome.jsonl")
	t.Setenv(supervisorDriverOutcomeEnv, outcomePath)
	t.Setenv(supervisorDriverHoldEnv, supervisorDriverHold.String())
	setupFakePiEnv(t, flowScript("flow answer"))
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	var proc *roleProcess
	var supervisorPID int
	start, _ := captureHandoffStart(t, &proc, &supervisorPID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handoff, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if err != nil || !handoff.accepted {
		t.Fatalf("accepted handoff: accepted=%v err=%v", handoff.accepted, err)
	}
	if handoff.snapshot.Supervisor.PID != supervisorPID {
		t.Fatalf("snapshot supervisor pid %d != spawned supervisor pid %d", handoff.snapshot.Supervisor.PID, supervisorPID)
	}

	// The supervisor consumed its already prepared ticket: the durable
	// ticket state must pass through the leased state while the host runs
	// and end empty after the lease release.
	observedLeased := false
	deadline := time.Now().Add(30 * time.Second)
	for {
		st := readAdmissionState(t, admissionRoot)
		if len(st.Tickets) == 1 && st.Tickets[0].State == "leased" && st.Tickets[0].OwnerPID == supervisorPID {
			observedLeased = true
		}
		doneSeen := false
		for _, event := range readSupervisorDriverEvents(t, outcomePath) {
			if event.Type == "done" {
				doneSeen = true
			}
		}
		if doneSeen && observedLeased && len(st.Tickets) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ticket was never observed leased then released; leased=%v state=%+v events=%v",
				observedLeased, readAdmissionState(t, admissionRoot), readSupervisorDriverEvents(t, outcomePath))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The execution completed after the starter returned from the
	// handoff: the done event carries the host's terminal result with its
	// fields preserved.
	done := waitSupervisorDriverEvent(t, outcomePath, "done")
	if done.Result == nil || done.Result.Status != pi.StatusCompleted || done.Result.Explanation != "flow answer" {
		t.Fatalf("done result = %+v, want completed with the fake Pi answer", done.Result)
	}
	if done.Supervisor != supervisorPID {
		t.Fatalf("done event supervisor pid %d != %d", done.Supervisor, supervisorPID)
	}
	if len(done.PiPIDs) != 1 || done.PiPIDs[0] <= 0 {
		t.Fatalf("pi pids = %v, want one positive pid", done.PiPIDs)
	}
	startEvent := waitSupervisorDriverEvent(t, outcomePath, "process-start")
	if startEvent.WorkerID != 1 || startEvent.PID != done.PiPIDs[0] {
		t.Fatalf("process-start event = %+v, want worker 1 with the recorded pi pid %d", startEvent, done.PiPIDs[0])
	}

	// Durable outcomes: the accepted snapshot is untouched (the run
	// lifecycle update slice is out of scope), the admission state is
	// empty, and the fake Pi exited.
	store, err := NewStore(backgroundRoot)
	if err != nil {
		t.Fatalf("construct store: %v", err)
	}
	loaded, err := store.Load(req.runID)
	if err != nil {
		t.Fatalf("load accepted snapshot: %v", err)
	}
	if loaded.State != RunAccepted || loaded.Terminal {
		t.Fatalf("snapshot state = %q terminal=%v, want untouched accepted", loaded.State, loaded.Terminal)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 || st.NextSequence != 2 {
		t.Fatalf("final admission state = %+v, want one consumed ticket and the sequence advanced to 2", st)
	}
	piPID := readPIDFile(t, pidPath)
	t.Cleanup(func() {
		if processAlive(piPID) {
			_ = unix.Kill(piPID, unix.SIGKILL)
		}
		waitProcessGone(t, piPID)
	})
	waitProcessGone(t, piPID)

	// The detached supervisor exited voluntarily with code 0 once its
	// bounded hold expired — the starter never killed it.
	status := reapHandoffChild(t, supervisorPID, 10*time.Second)
	assertVoluntaryExit(t, status)
	assertRoleChildGone(t, supervisorPID)
}

// TestWorkerHostFlowSupervisorHardKillCleansPiAndDescendants runs the
// same tree with the fake Pi run in flight and a spawned descendant, then
// hard-kills the supervisor: the host observes ownership EOF, cancels,
// and cleans Pi plus the attributable descendant. The durable admission
// state honestly preserves the killed supervisor's leased ticket until a
// future reconciliation reaps it.
func TestWorkerHostFlowSupervisorHardKillCleansPiAndDescendants(t *testing.T) {
	req, backgroundRoot, admissionRoot := flowStartRequest(t)
	outcomePath := filepath.Join(t.TempDir(), "driver-outcome.jsonl")
	t.Setenv(supervisorDriverOutcomeEnv, outcomePath)
	t.Setenv(supervisorDriverHoldEnv, "60s")
	setupFakePiEnv(t, stuckPathScript())
	piPIDPath := filepath.Join(t.TempDir(), "fakepi.pid")
	descendantPIDPath := filepath.Join(t.TempDir(), "fakepi-descendant.pid")
	t.Setenv("FAKEPI_PIDFILE", piPIDPath)
	t.Setenv("FAKEPI_SPAWN_PIDFILE", descendantPIDPath)

	var proc *roleProcess
	var supervisorPID int
	start, _ := captureHandoffStart(t, &proc, &supervisorPID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handoff, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if err != nil || !handoff.accepted {
		t.Fatalf("accepted handoff: accepted=%v err=%v", handoff.accepted, err)
	}

	// The run is in flight: the driver granted the prepared ticket and
	// the fake Pi (plus its attributable descendant) is live.
	waitSupervisorDriverEvent(t, outcomePath, "ticket-granted")
	startEvent := waitSupervisorDriverEvent(t, outcomePath, "process-start")
	piPID := readPIDFile(t, piPIDPath)
	descendantPID := readPIDFile(t, descendantPIDPath)
	if startEvent.PID != piPID {
		t.Fatalf("process-start event pid %d != fakepi pid %d", startEvent.PID, piPID)
	}
	if !processAlive(piPID) || !processAlive(descendantPID) {
		t.Fatalf("pi %d / descendant %d are not alive while the run is in flight", piPID, descendantPID)
	}
	// Defensive exact-pid cleanup on failure: no fakepi or descendant may
	// outlive this test.
	t.Cleanup(func() {
		for _, pid := range []int{descendantPID, piPID} {
			if processAlive(pid) {
				_ = unix.Kill(pid, unix.SIGKILL)
			}
		}
		waitProcessGone(t, piPID)
		waitProcessGone(t, descendantPID)
	})

	// Hard-kill the supervisor: its ownership writer closes, the host
	// observes ownership EOF, cancels, and cleans Pi and the descendant.
	if err := unix.Kill(supervisorPID, unix.SIGKILL); err != nil {
		t.Fatalf("kill supervisor %d: %v", supervisorPID, err)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(supervisorPID, &status, 0, nil); err != nil {
		t.Fatalf("wait supervisor %d: %v", supervisorPID, err)
	}
	if !status.Signaled() || status.Signal() != unix.SIGKILL {
		t.Fatalf("supervisor exit = %v, want SIGKILL", status)
	}
	assertRoleChildGone(t, supervisorPID)

	waitProcessGone(t, piPID)
	waitProcessGone(t, descendantPID)

	// Durable outcomes, honestly recorded: the accepted snapshot is
	// untouched, and the admission state preserves the killed
	// supervisor's leased ticket — reaped only by a future
	// reconciliation, which no test may run for it here.
	store, err := NewStore(backgroundRoot)
	if err != nil {
		t.Fatalf("construct store: %v", err)
	}
	loaded, err := store.Load(req.runID)
	if err != nil {
		t.Fatalf("load accepted snapshot: %v", err)
	}
	if loaded.State != RunAccepted {
		t.Fatalf("snapshot state = %q, want untouched accepted", loaded.State)
	}
	st := readAdmissionState(t, admissionRoot)
	if len(st.Tickets) != 1 || st.Tickets[0].State != "leased" || st.Tickets[0].OwnerPID != supervisorPID {
		t.Fatalf("durable admission state = %+v, want the killed supervisor's leased ticket (pid %d)", st.Tickets, supervisorPID)
	}
	if st.Tickets[0].RunID != req.runID || st.Tickets[0].WorkerID != 1 {
		t.Fatalf("leased ticket = %+v, want run %s worker 1", st.Tickets[0], req.runID)
	}
	// The driver never reached its done event.
	for _, event := range readSupervisorDriverEvents(t, outcomePath) {
		if event.Type == "done" {
			t.Fatalf("driver reported done after its supervisor was hard-killed: %+v", event)
		}
	}
}
