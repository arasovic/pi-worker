package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// listedRun is the subset of a runs list entry these tests assert on.
type listedRun struct {
	RunID   string `json:"runId"`
	Outcome string `json:"outcome"`
	Path    string `json:"path"`
}

// TestRunsListIncludesBackgroundRuns drives the real runs list against one
// finished background run and one hand-written foreground record and pins
// the merge: both identities appear, the background entry carries its own
// outcome and its run directory as its path, and the order is newest first. The human
// output names the background run too.
func TestRunsListIncludesBackgroundRuns(t *testing.T) {
	manager, root := setupBackgroundRun(t, backgroundHappyScript("list answer"))

	foregroundDir := t.TempDir()
	withRunlogDir(t, foregroundDir)
	withBackgroundRoot(t, root)

	backgroundRunID := startBackgroundRun(t, manager, "--task", "go", "--timeout", "5m")
	if _, err := manager.Wait(context.Background(), backgroundRunID, 90*time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// A foreground record dated well before any real clock run, so the
	// background run is unambiguously the newest entry.
	foregroundRunID := "20000101T000000Z-1"
	foregroundPath := writeListRecord(t, foregroundDir, foregroundRunID, deadPID, "2000-01-01T00:00:00Z", "/ws-fg", 1, true, "completed", "")

	code, stdout, stderr := runCLI(t, []string{"runs", "list", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs list --json = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	var document struct {
		SchemaVersion int         `json:"schemaVersion"`
		Runs          []listedRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode runs list document: %v\n%s", err, stdout)
	}
	var background, foreground *listedRun
	for i := range document.Runs {
		switch document.Runs[i].RunID {
		case backgroundRunID:
			background = &document.Runs[i]
		case foregroundRunID:
			foreground = &document.Runs[i]
		}
	}
	if background == nil {
		t.Fatalf("background run %s missing from runs list: %s", backgroundRunID, stdout)
	}
	if foreground == nil {
		t.Fatalf("foreground run %s missing from runs list: %s", foregroundRunID, stdout)
	}
	if background.Outcome != "completed" {
		t.Fatalf("background outcome = %q, want completed", background.Outcome)
	}
	if want := filepath.Join(root, backgroundRunID); background.Path != want {
		t.Fatalf("background path = %q, want its run directory %q", background.Path, want)
	}
	if foreground.Path != foregroundPath {
		t.Fatalf("foreground path = %q, want %q", foreground.Path, foregroundPath)
	}
	if document.Runs[0].RunID != backgroundRunID {
		t.Fatalf("runs[0] = %q, want the newest background run %q first", document.Runs[0].RunID, backgroundRunID)
	}

	code, stdout, stderr = runCLI(t, []string{"runs", "list"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs list = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	if !strings.Contains(stdout, backgroundRunID) {
		t.Fatalf("human runs list = %q, want it to name the background run %s", stdout, backgroundRunID)
	}
}

// recordLine is the subset of a run-record line these tests assert on.
type recordLine struct {
	Event      string          `json:"event"`
	RunID      string          `json:"runId"`
	PID        int             `json:"pid"`
	CreateTime int64           `json:"createTime"`
	WorkerID   int             `json:"workerId"`
	Result     json.RawMessage `json:"result"`
}

// waitForFinishedRecord polls the run record at path until its last line is
// the finish line and returns every line. The supervisor writes the finish
// line after the terminal snapshot, so a wait that saw the run terminal has
// not necessarily seen the record finished yet.
func waitForFinishedRecord(t *testing.T, path string) []recordLine {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var lines []recordLine
		if data, err := os.ReadFile(path); err == nil {
			for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var line recordLine
				if err := json.Unmarshal([]byte(raw), &line); err != nil {
					t.Fatalf("record line %q: %v", raw, err)
				}
				lines = append(lines, line)
			}
			if len(lines) > 0 && lines[len(lines)-1].Event == "finish" {
				return lines
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("record %s never finished; lines = %+v", path, lines)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunBackgroundWritesTheRunRecord requires that a background run writes
// the run record a foreground run writes, into the records directory the
// starting command resolved: the start line names the supervisor process the
// snapshot names, the worker line the process the snapshot names, and the
// finish line carries the snapshot's own result document. runs list shows
// the run once, as its background entry.
func TestRunBackgroundWritesTheRunRecord(t *testing.T) {
	manager, root := setupBackgroundRun(t, backgroundHappyScript("recorded answer"))
	recordsDir := t.TempDir()
	withRunlogDir(t, recordsDir)
	withBackgroundRoot(t, root)

	runID := startBackgroundRun(t, manager, "--task", "go", "--timeout", "5m")
	final, err := manager.Wait(context.Background(), runID, 90*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	lines := waitForFinishedRecord(t, filepath.Join(recordsDir, runID+".jsonl"))

	start := lines[0]
	if start.Event != "start" || start.RunID != runID {
		t.Fatalf("first line = %+v, want the start line of %s", start, runID)
	}
	if start.PID != final.Supervisor.PID || start.CreateTime != final.Supervisor.CreateTime {
		t.Fatalf("start line process = (%d, %d), want the supervisor (%d, %d)", start.PID, start.CreateTime, final.Supervisor.PID, final.Supervisor.CreateTime)
	}
	var worker *recordLine
	for i := range lines {
		if lines[i].Event == "worker" {
			worker = &lines[i]
		}
	}
	if worker == nil || final.Workers[0].Process == nil || worker.WorkerID != 1 || worker.PID != final.Workers[0].Process.PID {
		t.Fatalf("worker line = %+v, want worker 1 with the snapshot's process %+v", worker, final.Workers[0].Process)
	}
	want, err := json.Marshal(final.Result)
	if err != nil {
		t.Fatalf("encode snapshot result: %v", err)
	}
	if finish := lines[len(lines)-1]; string(finish.Result) != string(want) {
		t.Fatalf("finish line result = %s, want the snapshot's result %s", finish.Result, want)
	}

	code, stdout, stderr := runCLI(t, []string{"runs", "list", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs list --json = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	var document struct {
		Runs []listedRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode runs list document: %v\n%s", err, stdout)
	}
	if len(document.Runs) != 1 || document.Runs[0].RunID != runID || document.Runs[0].Path != filepath.Join(root, runID) {
		t.Fatalf("runs list = %+v, want the run once, as its background entry", document.Runs)
	}
}

// TestRunsPruneDeletesABackgroundRunsRecordButNotItsSnapshot requires that
// prune, which reads only the records directory, deletes the record a
// finished background run wrote there like any foreground record, and never
// its background snapshot. The background root is pointed at the run's real
// store, so a prune that wrongly reached into it would fail here.
func TestRunsPruneDeletesABackgroundRunsRecordButNotItsSnapshot(t *testing.T) {
	manager, root := setupBackgroundRun(t, backgroundHappyScript("prune answer"))

	foregroundDir := t.TempDir()
	withRunlogDir(t, foregroundDir)
	withBackgroundRoot(t, root)

	backgroundRunID := startBackgroundRun(t, manager, "--task", "go", "--timeout", "5m")
	if _, err := manager.Wait(context.Background(), backgroundRunID, 90*time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	snapshotPath := filepath.Join(root, backgroundRunID, "snapshot.json")
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatalf("background snapshot %s missing before prune: %v", snapshotPath, err)
	}
	recordPath := filepath.Join(foregroundDir, backgroundRunID+".jsonl")
	waitForFinishedRecord(t, recordPath)
	writeListRecord(t, foregroundDir, "20000101T000000Z-1", deadPID, "2000-01-01T00:00:00Z", "/ws-fg", 1, true, "completed", "")

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatalf("background snapshot %s vanished after prune: %v", snapshotPath, err)
	}
	var document struct {
		Deleted []string `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode runs prune document: %v\n%s", err, stdout)
	}
	if !slices.Contains(document.Deleted, backgroundRunID) {
		t.Fatalf("prune deleted %v, want the background run's record %s among them", document.Deleted, backgroundRunID)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("background run record %s after prune: %v, want it deleted", recordPath, err)
	}
}
