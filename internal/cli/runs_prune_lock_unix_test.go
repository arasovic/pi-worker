//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/arasovic/pi-worker/internal/runlog"
)

// TestRunsPruneSparesARunDirectoryWhoseLockIsTakenAfterListing requires
// that the owner lock decides at the moment of the delete: a run
// directory whose lock was free when listed — its owner gone, so it
// lists interrupted — and is taken by someone else before the delete is
// spared and reported as running, with every file still there.
func TestRunsPruneSparesARunDirectoryWhoseLockIsTakenAfterListing(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	const runID = "20260830T101500Z-1"
	runDir := writeRunDir(t, dir, runID)
	lockPath := filepath.Join(runDir, runlog.OwnerLockName)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("write owner lock: %v", err)
	}

	// The lock is taken right after the run directories are listed, so
	// the listing saw it free and the delete finds it held. A second
	// open of the file is a separate lock holder, even in this process.
	original := backgroundListRuns
	backgroundListRuns = func(root string) ([]runlog.Run, error) {
		runs, err := original(root)
		if root == dir {
			f, openErr := os.Open(lockPath)
			if openErr != nil {
				t.Fatalf("open owner lock: %v", openErr)
			}
			t.Cleanup(func() { f.Close() })
			if held, lockErr := runlog.TryLockOwner(f); !held || lockErr != nil {
				t.Fatalf("take owner lock = (%v, %v)", held, lockErr)
			}
		}
		return runs, err
	}
	t.Cleanup(func() { backgroundListRuns = original })

	code, document, stderr := runPruneJSON(t, "0")
	if code != 0 || stderr != "" || len(document.Deleted) != 0 || !slices.Equal(document.KeptRunning, []string{runID}) {
		t.Fatalf("runs prune = (%d, %+v, %q), want %s kept running and nothing deleted", code, document, stderr, runID)
	}
	requirePresent(t, filepath.Join(runDir, "snapshot.json"), lockPath)
}
