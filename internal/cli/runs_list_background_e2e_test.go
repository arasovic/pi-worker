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

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/run"
)

// TestRunsListIncludesRunDirectories requires that runs list reads the
// run directories <id>/ inside the records directory next to the flat
// records, and that a run directory wins over a flat record with the
// same id: the run is listed once, from its directory.
func TestRunsListIncludesRunDirectories(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	acceptedAt := time.Date(2026, 8, 30, 10, 20, 0, 0, time.UTC)
	dirRunID := "20260830T102000Z-2"
	snap, err := background.NewSnapshot(dirRunID, acceptedAt, "/ws-dir",
		background.ProcessIdentity{PID: deadPID, CreateTime: 1000},
		[]run.Task{{Prompt: "go", Model: "acme/m-1"}}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := background.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Create(snap); err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeListRecord(t, dir, dirRunID, deadPID, "2026-08-30T10:20:00Z", "/ws-flat", 1, true, "completed", "")
	flatRunID := "20260830T101500Z-1"
	flatPath := writeListRecord(t, dir, flatRunID, deadPID, "2026-08-30T10:15:00Z", "/ws-flat", 1, true, "completed", "")

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
	runs := document.Runs
	if len(runs) != 2 || runs[0].RunID != dirRunID || !strings.HasPrefix(runs[0].Path, filepath.Join(dir, dirRunID)) || strings.HasSuffix(runs[0].Path, ".jsonl") ||
		runs[1].RunID != flatRunID || runs[1].Path != flatPath {
		t.Fatalf("runs list = %+v, want the run directory once, then the flat record", runs)
	}
}

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

// TestRunsPruneDeletesABackgroundRunsRecordAndItsDirectory requires that
// prune deletes a finished background run whole — its run directory in
// the background store and the record it wrote into the records
// directory — reports it once, and that the pruned run is then unknown
// to runs status. The background root is pointed at the run's real
// store.
func TestRunsPruneDeletesABackgroundRunsRecordAndItsDirectory(t *testing.T) {
	manager, root := setupBackgroundRun(t, backgroundHappyScript("prune answer"))

	foregroundDir := t.TempDir()
	withRunlogDir(t, foregroundDir)
	withBackgroundRoot(t, root)

	// Started without startBackgroundRun: its cleanup drains the run,
	// which cannot be read once pruned. The run is drained here instead.
	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--json", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("run --background = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	backgroundRunID, _ := decodeJSONObject(t, stdout)["runId"].(string)
	if _, err := manager.Wait(context.Background(), backgroundRunID, 90*time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	runDir := filepath.Join(root, backgroundRunID)
	recordPath := filepath.Join(foregroundDir, backgroundRunID+".jsonl")
	waitForFinishedRecord(t, recordPath)

	code, stdout, stderr = runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	var document struct {
		Deleted []string `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode runs prune document: %v\n%s", err, stdout)
	}
	if !slices.Equal(document.Deleted, []string{backgroundRunID}) {
		t.Fatalf("prune deleted %v, want the background run %s once", document.Deleted, backgroundRunID)
	}
	for _, path := range []string{runDir, recordPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s after prune: %v, want it deleted", path, err)
		}
	}

	code, stdout, stderr = runCLI(t, []string{"runs", "status", backgroundRunID}, "")
	if code != 2 || !strings.Contains(stderr, "unknown run") {
		t.Fatalf("runs status of the pruned run = (%d, %q, %q), want exit 2 unknown run", code, stdout, stderr)
	}
}
