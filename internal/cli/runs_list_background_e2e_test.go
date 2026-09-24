package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
// outcome and its snapshot path, and the order is newest first. The human
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
	if !strings.HasSuffix(background.Path, "snapshot.json") {
		t.Fatalf("background path = %q, want it to end in snapshot.json", background.Path)
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

// TestRunsPruneLeavesBackgroundRunsUntouched requires that prune, which
// reads only the records directory, neither deletes nor reports a
// background run. The background root is pointed at the run's real store,
// so a prune that wrongly merged the background store would see it and
// fail here.
func TestRunsPruneLeavesBackgroundRunsUntouched(t *testing.T) {
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
	writeListRecord(t, foregroundDir, "20000101T000000Z-1", deadPID, "2000-01-01T00:00:00Z", "/ws-fg", 1, true, "completed", "")

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatalf("background snapshot %s vanished after prune: %v", snapshotPath, err)
	}
	if strings.Contains(stdout, backgroundRunID) {
		t.Fatalf("prune reported the background run %s: %q", backgroundRunID, stdout)
	}
}
