package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/runlog"
)

// runRecords returns every record in dir: a flat <id>.jsonl, or the
// record.jsonl inside a run directory <id>/. The interrupted-run scan keeps
// its one-shot marker (reported.json) in the same directory, so the marker
// is filtered out, exactly as the readers filter it.
func runRecords(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read record dir: %v", err)
	}
	var records []string
	for _, entry := range entries {
		switch {
		case entry.IsDir():
			if _, err := os.Stat(filepath.Join(dir, entry.Name(), "record.jsonl")); err == nil {
				records = append(records, filepath.Join(dir, entry.Name(), "record.jsonl"))
			}
		case strings.HasSuffix(entry.Name(), ".jsonl"):
			records = append(records, filepath.Join(dir, entry.Name()))
		}
	}
	return records
}

// TestRunLogRecordsSuccessfulRunEndToEnd drives a completed run whose
// supervisor writes the record into the run's own directory inside the
// records directory the command resolved, and asserts the on-disk record:
// exactly one, <id>/record.jsonl, whose last line carries the finish event
// and the same outcome the run reported. With --debug the debug log sits in
// the same directory.
func TestRunLogRecordsSuccessfulRunEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "write the answer", "--json", "--debug"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}

	runDir := filepath.Join(logDir, runIDFromRunLine(t, stderr))
	files := runRecords(t, logDir)
	if len(files) != 1 || files[0] != filepath.Join(runDir, "record.jsonl") {
		t.Fatalf("records = %v, want exactly %s", files, filepath.Join(runDir, "record.jsonl"))
	}
	if _, err := os.Stat(filepath.Join(runDir, "debug.log")); err != nil {
		t.Fatalf("debug log beside the record: %v", err)
	}
	// The finish line follows the terminal snapshot the run returned on.
	waitForFinishedRecord(t, files[0])
	content, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	finish := decodeJSONObject(t, lines[len(lines)-1])
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

// TestRunLogRecordsWorkerProcessEndToEnd drives a run and asserts the
// record carries exactly one worker line with the pid the run's own
// snapshot names for its worker: the whole chain from the CLI through the
// supervisor and the controller into the record.
func TestRunLogRecordsWorkerProcessEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	manager := useFakePi(t, backgroundHappyScript("done"))
	logDir, err := runlogDir()
	if err != nil {
		t.Fatalf("runlogDir: %v", err)
	}

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "write the answer", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	snap, err := manager.Status(runIDFromRunLine(t, stderr))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(snap.Workers) != 1 || snap.Workers[0].Process == nil {
		t.Fatalf("workers = %+v, want one worker with its process", snap.Workers)
	}
	wantPID := float64(snap.Workers[0].Process.PID)

	files := runRecords(t, logDir)
	if len(files) != 1 {
		t.Fatalf("records = %v, want exactly one", files)
	}
	// The finish line follows the terminal snapshot the run returned on.
	waitForFinishedRecord(t, files[0])
	content, err := os.ReadFile(files[0])
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
		if worker["pid"] != wantPID {
			t.Fatalf("worker line pid = %v, want the snapshot's %v", worker["pid"], wantPID)
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
// back to the production directory. Calling the seam twice proves each
// invocation produces a fresh directory — the isolation fix for
// run-ID collisions in the same process and second.
func TestRunlogDirStaysUnderSystemTemp(t *testing.T) {
	dir1, err := runlogDir()
	if err != nil {
		t.Fatalf("runlogDir (1st call): %v", err)
	}
	dir2, err := runlogDir()
	if err != nil {
		t.Fatalf("runlogDir (2nd call): %v", err)
	}
	temp := filepath.Clean(os.TempDir())
	assertUnderTemp := func(dir string) {
		t.Helper()
		if !filepath.IsAbs(dir) {
			t.Fatalf("runlogDir() = %q, want an absolute path under the system temporary directory %q", dir, os.TempDir())
		}
		if dir != temp && !strings.HasPrefix(dir, temp+string(filepath.Separator)) {
			t.Fatalf("runlogDir() = %q, want a path under the system temporary directory %q", dir, os.TempDir())
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("runlogDir() = %q: stat: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("runlogDir() = %q, want an existing directory", dir)
		}
	}
	assertUnderTemp(dir1)
	assertUnderTemp(dir2)
	if dir1 == dir2 {
		t.Fatalf("runlogDir() returned the same path %q on two calls; want distinct directories", dir1)
	}
}

// TestRunWithoutRecordsDirectoryStartsNothing drives a run with runlogDir
// failing and asserts it is refused before any worker starts, exit 9 with
// the reason: the records directory is where the run's own directory — its
// snapshot, the state the run is waited on through — is written.
func TestRunWithoutRecordsDirectoryStartsNothing(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	originalDir := runlogDir
	runlogDir = func() (string, error) { return "", errors.New("records disabled") }
	t.Cleanup(func() { runlogDir = originalDir })

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "still work"}, "")
	if code != 9 || stdout != "" || !strings.Contains(stderr, "pi-worker: determine records directory: records disabled") {
		t.Fatalf("run = (%d, %q, %q), want exit 9, no document, the reason", code, stdout, stderr)
	}
	if got := fakePiLog(t); got != "" {
		t.Fatalf("fake Pi log = %q, want no worker", got)
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
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
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
	if got := withoutRunLine(t, stderr); got != wantWarning {
		t.Fatalf("stderr = %q, want %q", got, wantWarning)
	}
	if _, err := os.Stat(filepath.Join(logDir, "reported.json")); err != nil {
		t.Fatalf("marker missing after the first run: %v", err)
	}

	code, _, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("second exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := withoutRunLine(t, stderr); got != "" {
		t.Fatalf("second run stderr = %q, want no warning", got)
	}
}

// TestRunSilentAboutAliveAndFinishedRecordsEndToEnd drives a run next
// to a still-running record — no finish line, pid of this very test
// process — and finished records, one of them an outcome cancelled
// run, and asserts nothing is warned: a missing finish line alone is
// not an interruption, and a finished run is never one, whatever its
// outcome.
func TestRunSilentAboutAliveAndFinishedRecordsEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
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
	if stderr := withoutRunLine(t, stderr); stderr != "" {
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
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
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
	if got := withoutRunLine(t, stderr); got != wantWarning {
		t.Fatalf("stderr = %q, want the single warning %q", got, wantWarning)
	}

	code, _, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("second exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := withoutRunLine(t, stderr); got != "" {
		t.Fatalf("second run stderr = %q, want no warning", got)
	}
}

// TestRunWarnsFiveInterruptedRunsPlusCountEndToEnd writes seven
// interrupted records and asserts the CLI prints five path lines and
// one summary line naming the remainder and the records directory.
func TestRunWarnsFiveInterruptedRunsPlusCountEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
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
	if stderr := withoutRunLine(t, stderr); stderr != want.String() {
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
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
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

// TestRunWarnsAboutLeftoverProcessesEndToEnd drives a run with the
// leftover-process scan scripted to one run carrying two pids and
// asserts the exact warning line appears on stderr — the full record
// path and the pids — while stdout carries only the run's own JSON
// document.
func TestRunWarnsAboutLeftoverProcessesEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })
	recordPath := filepath.Join(logDir, "20260830T101500Z-1.jsonl")
	originalLeftovers := runlogLeftovers
	runlogLeftovers = func(string) ([]runlog.Leftover, error) {
		return []runlog.Leftover{{RunID: "20260830T101500Z-1", Path: recordPath, PIDs: []int{4111, 4112}}}, nil
	}
	t.Cleanup(func() { runlogLeftovers = originalLeftovers })

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	want := "pi-worker: warning: an earlier run may have left processes running: " + recordPath + " (pids 4111, 4112)\n"
	if stderr := withoutRunLine(t, stderr); stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	// decodeJSONObject fails unless stdout is exactly one JSON document:
	// the warning must not have touched it.
	decodeJSONObject(t, stdout)
}

// TestRunWarnsFiveLeftoverRunsPlusCountEndToEnd drives a run with the
// leftover-process scan scripted to six runs and asserts the CLI prints
// five path lines and one summary line naming the remainder and the
// records directory.
func TestRunWarnsFiveLeftoverRunsPlusCountEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })
	leftovers := make([]runlog.Leftover, 0, 6)
	for i := 0; i < 6; i++ {
		leftovers = append(leftovers, runlog.Leftover{
			RunID: fmt.Sprintf("20260830T10%02d00Z-%d", i+10, i),
			Path:  filepath.Join(logDir, fmt.Sprintf("20260830T10%02d00Z-%d.jsonl", i+10, i)),
			PIDs:  []int{4100 + i},
		})
	}
	originalLeftovers := runlogLeftovers
	runlogLeftovers = func(string) ([]runlog.Leftover, error) { return leftovers, nil }
	t.Cleanup(func() { runlogLeftovers = originalLeftovers })

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	var want strings.Builder
	for _, leftover := range leftovers[:5] {
		fmt.Fprintf(&want, "pi-worker: warning: an earlier run may have left processes running: %s (pids %d)\n", leftover.Path, leftover.PIDs[0])
	}
	fmt.Fprintf(&want, "pi-worker: warning: 1 more runs may have left processes running in %s\n", logDir)
	if stderr := withoutRunLine(t, stderr); stderr != want.String() {
		t.Fatalf("stderr = %q, want %q", stderr, want.String())
	}
}

// TestRunWarnsCapsLeftoverPidsAtTenEndToEnd drives a run with the
// leftover-process scan scripted to one run carrying fourteen pids and
// asserts the warning line lists ten, comma-separated, and summarizes
// the rest inside the same parentheses, verbatim.
func TestRunWarnsCapsLeftoverPidsAtTenEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })
	recordPath := filepath.Join(logDir, "20260830T101500Z-1.jsonl")
	pids := make([]int, 14)
	for i := range pids {
		pids[i] = i + 1
	}
	originalLeftovers := runlogLeftovers
	runlogLeftovers = func(string) ([]runlog.Leftover, error) {
		return []runlog.Leftover{{RunID: "20260830T101500Z-1", Path: recordPath, PIDs: pids}}, nil
	}
	t.Cleanup(func() { runlogLeftovers = originalLeftovers })

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	want := "pi-worker: warning: an earlier run may have left processes running: " + recordPath + " (pids 1, 2, 3, 4, 5, 6, 7, 8, 9, 10 and 4 more)\n"
	if stderr := withoutRunLine(t, stderr); stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

// TestRunLeftoverCheckFailureWarnsAndContinues drives a run with the
// leftover-process scan scripted to fail and asserts the exit code is
// unchanged, the failure is one warning in the existing style, and the
// run still proceeds to start its record: a records problem never
// fails a run.
func TestRunLeftoverCheckFailureWarnsAndContinues(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })
	originalLeftovers := runlogLeftovers
	runlogLeftovers = func(string) ([]runlog.Leftover, error) { return nil, errors.New("process table unreadable") }
	t.Cleanup(func() { runlogLeftovers = originalLeftovers })

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr := withoutRunLine(t, stderr); stderr != "pi-worker: warning: leftover-process check unavailable: process table unreadable\n" {
		t.Fatalf("stderr = %q, want the check-unavailable warning", stderr)
	}
	if files := len(runRecords(t, logDir)); files != 1 {
		t.Fatalf("record files = %d, want exactly one: the run must still start its record", files)
	}
}

// TestRunSilentWithNoLeftoversEndToEnd drives a run with the
// leftover-process scan scripted to no runs at all and asserts nothing
// is printed: no leftovers is no warning.
func TestRunSilentWithNoLeftoversEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })
	originalLeftovers := runlogLeftovers
	runlogLeftovers = func(string) ([]runlog.Leftover, error) { return nil, nil }
	t.Cleanup(func() { runlogLeftovers = originalLeftovers })

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr := withoutRunLine(t, stderr); stderr != "" {
		t.Fatalf("stderr = %q, want no warning", stderr)
	}
}

// TestRunSilentAboutLeftoverWithNoPidsEndToEnd drives a run with the
// leftover-process scan scripted to a run carrying no pids — a shape
// the reader never returns, but a scripted fake might — and asserts
// nothing is printed: a run with no pids is skipped silently, never
// printed with an empty list.
func TestRunSilentAboutLeftoverWithNoPidsEndToEnd(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })
	originalLeftovers := runlogLeftovers
	runlogLeftovers = func(string) ([]runlog.Leftover, error) {
		return []runlog.Leftover{{RunID: "20260830T101500Z-1", Path: filepath.Join(logDir, "20260830T101500Z-1.jsonl")}}, nil
	}
	t.Cleanup(func() { runlogLeftovers = originalLeftovers })

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr := withoutRunLine(t, stderr); stderr != "" {
		t.Fatalf("stderr = %q, want no warning", stderr)
	}
}
