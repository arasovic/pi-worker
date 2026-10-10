package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// inFlightRun is one `run` command driven from a goroutine, so a test can act
// on the run while the command still waits for it.
type inFlightRun struct {
	done   chan struct{}
	code   int
	stdout lockedBuffer
	stderr lockedBuffer
}

func startInFlightRun(ctx context.Context, args []string) *inFlightRun {
	r := &inFlightRun{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.code = mainWithContext(ctx, args, strings.NewReader(""), &r.stdout, &r.stderr)
	}()
	return r
}

// runID returns the identity the command's run line names, once it is out.
func (r *inFlightRun) runID(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if match := runLinePattern.FindStringSubmatch(r.stderr.String()); match != nil {
			return match[1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no run line within 60s; stderr = %q", r.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// finish waits for the command to return, failing the test after limit.
func (r *inFlightRun) finish(t *testing.T, limit time.Duration) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(limit):
		t.Fatalf("run did not return within %v; stderr = %q", limit, r.stderr.String())
	}
}

// readPIDFile polls for the pid file fakepi writes at startup.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatalf("pid file %s = %q: %v", path, data, err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %s never written", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForProcessGone fails the test unless pid is gone within limit.
func waitForProcessGone(t *testing.T, what string, pid int, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for runlog.ProcessAlive(pid, 0) {
		if time.Now().After(deadline) {
			t.Fatalf("%s %d still alive after %v", what, pid, limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunPrintsWhatRunsWaitPrintsForTheSameRun requires that `run` prints,
// on both streams and in its exit code, exactly what `runs wait` prints for
// the same finished run, apart from the one run line: both render the stored
// snapshot.
func TestRunPrintsWhatRunsWaitPrintsForTheSameRun(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra func(t *testing.T) []string
	}{
		{name: "completed", extra: func(*testing.T) []string { return nil }},
		{name: "verification failed", extra: writeAlwaysFailingVerifyCommand},
	} {
		t.Run(test.name, func(t *testing.T) {
			newGitWorkspace(t)
			useFakePi(t, backgroundHappyScript("done"))
			args := append([]string{"run", "--model", "acme/m-1", "--task", "go"}, test.extra(t)...)

			runCode, runStdout, runStderr := runCLI(t, args, "")
			runID := runIDFromRunLine(t, runStderr)
			waitCode, waitStdout, waitStderr := runCLI(t, []string{"runs", "wait", runID}, "")

			if runCode != waitCode {
				t.Fatalf("run exit = %d, runs wait exit = %d", runCode, waitCode)
			}
			if runStdout != waitStdout {
				t.Fatalf("run stdout = %q\nruns wait stdout = %q", runStdout, waitStdout)
			}
			if got := withoutRunLine(t, runStderr); got != waitStderr {
				t.Fatalf("run stderr without its run line = %q\nruns wait stderr = %q", got, waitStderr)
			}
		})
	}
}

// TestRunJSONIsTheStoredResult requires that `run --json` prints the result
// document the supervisor stored for the run, and that the run line names
// that run, exactly once.
func TestRunJSONIsTheStoredResult(t *testing.T) {
	newGitWorkspace(t)
	manager := useFakePi(t, backgroundHappyScript("done"))

	code, stdout, stderr := runCLI(t, []string{"run", "--json", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if withoutRunLine(t, stderr) != "" {
		t.Fatalf("stderr = %q, want only the run line", stderr)
	}
	runID := runIDFromRunLine(t, stderr)
	snap, err := manager.Status(runID)
	if err != nil {
		t.Fatalf("Status of the run the run line names: %v", err)
	}
	if snap.RunID != runID || !snap.Terminal || snap.Result == nil {
		t.Fatalf("stored run = %+v, want the terminal run %s with a result", snap, runID)
	}
	var want, discard bytes.Buffer
	if err := printRunDocument(*snap.Result, true, &want, &discard); err != nil {
		t.Fatalf("render stored result: %v", err)
	}
	if stdout != want.String() {
		t.Fatalf("stdout = %q\nwant the stored result %q", stdout, want.String())
	}
}

// TestRunBackgroundPrintsNoRunLine requires that the run line belongs to the
// waiting `run` only: `run --background` already reports the run it
// accepted.
func TestRunBackgroundPrintsNoRunLine(t *testing.T) {
	manager := setupBackgroundCLI(t, "done")
	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	runID := runIDFromHumanOutput(t, stdout)
	t.Cleanup(func() { _, _ = manager.Wait(context.Background(), runID, 90*time.Second) })
	if runLinePattern.MatchString(stderr) {
		t.Fatalf("stderr = %q, want no run line with --background", stderr)
	}
}

// TestRunDebugStreamsTheRunsDebugLog requires that `run --debug` streams the
// debug lines every worker of the run writes to stderr, labelled by worker,
// prints no debug-log path line, and keeps --json stdout one document.
func TestRunDebugStreamsTheRunsDebugLog(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))

	code, stdout, stderr := runCLI(t, []string{"run", "--json", "--debug", "--model", "acme/m-1", "--task", "a", "--task", "b", "--task", "c"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	for _, want := range []string{"worker=1 phase=starting", "worker=2 phase=starting", "worker=3 phase=starting", "worker=3 status=completed"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want a streamed %q line", stderr, want)
		}
	}
	if strings.Contains(stderr, "debug log") {
		t.Fatalf("stderr = %q, want no debug-log path line", stderr)
	}
	if strings.Contains(stdout, "[pi-worker +") || strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout = %q, want one JSON document and no debug line", stdout)
	}
	decodeJSONObject(t, stdout)
}

// TestRunVerifyPastTheTimeoutExitsSevenWithADocument requires that a
// verification command still running when the run's --timeout expires ends
// the run timed out, exit 7, with the stored result document printed.
func TestRunVerifyPastTheTimeoutExitsSevenWithADocument(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	verify := strings.Join(cliVerifyHelperArgs(t, "0", "0"), " ")
	t.Setenv("PI_WORKER_CLI_VERIFY_SLEEP_MS", "10000")

	code, stdout, stderr := runCLI(t, []string{"run", "--json", "--timeout", "3s", "--model", "acme/m-1", "--task", "go", "--verify", verify}, "")
	if code != 7 {
		t.Fatalf("exit = %d, want timed-out 7 (not verification 6); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "verification") {
		t.Fatalf("stderr = %q, want the verification error", stderr)
	}
	if outcome := decodeJSONObject(t, stdout)["outcome"]; outcome != string(contracts.OutcomeTimeout) {
		t.Fatalf("document outcome = %v, want %s; stdout = %q", outcome, contracts.OutcomeTimeout, stdout)
	}
}

// TestRunVerifyThatCannotStartExitsNineWithADocument requires that a
// verification command that cannot start fails the run as an internal
// error, exit 9, with the error on stderr and the stored document printed.
func TestRunVerifyThatCannotStartExitsNineWithADocument(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))

	code, stdout, stderr := runCLI(t, []string{"run", "--json", "--model", "acme/m-1", "--task", "go", "--verify", "pi-worker-no-such-command"}, "")
	if code != 9 {
		t.Fatalf("exit = %d, want 9 (not verification 6); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "verification") || !strings.Contains(stderr, "pi-worker-no-such-command") {
		t.Fatalf("stderr = %q, want the start failure", stderr)
	}
	if outcome := decodeJSONObject(t, stdout)["outcome"]; outcome != string(contracts.OutcomeInternalError) {
		t.Fatalf("document outcome = %v, want %s; stdout = %q", outcome, contracts.OutcomeInternalError, stdout)
	}
}

// TestRunCtrlCCancelsTheRunAndWaitsForIt requires that interrupting the
// waiting command after the worker is prompted cancels the run, waits until
// it is over, and exits 8 with the cancelled run printed — the document, or
// in human mode the change line and outcome=cancelled last: the run is
// stored terminal and cancelled, and its Pi process is gone.
func TestRunCtrlCCancelsTheRunAndWaitsForIt(t *testing.T) {
	for _, jsonMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("json=%v", jsonMode), func(t *testing.T) {
			newGitWorkspace(t)
			manager := useFakePi(t, heldHappyScript("done", 60_000))
			pidFile := filepath.Join(t.TempDir(), "pi.pid")
			t.Setenv("FAKEPI_PIDFILE", pidFile)
			args := []string{"run", "--model", "acme/m-1", "--task", "go"}
			if jsonMode {
				args = append(args, "--json")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := startInFlightRun(ctx, args)
			runID := r.runID(t)
			waitForRequestLog(t, os.Getenv("FAKEPI_LOG"), "prompt")
			piPID := readPIDFile(t, pidFile)
			cancel()
			r.finish(t, 30*time.Second)

			if r.code != 8 {
				t.Fatalf("exit = %d, want 8; stderr = %q", r.code, r.stderr.String())
			}
			stdout := r.stdout.String()
			if jsonMode {
				if outcome := decodeJSONObject(t, stdout)["outcome"]; outcome != string(contracts.OutcomeCancelled) {
					t.Fatalf("document outcome = %v, want cancelled; stdout = %q", outcome, stdout)
				}
			} else if lines := strings.Split(strings.TrimSpace(stdout), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "changes: ") || lines[1] != "outcome=cancelled" {
				t.Fatalf("human stdout = %q, want one changes: line then exactly outcome=cancelled", stdout)
			}
			snap, err := manager.Status(runID)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !snap.Terminal || snap.Outcome == nil || *snap.Outcome != contracts.OutcomeCancelled {
				t.Fatalf("stored run terminal=%v outcome=%v, want terminal cancelled", snap.Terminal, snap.Outcome)
			}
			waitForProcessGone(t, "fake Pi", piPID, 5*time.Second)
		})
	}
}

// TestRunCtrlCDuringVerificationExitsEightWithADocument requires that an
// interrupt while the verification command runs cancels the run and prints
// the cancelled document.
func TestRunCtrlCDuringVerificationExitsEightWithADocument(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	verify := strings.Join(cliVerifyHelperArgs(t, "0", "0"), " ")
	t.Setenv("PI_WORKER_CLI_VERIFY_SLEEP_MS", "30000")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := startInFlightRun(ctx, []string{"run", "--json", "--model", "acme/m-1", "--task", "go", "--verify", verify})
	r.runID(t)
	waitForRequestLog(t, os.Getenv("FAKEPI_LOG"), "get_last_assistant_text")
	time.Sleep(time.Second)
	cancel()
	r.finish(t, 30*time.Second)

	if r.code != 8 {
		t.Fatalf("exit = %d, want 8 (not verification 6); stderr = %q", r.code, r.stderr.String())
	}
	if outcome := decodeJSONObject(t, r.stdout.String())["outcome"]; outcome != string(contracts.OutcomeCancelled) {
		t.Fatalf("document outcome = %v, want cancelled; stdout = %q", outcome, r.stdout.String())
	}
}

// TestRunCancelledBeforeAcceptanceExitsEightWithoutADocument requires that
// an interrupt before the supervisor accepts the run exits 8 with nothing on
// stdout, no run line, and no worker started.
func TestRunCancelledBeforeAcceptanceExitsEightWithoutADocument(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, stdout, stderr := runCLIWithContext(t, ctx, []string{"run", "--json", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 8 {
		t.Fatalf("exit = %d, want 8; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no document", stdout)
	}
	if runLinePattern.MatchString(stderr) {
		t.Fatalf("stderr = %q, want no run line for a run never accepted", stderr)
	}
	if got := fakePiLog(t); got != "" {
		t.Fatalf("fake Pi log = %q, want no worker", got)
	}
}

// TestRunRefusedWhereNoSupervisedRunCanExist requires that `run` on a
// platform without supervised runs exits 9 with its own message and starts
// nothing.
func TestRunRefusedWhereNoSupervisedRunCanExist(t *testing.T) {
	original := backgroundSupportsRuns
	backgroundSupportsRuns = func() bool { return false }
	t.Cleanup(func() { backgroundSupportsRuns = original })
	managerBuilt := false
	originalManager := newBackgroundManager
	newBackgroundManager = func(string, int) (*background.Manager, error) {
		managerBuilt = true
		return nil, errors.New("unexpected")
	}
	t.Cleanup(func() { newBackgroundManager = originalManager })

	code, stdout, stderr := runCLI(t, []string{"run", "--json", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 9 || stdout != "" || stderr != "pi-worker: run is not supported on this platform\n" {
		t.Fatalf("run = (%d, %q, %q), want exit 9 with only the platform message", code, stdout, stderr)
	}
	if managerBuilt {
		t.Fatal("the refused run reached the background manager")
	}
}

// TestRunGivesTheSupervisorTheResolvedAdmission requires that the admission
// root and model-worker limit `run` resolves from its configuration reach the
// manager that starts the run, and that a manager that cannot be built fails
// the run with exit 9 before any worker starts.
func TestRunGivesTheSupervisorTheResolvedAdmission(t *testing.T) {
	t.Run("passes root and limit", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := config.Save(configPath, config.Config{SchemaVersion: 2, MaxModelWorkers: 1}); err != nil {
			t.Fatal(err)
		}
		installConfigPath(t, configPath)
		newGitWorkspace(t)
		useFakePi(t, backgroundHappyScript("done"))
		var gotRoot string
		gotMax := -1
		wrapped := newBackgroundManager
		newBackgroundManager = func(admissionRoot string, maxModelWorkers int) (*background.Manager, error) {
			gotRoot, gotMax = admissionRoot, maxModelWorkers
			return wrapped(admissionRoot, maxModelWorkers)
		}
		t.Cleanup(func() { newBackgroundManager = wrapped })

		code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
		}
		if want := filepath.Join(filepath.Dir(configPath), "admission"); gotRoot != want || gotMax != 1 {
			t.Fatalf("manager built with (%q, %d), want (%q, 1)", gotRoot, gotMax, want)
		}
	})

	t.Run("manager failure starts nothing", func(t *testing.T) {
		newGitWorkspace(t)
		useFakePi(t, backgroundHappyScript("done"))
		wrapped := newBackgroundManager
		newBackgroundManager = func(string, int) (*background.Manager, error) {
			return nil, errors.New("admission unavailable")
		}
		t.Cleanup(func() { newBackgroundManager = wrapped })

		code, stdout, stderr := runCLI(t, []string{"run", "--json", "--model", "acme/m-1", "--task", "go"}, "")
		if code != 9 || stdout != "" || !strings.Contains(stderr, "pi-worker: admission unavailable") {
			t.Fatalf("run = (%d, %q, %q), want exit 9, no document, the error", code, stdout, stderr)
		}
		if got := fakePiLog(t); got != "" {
			t.Fatalf("fake Pi log = %q, want no worker", got)
		}
	})
}

// TestLargeValidResultsEndReadable is the regression for issue #510: three
// workers each answering 6 MiB of text encode a snapshot above the 32 MiB
// read ceiling. Instead of leaving an oversized snapshot the reader cannot
// load, the run ends internal-error with exit 9, and the stored snapshot
// stays readable with only the worker answer texts dropped. A one-worker
// control with the same answer stays under the ceiling and keeps its answer.
func TestLargeValidResultsEndReadable(t *testing.T) {
	const answerBytes = 6 << 20
	answer := strings.Repeat("x", answerBytes)

	t.Run("three workers exceed the ceiling", func(t *testing.T) {
		newGitWorkspace(t)
		useFakePi(t, backgroundHappyScript(answer))

		code, stdout, stderr := runCLI(t, []string{
			"run", "--json", "--model", "acme/m-1",
			"--task", "a", "--task", "b", "--task", "c",
		}, "")
		runID := runIDFromRunLine(t, stderr)

		if code != 9 {
			t.Fatalf("exit = %d, want 9; stderr = %q", code, stderr)
		}
		document := decodeJSONObject(t, stdout)
		if got := document["outcome"]; got != string(contracts.OutcomeInternalError) {
			t.Fatalf("document outcome = %v, want %s; stdout = %q", got, contracts.OutcomeInternalError, stdout)
		}
		if got := document["status"]; got != string(contracts.RunFailed) {
			t.Fatalf("document status = %v, want %s", got, contracts.RunFailed)
		}
		workers := requireJSONArray(t, document["workers"], "workers")
		if len(workers) != 3 {
			t.Fatalf("workers = %d, want 3", len(workers))
		}
		for i, raw := range workers {
			worker, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("worker %d = %#v, want an object", i+1, raw)
			}
			if worker["status"] != "completed" {
				t.Fatalf("worker %d status = %v, want completed", i+1, worker["status"])
			}
			if _, present := worker["explanation"]; present {
				t.Fatalf("worker %d carries an explanation, want it dropped", i+1)
			}
		}
		if !strings.Contains(stderr, "result too large to store") {
			t.Fatalf("stderr = %q, want the size reason", stderr)
		}

		root, err := runlogDir()
		if err != nil {
			t.Fatalf("runlogDir: %v", err)
		}
		snapPath := filepath.Join(root, runID, "snapshot.json")
		info, err := os.Stat(snapPath)
		if err != nil {
			t.Fatalf("stat snapshot: %v", err)
		}
		if info.Size() > 32<<20 {
			t.Fatalf("snapshot is %d bytes, want at most 32 MiB", info.Size())
		}

		statusCode, statusStdout, statusStderr := runCLI(t, []string{"runs", "status", runID, "--json"}, "")
		if statusCode != 9 {
			t.Fatalf("runs status exit = %d, want 9; stderr = %q", statusCode, statusStderr)
		}
		if outcome := decodeJSONObject(t, statusStdout)["outcome"]; outcome != string(contracts.OutcomeInternalError) {
			t.Fatalf("stored outcome = %v, want %s", outcome, contracts.OutcomeInternalError)
		}
	})

	t.Run("an earlier failure keeps its outcome", func(t *testing.T) {
		newGitWorkspace(t)
		useFakePi(t, backgroundHappyScript(answer))
		verify := strings.Join(cliVerifyHelperArgs(t, "3", "2"), " ")

		code, stdout, stderr := runCLI(t, []string{
			"run", "--json", "--model", "acme/m-1",
			"--task", "a", "--task", "b", "--task", "c",
			"--verify", verify,
		}, "")
		runID := runIDFromRunLine(t, stderr)

		if code != 6 {
			t.Fatalf("exit = %d, want 6 (verification-failed, not internal-error 9); stderr = %q", code, stderr)
		}
		document := decodeJSONObject(t, stdout)
		if got := document["outcome"]; got != string(contracts.OutcomeVerificationFailed) {
			t.Fatalf("document outcome = %v, want %s; stdout = %q", got, contracts.OutcomeVerificationFailed, stdout)
		}
		workers := requireJSONArray(t, document["workers"], "workers")
		if len(workers) != 3 {
			t.Fatalf("workers = %d, want 3", len(workers))
		}
		for i, raw := range workers {
			worker, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("worker %d = %#v, want an object", i+1, raw)
			}
			if _, present := worker["explanation"]; present {
				t.Fatalf("worker %d carries an explanation, want it dropped", i+1)
			}
		}
		if !strings.Contains(stderr, "result too large to store") {
			t.Fatalf("stderr = %q, want the size reason", stderr)
		}

		root, err := runlogDir()
		if err != nil {
			t.Fatalf("runlogDir: %v", err)
		}
		snapPath := filepath.Join(root, runID, "snapshot.json")
		info, err := os.Stat(snapPath)
		if err != nil {
			t.Fatalf("stat snapshot: %v", err)
		}
		if info.Size() > 32<<20 {
			t.Fatalf("snapshot is %d bytes, want at most 32 MiB", info.Size())
		}

		statusCode, statusStdout, statusStderr := runCLI(t, []string{"runs", "status", runID, "--json"}, "")
		if statusCode != 6 {
			t.Fatalf("runs status exit = %d, want 6; stderr = %q", statusCode, statusStderr)
		}
		if outcome := decodeJSONObject(t, statusStdout)["outcome"]; outcome != string(contracts.OutcomeVerificationFailed) {
			t.Fatalf("stored outcome = %v, want %s", outcome, contracts.OutcomeVerificationFailed)
		}
	})

	t.Run("one worker under the ceiling keeps its answer", func(t *testing.T) {
		newGitWorkspace(t)
		useFakePi(t, backgroundHappyScript(answer))

		code, stdout, stderr := runCLI(t, []string{
			"run", "--json", "--model", "acme/m-1", "--task", "a",
		}, "")
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
		}
		document := decodeJSONObject(t, stdout)
		workers := requireJSONArray(t, document["workers"], "workers")
		if len(workers) != 1 {
			t.Fatalf("workers = %d, want 1", len(workers))
		}
		worker, ok := workers[0].(map[string]any)
		if !ok {
			t.Fatalf("worker = %#v, want an object", workers[0])
		}
		explanation, ok := worker["explanation"].(string)
		if !ok || len(explanation) != answerBytes {
			t.Fatalf("explanation length = %d, want %d", len(explanation), answerBytes)
		}
	})
}
