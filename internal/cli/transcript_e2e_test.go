package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// workerTranscript returns worker 1's transcript field from a run document,
// and whether the field is present.
func workerTranscript(t *testing.T, document map[string]any) (any, bool) {
	t.Helper()
	workers := requireJSONArray(t, document["workers"], "workers")
	transcript, ok := workers[0].(map[string]any)["transcript"]
	return transcript, ok
}

// requireKeptTranscript checks that worker 1's Pi session was kept in the
// run's private worker-1 directory and that the document names it.
func requireKeptTranscript(t *testing.T, document map[string]any, runDir string) {
	t.Helper()
	dir := filepath.Join(runDir, "worker-1")
	want := filepath.Join(dir, "fakepi-session.jsonl")
	if got, _ := workerTranscript(t, document); got != want {
		t.Fatalf("worker transcript = %v, want %s", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("transcript file: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("transcript directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("transcript directory mode = %v, want 0700", perm)
	}
}

// requireNoTranscript checks that a --no-transcript run kept no worker
// directory and reports no transcript.
func requireNoTranscript(t *testing.T, document map[string]any, runDir string) {
	t.Helper()
	if got, present := workerTranscript(t, document); present {
		t.Fatalf("worker transcript = %v, want no field", got)
	}
	if _, err := os.Lstat(filepath.Join(runDir, "worker-1")); !os.IsNotExist(err) {
		t.Fatalf("worker-1 directory of a --no-transcript run: %v, want none", err)
	}
}

// TestRunKeepsTheWorkerTranscript requires that a plain run keeps each
// worker's Pi session in its run directory and reports it as transcript.
func TestRunKeepsTheWorkerTranscript(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	root, err := runlogDir()
	if err != nil {
		t.Fatalf("runlogDir: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 0 {
		t.Fatalf("run = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	requireKeptTranscript(t, decodeJSONObject(t, stdout), filepath.Join(root, runIDFromRunLine(t, stderr)))
}

// TestRunNoTranscriptKeepsNothing requires that --no-transcript keeps the
// session in a temporary directory: no worker directory, no field.
func TestRunNoTranscriptKeepsNothing(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	root, err := runlogDir()
	if err != nil {
		t.Fatalf("runlogDir: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json", "--no-transcript"}, "")
	if code != 0 {
		t.Fatalf("run --no-transcript = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	requireNoTranscript(t, decodeJSONObject(t, stdout), filepath.Join(root, runIDFromRunLine(t, stderr)))
}

// TestBackgroundRunTranscriptIsReportedByRunsWait requires that a
// background run keeps the transcript the same way, that runs wait reports
// it, and that --no-transcript travels with run --background too.
func TestBackgroundRunTranscriptIsReportedByRunsWait(t *testing.T) {
	_, root := setupBackgroundRun(t, backgroundHappyScript("done"))
	for _, tc := range []struct {
		name  string
		extra []string
		check func(*testing.T, map[string]any, string)
	}{
		{"kept", nil, requireKeptTranscript},
		{"off", []string{"--no-transcript"}, requireNoTranscript},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"run", "--background", "--json", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, tc.extra...)
			code, stdout, stderr := runCLI(t, args, "")
			if code != 0 {
				t.Fatalf("run --background = (%d, %q, %q), want 0", code, stdout, stderr)
			}
			runID, _ := decodeJSONObject(t, stdout)["runId"].(string)
			code, stdout, stderr = runCLI(t, []string{"runs", "wait", runID, "--json", "--timeout", "90s"}, "")
			if code != 0 {
				t.Fatalf("runs wait = (%d, %q, %q), want 0", code, stdout, stderr)
			}
			// The wait document carries the run document under result.
			result, ok := decodeJSONObject(t, stdout)["result"].(map[string]any)
			if !ok {
				t.Fatalf("runs wait document = %s, want a result object", stdout)
			}
			tc.check(t, result, filepath.Join(root, runID))
		})
	}
}
