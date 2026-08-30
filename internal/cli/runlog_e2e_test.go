package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/pi"
)

// TestRunLogRecordsSuccessfulRunEndToEnd drives a completed run with the
// real runlog.Start and runlog.Finish running inside runCommand and
// asserts the on-disk record: exactly one file holding exactly two
// lines, the second carrying the finish event and the same outcome the
// run reported. This is the only test that proves Finish is wired into
// runCommand at all.
func TestRunLogRecordsSuccessfulRunEndToEnd(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "write the answer", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("read record dir: %v", err)
	}
	// Only *.jsonl files are records. The interrupted-run scan keeps
	// its one-shot marker (reported.json) in the same directory, so a
	// record count must filter it out, exactly as the reader does.
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, entry.Name())
		}
	}
	if len(files) != 1 {
		t.Fatalf("record files = %v, want exactly one", files)
	}
	content, err := os.ReadFile(filepath.Join(logDir, files[0]))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("record lines = %d, want 2", len(lines))
	}
	finish := decodeJSONObject(t, lines[1])
	if finish["event"] != "finish" {
		t.Fatalf("second line event = %v, want finish", finish["event"])
	}
	result, ok := finish["result"].(map[string]any)
	if !ok {
		t.Fatalf("finish result = %#v, want object", finish["result"])
	}
	runDocument := decodeJSONObject(t, stdout)
	if result["outcome"] != runDocument["outcome"] {
		t.Fatalf("recorded outcome = %v, run outcome = %v", result["outcome"], runDocument["outcome"])
	}
	if _, present := finish["error"]; present {
		t.Fatalf("finish line carries an error on a successful run: %#v", finish)
	}
}

// TestRunLogRecordsWorkerProcessEndToEnd drives a run whose fake worker
// reports a process identity through OnProcessStart and asserts the
// record carries exactly one worker line with the exact pid the test
// passed in. This is the only test that proves the whole chain from the
// CLI through the controller into the record.
func TestRunLogRecordsWorkerProcessEndToEnd(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	fake.onRequest = func(req pi.WorkerRequest) {
		if req.OnProcessStart != nil {
			req.OnProcessStart(req.WorkerID, 4242)
		}
	}
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "write the answer", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("read record dir: %v", err)
	}
	// Only *.jsonl files are records; the interrupted-run scan's
	// one-shot marker (reported.json) shares the directory and must be
	// filtered out, exactly as the reader filters it.
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, entry.Name())
		}
	}
	if len(files) != 1 {
		t.Fatalf("record files = %v, want exactly one", files)
	}
	content, err := os.ReadFile(filepath.Join(logDir, files[0]))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	workerLines := 0
	for i, line := range lines {
		worker := decodeJSONObject(t, line)
		if worker["event"] != "worker" {
			continue
		}
		workerLines++
		if workerLines > 1 {
			t.Fatalf("line %d: more than one worker line", i)
		}
		if worker["workerId"] != float64(1) {
			t.Fatalf("worker line workerId = %v, want 1", worker["workerId"])
		}
		if worker["pid"] != float64(4242) {
			t.Fatalf("worker line pid = %v, want 4242", worker["pid"])
		}
	}
	if workerLines != 1 {
		t.Fatalf("worker lines = %d, want exactly 1", workerLines)
	}
}

// TestRunlogDirStaysUnderSystemTemp asserts that this package's tests
// never write run records into the user's real records directory:
// TestMain redirects the runlogDir seam to a temporary directory for
// the whole package test run. The expectation is derived from
// os.TempDir(), not from a copy of whatever path TestMain built, so
// this test fails if that redirect is ever removed and the seam falls
// back to the production directory.
func TestRunlogDirStaysUnderSystemTemp(t *testing.T) {
	dir, err := runlogDir()
	if err != nil {
		t.Fatalf("runlogDir: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("runlogDir() = %q, want an absolute path under the system temporary directory %q", dir, os.TempDir())
	}
	temp := filepath.Clean(os.TempDir())
	if dir != temp && !strings.HasPrefix(dir, temp+string(filepath.Separator)) {
		t.Fatalf("runlogDir() = %q, want a path under the system temporary directory %q", dir, os.TempDir())
	}
}

// TestRunLogUnavailableDoesNotFailRun drives a run with runlogDir
// failing and asserts the run still completes with its normal exit code
// and the record-unavailable warning appears on stderr: a record that
// cannot be written must never fail or block a run.
func TestRunLogUnavailableDoesNotFailRun(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	originalDir := runlogDir
	runlogDir = func() (string, error) { return "", errors.New("records disabled") }
	t.Cleanup(func() { runlogDir = originalDir })

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "still work"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "pi-worker: warning: run record unavailable: records disabled") {
		t.Fatalf("stderr = %q, want the record-unavailable warning", stderr)
	}
}

// deadPID is a pid that cannot exist on this machine: it sits far above
// every platform's pid ceiling, so the process table can never contain
// it, and kill(pid, 0) finds no process. The end-to-end tests rely on
// real process liveness — the runlog package's own tests script
// liveness through their seam instead — so the records here carry pids
// the test chose, never one read out of a record.
const deadPID = 1 << 30

// writeRecordFile writes one hand-built record file into dir for the
// interrupted-run tests: a start line carrying the pid the test chose
// and, when finishOutcome is non-empty, a finish line whose result
// carries that outcome. The reader's only interest is the two event
// fields and the pid, so the rest of each line is minimal.
func writeRecordFile(t *testing.T, dir, runID string, pid int, finishOutcome string) string {
	t.Helper()
	lines := []map[string]any{
		{
			"schemaVersion": 1,
			"event":         "start",
			"runId":         runID,
			"startedAt":     "2026-08-30T10:15:00Z",
			"workspace":     "/workspace",
			"pid":           pid,
			"tasks":         []any{},
		},
	}
	if finishOutcome != "" {
		lines = append(lines, map[string]any{
			"schemaVersion": 1,
			"event":         "finish",
			"runId":         runID,
			"finishedAt":    "2026-08-30T10:15:30Z",
			"result":        map[string]any{"schemaVersion": 1, "outcome": finishOutcome},
		})
	}
	var record strings.Builder
	for _, line := range lines {
		data, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("marshal record line: %v", err)
		}
		record.Write(data)
		record.WriteByte('\n')
	}
	path := filepath.Join(dir, runID+".jsonl")
	if err := os.WriteFile(path, []byte(record.String()), 0o600); err != nil {
		t.Fatalf("write record file: %v", err)
	}
	return path
}

// TestRunWarnsAboutInterruptedRunOnceEndToEnd writes one record whose
// run was interrupted — no finish line, pid long gone — drives two
// consecutive runs, and asserts the warning appears once, naming the
// full record path, and never again. The marker file the reader keeps
// in the records directory is what makes the second run silent; this
// is the only test that proves the whole chain from runCommand through
// runlog.Interrupted into the on-disk marker.
func TestRunWarnsAboutInterruptedRunOnceEndToEnd(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })

	recordPath := writeRecordFile(t, logDir, "20260830T101500Z-1", deadPID, "")

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	wantWarning := "pi-worker: warning: an earlier run was interrupted: " + recordPath + "\n"
	if stderr != wantWarning {
		t.Fatalf("stderr = %q, want %q", stderr, wantWarning)
	}
	if _, err := os.Stat(filepath.Join(logDir, "reported.json")); err != nil {
		t.Fatalf("marker missing after the first run: %v", err)
	}

	code, _, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("second exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("second run stderr = %q, want no warning", stderr)
	}
}

// TestRunSilentAboutAliveAndFinishedRecordsEndToEnd drives a run next
// to a still-running record — no finish line, pid of this very test
// process — and finished records, one of them an outcome cancelled
// run, and asserts nothing is warned: a missing finish line alone is
// not an interruption, and a finished run is never one, whatever its
// outcome.
func TestRunSilentAboutAliveAndFinishedRecordsEndToEnd(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })

	writeRecordFile(t, logDir, "20260830T101500Z-1", os.Getpid(), "")
	writeRecordFile(t, logDir, "20260830T102000Z-2", deadPID, "cancelled")
	writeRecordFile(t, logDir, "20260830T103000Z-3", deadPID, "completed")

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want no warning", stderr)
	}
}

// TestRunWarnsOnceForInterruptedRunAfterStillRunningEndToEnd places an
// interrupted record after a still-running one — the watermark stops
// at the live run, so the interrupted one is remembered in the
// marker's reported list instead of under the watermark — and asserts
// the warning appears exactly once across two runs, carrying the full
// record path.
func TestRunWarnsOnceForInterruptedRunAfterStillRunningEndToEnd(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })

	writeRecordFile(t, logDir, "20260830T101500Z-1", os.Getpid(), "")
	recordPath := writeRecordFile(t, logDir, "20260830T103000Z-2", deadPID, "")

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	wantWarning := "pi-worker: warning: an earlier run was interrupted: " + recordPath + "\n"
	if stderr != wantWarning {
		t.Fatalf("stderr = %q, want the single warning %q", stderr, wantWarning)
	}

	code, _, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("second exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("second run stderr = %q, want no warning", stderr)
	}
}

// TestRunWarnsFiveInterruptedRunsPlusCountEndToEnd writes seven
// interrupted records and asserts the CLI prints five path lines and
// one summary line naming the remainder and the records directory.
func TestRunWarnsFiveInterruptedRunsPlusCountEndToEnd(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })

	paths := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		paths = append(paths, writeRecordFile(t, logDir, fmt.Sprintf("20260830T10%02d00Z-%d", i+10, i), deadPID, ""))
	}

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	var want strings.Builder
	for _, path := range paths[:5] {
		fmt.Fprintf(&want, "pi-worker: warning: an earlier run was interrupted: %s\n", path)
	}
	fmt.Fprintf(&want, "pi-worker: warning: 2 more interrupted runs in %s\n", logDir)
	if stderr != want.String() {
		t.Fatalf("stderr = %q, want %q", stderr, want.String())
	}
}

// TestRunInterruptedCheckFailureWarnsAndContinues drives a run with
// the interrupted-run scan scripted to fail — once with no paths, as
// an unreadable records directory reports, and once with the paths it
// still found, as an unwritable marker reports — and asserts the exit
// code is unchanged and each failure is one warning in the existing
// style: a records problem never fails a run.
func TestRunInterruptedCheckFailureWarnsAndContinues(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})
	for _, test := range []struct {
		name  string
		err   error
		paths []string
	}{
		{name: "records directory unreadable", err: errors.New("records directory unreadable")},
		{name: "marker unwritable", err: errors.New("marker unwritable"), paths: []string{"/tmp/interrupted.jsonl"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			originalInterrupted := runlogInterrupted
			runlogInterrupted = func(string) ([]string, error) { return test.paths, test.err }
			t.Cleanup(func() { runlogInterrupted = originalInterrupted })

			code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
			}
			if !strings.Contains(stderr, "pi-worker: warning: interrupted-run check unavailable: "+test.err.Error()) {
				t.Fatalf("stderr = %q, want the check-unavailable warning", stderr)
			}
			for _, path := range test.paths {
				if !strings.Contains(stderr, "pi-worker: warning: an earlier run was interrupted: "+path) {
					t.Fatalf("stderr = %q, want the interrupted-run warning for %s", stderr, path)
				}
			}
		})
	}
}
