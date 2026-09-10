package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// backgroundRunDelayStep is how long the scripted fake Pi holds one worker's
// prompt open. It is long enough that the run is unmistakably still going at
// the moment a status is asked for and at the moment a short wait runs out,
// and short enough that a test which then waits for the run to finish is not
// waiting on anything but the run.
const backgroundRunDelayStep = 3 * time.Second

// slowBackgroundScript answers one worker with finalText after holding its
// prompt open for delayStep, so the run is observably in flight for that long.
func slowBackgroundScript(finalText string, delayStep time.Duration) *script.Script {
	s := backgroundHappyScript(finalText)
	steps := s.Triggers["prompt"]
	slow := make([]script.Step, 0, len(steps)+1)
	slow = append(slow, steps[0])
	slow = append(slow, script.Step{SleepMS: int(delayStep.Milliseconds())})
	s.Triggers["prompt"] = append(slow, steps[1:]...)
	return s
}

// setupBackgroundRun points one background run at scratch roots, the built
// binary and the fake Pi driven by the given script, and returns the Manager
// that reads the same state the command writes along with the root that state
// is stored under. The run is not started here; the test starts it, through
// the command under test.
func setupBackgroundRun(t *testing.T, s *script.Script) (*background.Manager, string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("background runs need a platform that can host a role process")
	}
	// The binary is built before the working directory moves: `go build`
	// needs to run inside the module, and the run itself must not.
	roleBin := piWorkerBinForBackground(t)
	setupFakePiScript(t, s)
	workspace := t.TempDir()
	t.Chdir(workspace)

	root, admissionRoot := t.TempDir(), t.TempDir()
	manager, err := background.NewManager(root, admissionRoot, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	originalManager, originalPi, originalRole := newBackgroundManager, backgroundPiExecutable, backgroundRoleExecutable
	newBackgroundManager = func(string, int) (*background.Manager, error) { return manager, nil }
	backgroundPiExecutable = fakePiBin
	backgroundRoleExecutable = roleBin
	t.Cleanup(func() {
		newBackgroundManager, backgroundPiExecutable, backgroundRoleExecutable = originalManager, originalPi, originalRole
	})
	return manager, root
}

// startBackgroundRun starts one background run through the real command and
// returns the identity the command reported, draining that run on the way
// out of the test so no run is left running after it.
func startBackgroundRun(t *testing.T, manager *background.Manager, args ...string) string {
	t.Helper()
	code, stdout, stderr := runCLI(t, append([]string{"run", "--background", "--json", "--model", "acme/m-1"}, args...), "")
	if code != 0 {
		t.Fatalf("run --background = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	runID, ok := decodeJSONObject(t, stdout)["runId"].(string)
	if !ok || runID == "" {
		t.Fatalf("accepted run reported no identity: %q", stdout)
	}
	t.Cleanup(func() {
		if _, err := manager.Wait(context.Background(), runID, 90*time.Second); err != nil {
			t.Errorf("drain run %s: %v", runID, err)
		}
	})
	return runID
}

// writeAlwaysFailingVerifyCommand writes a verification command that fails,
// and returns the argv that runs it. A background run and a foreground run
// given the same task and the same command must reach the same result, so
// the pair can be compared.
func writeAlwaysFailingVerifyCommand(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verify-fails")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write verification command: %v", err)
	}
	return []string{"--verify", path}
}

// TestRunsStatusOfARunInFlightAnswersImmediatelyAndReportsItGoing requires
// that a status of a run that is still going is one read and one report: it
// comes back while the run is unmistakably in flight, says the run is not
// finished, and returns without waiting for anything.
func TestRunsStatusOfARunInFlightAnswersImmediatelyAndReportsItGoing(t *testing.T) {
	manager, _ := setupBackgroundRun(t, slowBackgroundScript("in flight", backgroundRunDelayStep))
	runID := startBackgroundRun(t, manager, "--task", "go", "--timeout", "5m")

	started := time.Now()
	code, stdout, stderr := runCLI(t, []string{"runs", "status", runID}, "")
	elapsed := time.Since(started)
	// The command reads once and returns, so it cannot take the run's own
	// remaining time to answer; the bound is loose on purpose, and it is
	// the same bound the run is still going under.
	if elapsed >= backgroundRunDelayStep {
		t.Fatalf("runs status took %v for a run in flight; it must answer without waiting", elapsed)
	}
	if code != 0 || stderr != "" {
		t.Fatalf("runs status of a run in flight = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	// Still going: the identity, its state, and its worker's state, and no
	// result claimed — the outcome line is what a finished run prints.
	if !strings.Contains(stdout, runID) {
		t.Fatalf("stdout = %q, want the run identity", stdout)
	}
	if strings.Contains(stdout, "outcome=") {
		t.Fatalf("stdout = %q, want no outcome claimed for a run still going", stdout)
	}
	for _, want := range []string{"RUN ID", "STATE", "WORKER", "ANSWER"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want a %s column", stdout, want)
		}
	}
	if strings.Contains(stdout, string(background.RunCompleted)) {
		t.Fatalf("stdout = %q, want no finished state reported", stdout)
	}

	// What it printed is the durable state, and the state it printed was
	// not terminal: the same question asked of the run's own state agrees.
	snap, err := manager.Status(runID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if snap.Terminal {
		t.Fatalf("the run finished inside its own %v prompt step; state = %q", backgroundRunDelayStep, snap.State)
	}
	if !strings.Contains(stdout, string(snap.State)) {
		t.Fatalf("stdout = %q, want the run's own state %q", stdout, snap.State)
	}

	// --json of the same run is the stored document: the same fields, the
	// same non-terminal answer, and nothing invented for the command.
	code, stdout, stderr = runCLI(t, []string{"runs", "status", runID, "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs status --json = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "acceptedAt", "runId", "schemaVersion", "state", "supervisor", "terminal", "updatedAt", "workers", "workspace")
	if terminal, _ := document["terminal"].(bool); terminal {
		t.Fatal("terminal = true for a run still going")
	}
	if document["runId"] != runID {
		t.Fatalf("runId = %#v, want %q", document["runId"], runID)
	}
}

// TestRunsStatusOfAFinishedRunReportsItsResult requires that a status of a
// run that has finished reports what it became, what each worker answered,
// and the exit code that same result produces in the foreground.
func TestRunsStatusOfAFinishedRunReportsItsResult(t *testing.T) {
	manager, _ := setupBackgroundRun(t, backgroundHappyScript("finished answer"))
	runID := startBackgroundRun(t, manager, "--task", "go", "--timeout", "5m")
	final, err := manager.Wait(context.Background(), runID, 90*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !final.Terminal || final.State != background.RunCompleted {
		t.Fatalf("run = state %q terminal=%v, want a completed run to report on", final.State, final.Terminal)
	}

	code, stdout, stderr := runCLI(t, []string{"runs", "status", runID}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs status of a finished run = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	if !strings.Contains(stdout, string(background.RunCompleted)) || !strings.Contains(stdout, "outcome=completed") {
		t.Fatalf("stdout = %q, want the completed state and its outcome", stdout)
	}
	// What the worker answered is the point of the block for a run that is
	// done, and it is the run's own answer, not a placeholder.
	if !strings.Contains(stdout, "finished answer") {
		t.Fatalf("stdout = %q, want the worker's answer", stdout)
	}

	// A finished run is reported with the code the same result earns in the
	// foreground: the snapshot's own result document goes through the one
	// mapping the product already has.
	if want := foregroundCodeOfFinishedRun(final); want != code {
		t.Fatalf("runs status exit = %d, want the foreground code %d for that result", code, want)
	}

	// --json carries the whole result the run wrote, unchanged: the run
	// document and each worker's answer are in it.
	code, stdout, stderr = runCLI(t, []string{"runs", "status", runID, "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs status --json of a finished run = (%d, %q, %q)", code, stdout, stderr)
	}
	document := decodeJSONObject(t, stdout)
	if terminal, _ := document["terminal"].(bool); !terminal {
		t.Fatal("terminal = false for a finished run")
	}
	if state, _ := document["state"].(string); state != string(background.RunCompleted) {
		t.Fatalf("state = %#v, want %q", document["state"], background.RunCompleted)
	}
	result, ok := document["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want the run's own result document", document["result"])
	}
	if result["outcome"] != string(contracts.OutcomeCompleted) {
		t.Fatalf("result.outcome = %#v, want %q", result["outcome"], contracts.OutcomeCompleted)
	}
	// The stored result is the same document a foreground run prints, so
	// the fields that document promises are never absent are present here
	// too: a background run's supervisor measures what the run changed
	// exactly as the foreground path does.
	for _, field := range []string{"schemaVersion", "status", "outcome", "workers", "changes"} {
		if _, ok := result[field]; !ok {
			t.Fatalf("result is missing %q; keys = %v", field, sortedKeys(result))
		}
	}
	workers := requireJSONArray(t, result["workers"], "result.workers")
	worker := workers[0].(map[string]any)
	if worker["explanation"] != "finished answer" {
		t.Fatalf("worker explanation = %#v, want the answer the fake Pi gave", worker["explanation"])
	}
}

// foregroundCodeOfFinishedRun is the exit code a finished run's result
// produces in the foreground: the very mapping `pi-worker run` exits through,
// applied to the result document the run wrote into its snapshot.
func foregroundCodeOfFinishedRun(snap background.Snapshot) int {
	if snap.Result == nil {
		panic("a terminal snapshot always carries its run result")
	}
	_, code := runOutcome(*snap.Result)
	return code
}

// TestRunsWaitReturnsTheFinishedRunAndItsForegroundCode requires that a wait
// comes back with the run that finished, that it says what the workers
// answered, and that its exit code is the code the same result produces in
// the foreground — measured against a real foreground run of the same task
// and the same verification command, not against a hardcoded number.
func TestRunsWaitReturnsTheFinishedRunAndItsForegroundCode(t *testing.T) {
	manager, _ := setupBackgroundRun(t, backgroundHappyScript("waited answer"))
	verify := writeAlwaysFailingVerifyCommand(t)

	// The same run in the foreground: a verification that fails on a run
	// whose workers all completed is the contract's own exit 6, and that is
	// the code a background run of the same thing must report.
	installRealFakePiWorker(t)
	foregroundCode, foregroundStdout, foregroundStderr := runCLI(t, append([]string{"run", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, verify...), "")
	if !strings.Contains(foregroundStdout, "waited answer") && foregroundCode == 0 {
		t.Fatalf("foreground run = (%d, %q, %q), want the failing verification to be reported", foregroundCode, foregroundStdout, foregroundStderr)
	}

	runID := startBackgroundRun(t, manager, append([]string{"--task", "go", "--timeout", "5m"}, verify...)...)
	code, stdout, stderr := runCLI(t, []string{"runs", "wait", runID}, "")
	if code != foregroundCode {
		t.Fatalf("runs wait exit = %d, want the foreground code %d for the same result; stderr = %q", code, foregroundCode, stderr)
	}
	if code == 0 {
		t.Fatal("a run whose verification failed must not exit 0")
	}
	if !strings.Contains(stdout, string(background.RunCompleted)) || !strings.Contains(stdout, "outcome=") {
		t.Fatalf("stdout = %q, want the finished run's state and outcome", stdout)
	}
	if !strings.Contains(stdout, "waited answer") {
		t.Fatalf("stdout = %q, want the worker's answer", stdout)
	}

	// --json prints the run's own terminal document: the state, the
	// outcome, and the exit code all agree with the human block.
	code, stdout, stderr = runCLI(t, []string{"runs", "wait", runID, "--json"}, "")
	if code != foregroundCode || stderr != "" {
		t.Fatalf("runs wait --json = (%d, %q, %q), want the finished run's code %d with empty stderr", code, stdout, stderr, foregroundCode)
	}
	document := decodeJSONObject(t, stdout)
	if terminal, _ := document["terminal"].(bool); !terminal {
		t.Fatal("terminal = false for a run the wait reported as finished")
	}
	if _, ok := document["result"].(map[string]any); !ok {
		t.Fatalf("result = %#v, want the run's own result document", document["result"])
	}
}

// TestRunsWaitTimeoutReportsLatestStateAndLeavesTheRunGoing requires that a
// wait whose bound arrives first prints the latest state, says on stderr that
// the wait ran out, exits with the code the contract gives a timeout, and
// leaves the run completely alone: nothing cancelled, nothing killed, the run
// finishing by itself afterwards.
func TestRunsWaitTimeoutReportsLatestStateAndLeavesTheRunGoing(t *testing.T) {
	manager, _ := setupBackgroundRun(t, slowBackgroundScript("late answer", backgroundRunDelayStep))
	runID := startBackgroundRun(t, manager, "--task", "go", "--timeout", "5m")

	code, stdout, stderr := runCLI(t, []string{"runs", "wait", runID, "--timeout", "200ms"}, "")
	if code != contracts.ExitCode(contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout}) {
		t.Fatalf("runs wait --timeout = %d, want the contract's timeout code 7; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "ran out") {
		t.Fatalf("stderr = %q, want it to say the wait ran out", stderr)
	}
	if !strings.Contains(stderr, "still going") {
		t.Fatalf("stderr = %q, want it to say the run is still going", stderr)
	}
	if !strings.Contains(stdout, runID) {
		t.Fatalf("stdout = %q, want the latest state of the run that was waited for", stdout)
	}
	if strings.Contains(stdout, "outcome=") {
		t.Fatalf("stdout = %q, want no outcome claimed for a run that has not finished", stdout)
	}
	// The latest state, and not a failure invented for the wait: what the
	// run's own durable state says at that moment is what was printed.
	snap, err := manager.Status(runID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if snap.Terminal {
		t.Fatalf("the run finished inside its own prompt step; state = %q", snap.State)
	}
	if !strings.Contains(stdout, string(snap.State)) {
		t.Fatalf("stdout = %q, want the run's latest state %q", stdout, snap.State)
	}

	// The same on the machine side: the document is the latest state, and
	// it is a live run's state, not a failure arm.
	code, stdout, stderr = runCLI(t, []string{"runs", "wait", runID, "--timeout", "200ms", "--json"}, "")
	if code != 7 {
		t.Fatalf("runs wait --timeout --json = %d, want 7; stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	if terminal, _ := document["terminal"].(bool); terminal {
		t.Fatal("terminal = true for a run whose wait ran out")
	}
	if _, ok := document["result"]; ok {
		t.Fatalf("result = %#v, want no result for a run still going", document["result"])
	}
	// The note is a human line and belongs to stderr: stdout stays the
	// document and nothing else.
	if strings.Contains(stdout, "ran out") {
		t.Fatalf("stdout = %q, want the ran-out note off the document", stdout)
	}
	if !strings.Contains(stderr, "ran out") {
		t.Fatalf("stderr = %q, want the ran-out note", stderr)
	}

	// And the run the wait gave up on is untouched: it finishes, on its
	// own, with its answer intact.
	final, err := manager.Wait(context.Background(), runID, 90*time.Second)
	if err != nil {
		t.Fatalf("the run a wait ran out on did not finish on its own: %v", err)
	}
	if !final.Terminal || final.State != background.RunCompleted {
		t.Fatalf("run after the wait ran out = state %q terminal=%v, want it to have finished by itself", final.State, final.Terminal)
	}
	if len(final.Workers) != 1 || final.Workers[0].Result == nil || final.Workers[0].Result.Explanation != "late answer" {
		t.Fatalf("workers = %+v, want the untouched run's answer", final.Workers)
	}
	// A later status of that same run reports the finished result.
	code, stdout, stderr = runCLI(t, []string{"runs", "status", runID}, "")
	if code != 0 || !strings.Contains(stdout, "outcome=completed") || !strings.Contains(stdout, "late answer") {
		t.Fatalf("runs status after the run finished = (%d, %q, %q)", code, stdout, stderr)
	}
}

// TestRunsStatusAndWaitRefuseAnUnknownRunID requires that an identity no run
// is recorded under is a usage error for both commands, reported by name, and
// that neither invents a state for a run that does not exist.
func TestRunsStatusAndWaitRefuseAnUnknownRunID(t *testing.T) {
	_, _ = setupBackgroundRun(t, backgroundHappyScript("never started"))
	const unknown = "20260830T101500Z-4242"

	for _, args := range [][]string{
		{"runs", "status", unknown},
		{"runs", "status", unknown, "--json"},
		{"runs", "wait", unknown},
		{"runs", "wait", unknown, "--timeout", "100ms"},
		{"runs", "wait", unknown, "--json"},
		{"runs", "status", "not-a-run-id"},
		{"runs", "wait", "not-a-run-id"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 2 {
				t.Fatalf("%v = (%d, %q, %q), want exit 2", args, code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("%v printed %q, want nothing on stdout for a run that does not exist", args, stdout)
			}
			if !strings.Contains(stderr, "unknown run") {
				t.Fatalf("%v stderr = %q, want an unknown-run refusal", args, stderr)
			}
			if !strings.Contains(stderr, `"not-a-run-id"`) && !strings.Contains(stderr, `"`+unknown+`"`) {
				t.Fatalf("%v stderr = %q, want the refusal to name the identity it could not find", args, stderr)
			}
		})
	}
}

// TestRunsStatusAndWaitUsageErrors pins the argv surface of both commands: a
// missing identity, a second one, the flags that belong to another command,
// and a bad bound are all usage errors, and none of them reaches a run.
func TestRunsStatusAndWaitUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"runs", "status"},
		{"runs", "wait"},
		{"runs", "status", "a", "b"},
		{"runs", "wait", "a", "b"},
		{"runs", "status", "--keep", "1"},
		{"runs", "status", "--yes"},
		{"runs", "wait", "--keep", "1"},
		{"runs", "wait", "--yes"},
		{"runs", "list", "20260830T101500Z-4242"},
		{"runs", "prune", "--keep", "0", "20260830T101500Z-4242"},
		{"runs", "status", "20260830T101500Z-4242", "--timeout", "1s"},
		{"runs", "wait", "20260830T101500Z-4242", "--timeout"},
		{"runs", "wait", "20260830T101500Z-4242", "--timeout", "soon"},
		{"runs", "wait", "20260830T101500Z-4242", "--timeout", "0s"},
		{"runs", "wait", "20260830T101500Z-4242", "--timeout", "-1s"},
		{"runs", "wait", "20260830T101500Z-4242", "--timeout", "1s", "--timeout", "2s"},
		{"runs", "wait", "20260830T101500Z-4242", "--bogus"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
				t.Fatalf("%v = (%d, %q, %q), want exit 2 with the reason on stderr", args, code, stdout, stderr)
			}
		})
	}
}

// TestRunsStatusAndWaitDefaultTimeoutIsDocumented pins the bound a wait takes
// when it was given none, and that a status takes no bound at all: the number
// in the docs is the number in the parser.
func TestRunsStatusAndWaitDefaultTimeoutIsDocumented(t *testing.T) {
	opts, err := parseRunsArgs([]string{"wait", "20260830T101500Z-4242"})
	if err != nil {
		t.Fatalf("parse runs wait: %v", err)
	}
	if opts.timeout != defaultRunsWaitTimeout {
		t.Fatalf("runs wait default timeout = %v, want %v", opts.timeout, defaultRunsWaitTimeout)
	}
	// The number the docs promise is the number the parser carries: the
	// usage document states the bound a wait takes when it was given none.
	usage, err := os.ReadFile(filepath.Join("..", "..", "docs", "v0-usage.md"))
	if err != nil {
		t.Fatalf("read the usage document: %v", err)
	}
	if !strings.Contains(string(usage), "bound is `"+strings.TrimSuffix(defaultRunsWaitTimeout.String(), "0s")+"`") {
		t.Fatalf("docs/v0-usage.md does not state the default wait bound %v", defaultRunsWaitTimeout)
	}
	if _, err := parseRunsArgs([]string{"status", "20260830T101500Z-4242", "--timeout", "1m"}); err == nil {
		t.Fatal("runs status takes a --timeout; a status waits for nothing")
	}
	// A wait's bound binds to the wait even when the identity leads, and
	// both spellings are accepted.
	for _, args := range [][]string{{"wait", "--timeout", "1m", "x"}, {"wait", "x", "--timeout=1m"}} {
		opts, err := parseRunsArgs(args)
		if err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		if opts.timeout != time.Minute || opts.command != "wait" {
			t.Fatalf("parse %v = %+v, want a one-minute wait", args, opts)
		}
	}
}

// TestRunsStatusAndWaitRefusedWhereNoBackgroundRunCanExist requires that both
// commands are refused, not broken, where no background run can exist: no
// identity is asked about, no document is printed, and the reason is said. It
// goes through the seam the commands ask the platform question through, so it
// runs everywhere — including on the platforms that can host a run, which is
// where the refusal would otherwise never be reached — and the refusal it
// pins is the one the Windows build takes by its own build tag.
func TestRunsStatusAndWaitRefusedWhereNoBackgroundRunCanExist(t *testing.T) {
	original := backgroundSupportsRuns
	backgroundSupportsRuns = func() bool { return false }
	t.Cleanup(func() { backgroundSupportsRuns = original })

	for _, args := range [][]string{
		{"runs", "status", "20260830T101500Z-4242"},
		{"runs", "status", "20260830T101500Z-4242", "--json"},
		{"runs", "wait", "20260830T101500Z-4242"},
		{"runs", "wait", "20260830T101500Z-4242", "--timeout", "1s", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 9 || stdout != "" {
				t.Fatalf("%v = (%d, %q), want a refusal with no document on stdout", args, code, stdout)
			}
			if strings.Contains(stderr, "unknown run") {
				t.Fatalf("%v stderr = %q, want the platform refusal rather than a per-run answer", args, stderr)
			}
			if !strings.Contains(stderr, "background runs are not supported on this platform") {
				t.Fatalf("%v stderr = %q, want the platform refusal", args, stderr)
			}
		})
	}
}

// TestMainUsageIncludesBackgroundRunCommands asserts the usage line the binary
// prints names both new commands with their flags.
func TestMainUsageIncludesBackgroundRunCommands(t *testing.T) {
	code, stdout, stderr := runCLI(t, []string{}, "")
	if code != 2 || stdout != "" {
		t.Fatalf("empty argv = (%d, %q, %q)", code, stdout, stderr)
	}
	for _, want := range []string{
		"pi-worker runs status <id> [--json]",
		"pi-worker runs wait <id> [--timeout <duration>] [--json]",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("usage missing %q:\n%s", want, stderr)
		}
	}
}

// TestRunsStatusAndWaitAreWiredIntoMainWithContext asserts the second dispatch
// switch — mainWithContext, the seam the cancellation tests drive — routes both
// commands the same way Main does.
func TestRunsStatusAndWaitAreWiredIntoMainWithContext(t *testing.T) {
	original := backgroundSupportsRuns
	backgroundSupportsRuns = func() bool { return false }
	t.Cleanup(func() { backgroundSupportsRuns = original })

	for _, args := range [][]string{{"runs", "status", "20260830T101500Z-4242"}, {"runs", "wait", "20260830T101500Z-4242"}} {
		code, stdout, stderr := runCLIWithContext(t, context.Background(), args, "")
		if code != 9 || stdout != "" || !strings.Contains(stderr, "background runs are not supported") {
			t.Fatalf("mainWithContext %v = (%d, %q, %q), want the same refusal Main gives", args, code, stdout, stderr)
		}
	}
}

// TestRunsStatusJSONDocumentIsTheStoredSnapshot pins that the machine format
// of both commands is exactly the documented background document: the fields
// the store holds, with the run's own result inside them, and no second shape.
func TestRunsStatusJSONDocumentIsTheStoredSnapshot(t *testing.T) {
	manager, _ := setupBackgroundRun(t, backgroundHappyScript("stored answer"))
	runID := startBackgroundRun(t, manager, "--task", "go", "--timeout", "5m")
	final, err := manager.Wait(context.Background(), runID, 90*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	stored, err := json.Marshal(final)
	if err != nil {
		t.Fatalf("encode the stored snapshot: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{"runs", "status", runID, "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs status --json = (%d, %q, %q)", code, stdout, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("document spans more than one line: %q", stdout)
	}
	if strings.TrimSpace(stdout) != string(stored) {
		t.Fatalf("document = %q, want the stored snapshot verbatim %q", stdout, stored)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRunsWaitOnAnUnreadableRunReportsTheReadFailureNotAnEmptyDocument
// requires that a wait whose bound is already spent still reports a run it
// cannot read as unreadable. The ran-out arm exists to report a live run's
// latest state; a run whose stored state is gone has no state, and passing a
// zero snapshot off as one prints a run with an empty identity.
func TestRunsWaitOnAnUnreadableRunReportsTheReadFailureNotAnEmptyDocument(t *testing.T) {
	manager, root := setupBackgroundRun(t, backgroundHappyScript("state removed under the wait"))
	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--json", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("run --background = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	runID, ok := decodeJSONObject(t, stdout)["runId"].(string)
	if !ok || runID == "" {
		t.Fatalf("accepted run reported no identity: %q", stdout)
	}
	if _, err := manager.Wait(context.Background(), runID, 90*time.Second); err != nil {
		t.Fatalf("drain run %s: %v", runID, err)
	}
	if err := os.RemoveAll(filepath.Join(root, runID)); err != nil {
		t.Fatalf("remove stored state of run %s: %v", runID, err)
	}

	// The bound is already spent when the first read happens, which is the
	// only arrangement in which the wait has both a read failure and a
	// context error to choose between.
	for _, args := range [][]string{
		{"runs", "wait", runID, "--timeout", "1ns"},
		{"runs", "wait", runID, "--timeout", "1ns", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code == contracts.ExitCode(contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout}) {
				t.Fatalf("%v = (%d, %q, %q), want a read failure rather than the timeout code", args, code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("%v printed %q, want nothing on stdout for a run nobody can read", args, stdout)
			}
			if !strings.Contains(stderr, "unknown run") || !strings.Contains(stderr, `"`+runID+`"`) {
				t.Fatalf("%v stderr = %q, want a refusal naming the identity it could not read", args, stderr)
			}
		})
	}
}
