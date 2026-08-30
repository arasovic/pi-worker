package cli

import (
	"errors"
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
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
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
