package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// writeRunDir writes one run directory <parent>/<runID>/ holding a
// non-terminal snapshot.json whose supervisor is the provably-dead
// deadPID beside its free owner lock, and returns the directory. The free
// lock decides, so the run lists as interrupted.
func writeRunDir(t *testing.T, parent, runID string) string {
	t.Helper()
	acceptedAt, err := runlog.ParseRunID(runID)
	if err != nil {
		t.Fatalf("ParseRunID: %v", err)
	}
	snap, err := background.NewSnapshot(runID, acceptedAt, "/ws-dir",
		background.ProcessIdentity{PID: deadPID, CreateTime: 1000},
		[]run.Task{{Prompt: "go", Model: "acme/m-1"}}, time.Minute, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	store, err := background.NewStore(parent)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	lock, err := store.Create(snap)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The supervisor that wrote it is gone: its owner lock is free.
	lock.Close()
	return filepath.Join(parent, runID)
}

// ageRunDir moves the modification time of every entry of dir, and of
// dir itself last, back by age: an entry written after the directory's
// own time was set would move it forward again.
func ageRunDir(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	then := time.Now().Add(-age)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if err := os.Chtimes(filepath.Join(dir, entry.Name()), then, then); err != nil {
			t.Fatalf("age %s: %v", entry.Name(), err)
		}
	}
	if err := os.Chtimes(dir, then, then); err != nil {
		t.Fatalf("age %s: %v", dir, err)
	}
}

// pruneDocumentResult is the decoded runs prune --json document.
type pruneDocumentResult struct {
	Deleted        []string `json:"deleted"`
	KeptNewest     int      `json:"keptNewest"`
	KeptRunning    []string `json:"keptRunning"`
	KeptUnreadable []string `json:"keptUnreadable"`
}

// runPruneJSON runs runs prune --keep keep --yes --json and decodes its
// document, whatever the exit code.
func runPruneJSON(t *testing.T, keep string) (int, pruneDocumentResult, string) {
	t.Helper()
	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", keep, "--yes", "--json"}, "")
	var document pruneDocumentResult
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode runs prune document (exit %d, stderr %q): %v\n%s", code, stderr, err, stdout)
	}
	return code, document, stderr
}

// requireGone fails the test for every path that still exists.
func requireGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after prune: %v", path, err)
		}
	}
}

// requirePresent fails the test for every path that no longer exists.
func requirePresent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s is gone after prune: %v", path, err)
		}
	}
}

// TestRunsPruneDeletesAnInterruptedLegacyBackgroundRun is the acceptance
// case: a legacy background directory holding only a non-terminal
// snapshot whose supervisor is gone, untouched for 19 days, is an
// interrupted run like any other, and prune deletes it — the directory
// is gone, not just the snapshot.
func TestRunsPruneDeletesAnInterruptedLegacyBackgroundRun(t *testing.T) {
	withRunlogDir(t, t.TempDir())
	bgRoot := t.TempDir()
	withBackgroundRoot(t, bgRoot)
	const runID = "20260911T085209Z-50677"
	runDir := writeRunDir(t, bgRoot, runID)
	ageRunDir(t, runDir, 19*24*time.Hour)

	code, document, stderr := runPruneJSON(t, "0")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %+v, %q), want exit 0 with empty stderr", code, document, stderr)
	}
	if !slices.Equal(document.Deleted, []string{runID}) {
		t.Fatalf("deleted = %v, want [%s]", document.Deleted, runID)
	}
	requireGone(t, runDir)
}

// TestRunsPruneRefusesARunDirectoryWithAnUnexpectedEntry requires that a
// run directory holding anything the supervisor does not write is
// refused whole: exit 9, the refusal on stderr, and every file still
// there — prune deletes known names one by one, never a tree.
func TestRunsPruneRefusesARunDirectoryWithAnUnexpectedEntry(t *testing.T) {
	for name, add := range map[string]func(string) error{
		"notes.txt": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o600)
		},
		"transcripts/": func(dir string) error {
			if err := os.Mkdir(filepath.Join(dir, "transcripts"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "transcripts", "a.jsonl"), []byte("{}\n"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			withRunlogDir(t, dir)
			const runID = "20260830T101500Z-1"
			runDir := writeRunDir(t, dir, runID)
			if err := os.WriteFile(filepath.Join(runDir, "record.jsonl"), []byte("{}\n"), 0o600); err != nil {
				t.Fatalf("write record.jsonl: %v", err)
			}
			if err := add(runDir); err != nil {
				t.Fatalf("add %s: %v", name, err)
			}
			entries, err := os.ReadDir(runDir)
			if err != nil {
				t.Fatalf("read run directory: %v", err)
			}

			code, document, stderr := runPruneJSON(t, "0")
			if code != 9 || len(document.Deleted) != 0 || !strings.Contains(stderr, "unexpected entry") {
				t.Fatalf("runs prune = (%d, %+v, %q), want exit 9, nothing deleted, the refusal on stderr", code, document, stderr)
			}
			for _, entry := range entries {
				requirePresent(t, filepath.Join(runDir, entry.Name()))
			}
		})
	}
}

// writeWorkerTranscript writes one transcript file into
// <runDir>/<worker>/, creating the worker directory.
func writeWorkerTranscript(t *testing.T, runDir, worker, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(runDir, worker), 0o700); err != nil {
		t.Fatalf("create %s: %v", worker, err)
	}
	if err := os.WriteFile(filepath.Join(runDir, worker, file), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write %s/%s: %v", worker, file, err)
	}
}

// TestRunsPruneDeletesWorkerTranscriptDirectories requires that a run
// directory holding a worker-<n>/ transcript directory per worker —
// including an empty one, left by a worker that never got a message
// written — is deleted whole.
func TestRunsPruneDeletesWorkerTranscriptDirectories(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	const runID = "20260830T101500Z-1"
	runDir := writeRunDir(t, dir, runID)
	writeWorkerTranscript(t, runDir, "worker-1", "2026-08-30T10-15-01-000Z_a.jsonl")
	writeWorkerTranscript(t, runDir, "worker-2", "2026-08-30T10-15-02-000Z_b.jsonl")
	if err := os.Mkdir(filepath.Join(runDir, "worker-3"), 0o700); err != nil {
		t.Fatalf("create worker-3: %v", err)
	}

	code, document, stderr := runPruneJSON(t, "0")
	if code != 0 || stderr != "" || !slices.Equal(document.Deleted, []string{runID}) {
		t.Fatalf("runs prune = (%d, %+v, %q), want exit 0 deleting %s", code, document, stderr, runID)
	}
	requireGone(t, runDir)
}

// TestRunsPruneRefusesAWorkerDirectoryItDoesNotRecognise requires that a
// run directory whose worker directory is anything but a real
// worker-<n> directory of *.jsonl files is refused whole: exit 9, the
// refusal on stderr, and every file at both levels still there — a
// symlinked worker directory's target included.
func TestRunsPruneRefusesAWorkerDirectoryItDoesNotRecognise(t *testing.T) {
	for name, add := range map[string]func(t *testing.T, runDir string) []string{
		"worker-1/subdir": func(t *testing.T, runDir string) []string {
			writeWorkerTranscript(t, runDir, "worker-1", "a.jsonl")
			if err := os.Mkdir(filepath.Join(runDir, "worker-1", "subdir"), 0o700); err != nil {
				t.Fatalf("create subdir: %v", err)
			}
			return []string{"worker-1/a.jsonl", "worker-1/subdir"}
		},
		"worker-1/notes.txt": func(t *testing.T, runDir string) []string {
			writeWorkerTranscript(t, runDir, "worker-1", "a.jsonl")
			writeWorkerTranscript(t, runDir, "worker-1", "notes.txt")
			return []string{"worker-1/a.jsonl", "worker-1/notes.txt"}
		},
		"worker-1/symlink.jsonl": func(t *testing.T, runDir string) []string {
			writeWorkerTranscript(t, runDir, "worker-1", "a.jsonl")
			if err := os.Symlink("a.jsonl", filepath.Join(runDir, "worker-1", "link.jsonl")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			return []string{"worker-1/a.jsonl", "worker-1/link.jsonl"}
		},
		"worker-1 symlink to a directory": func(t *testing.T, runDir string) []string {
			target := t.TempDir()
			if err := os.WriteFile(filepath.Join(target, "a.jsonl"), []byte("{}\n"), 0o600); err != nil {
				t.Fatalf("write target: %v", err)
			}
			if err := os.Symlink(target, filepath.Join(runDir, "worker-1")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			return []string{"worker-1", filepath.Join(target, "a.jsonl")}
		},
		"worker-0": func(t *testing.T, runDir string) []string {
			writeWorkerTranscript(t, runDir, "worker-0", "a.jsonl")
			return []string{"worker-0/a.jsonl"}
		},
		"worker-01": func(t *testing.T, runDir string) []string {
			writeWorkerTranscript(t, runDir, "worker-01", "a.jsonl")
			return []string{"worker-01/a.jsonl"}
		},
		"worker-x": func(t *testing.T, runDir string) []string {
			writeWorkerTranscript(t, runDir, "worker-x", "a.jsonl")
			return []string{"worker-x/a.jsonl"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			withRunlogDir(t, dir)
			const runID = "20260830T101500Z-1"
			runDir := writeRunDir(t, dir, runID)
			if err := os.WriteFile(filepath.Join(runDir, "record.jsonl"), []byte("{}\n"), 0o600); err != nil {
				t.Fatalf("write record.jsonl: %v", err)
			}
			// A valid worker directory beside the bad one must survive too.
			writeWorkerTranscript(t, runDir, "worker-2", "b.jsonl")
			kept := append([]string{"record.jsonl", "snapshot.json", runlog.OwnerLockName, "worker-2/b.jsonl"}, add(t, runDir)...)

			code, document, stderr := runPruneJSON(t, "0")
			if code != 9 || len(document.Deleted) != 0 || !strings.Contains(stderr, "unexpected entry") {
				t.Fatalf("runs prune = (%d, %+v, %q), want exit 9, nothing deleted, the refusal on stderr", code, document, stderr)
			}
			for _, path := range kept {
				if !filepath.IsAbs(path) {
					path = filepath.Join(runDir, path)
				}
				requirePresent(t, path)
			}
		})
	}
}

// TestRunsPruneDeletesOrphanedSnapshotStageAndDebugLog requires that the
// entries a supervisor leaves in its run directory — the record, the
// debug log, a free owner lock, and a snapshot replacement stage a kill
// left behind — are all deleted with the directory.
func TestRunsPruneDeletesOrphanedSnapshotStageAndDebugLog(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	const runID = "20260830T101500Z-1"
	runDir := writeRunDir(t, dir, runID)
	for _, name := range []string{"record.jsonl", "debug.log", ".snapshot.json.tmp-123456"} {
		if err := os.WriteFile(filepath.Join(runDir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	code, document, stderr := runPruneJSON(t, "0")
	if code != 0 || stderr != "" || !slices.Equal(document.Deleted, []string{runID}) {
		t.Fatalf("runs prune = (%d, %+v, %q), want exit 0 deleting %s", code, document, stderr, runID)
	}
	requireGone(t, runDir)
}

// TestRunsPruneUnknownRunDirectoryGrace requires the unknown rule for a
// run directory: one whose snapshot cannot be read and that changed
// within the grace window may be a run still being created, so it is
// kept and reported unreadable; the same directory untouched for longer
// is junk and deleted.
func TestRunsPruneUnknownRunDirectoryGrace(t *testing.T) {
	const runID = "20260830T101500Z-1"
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		withRunlogDir(t, dir)
		runDir := filepath.Join(dir, runID)
		if err := os.Mkdir(runDir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "snapshot.json"), []byte("not json"), 0o600); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}
		return runDir
	}

	t.Run("changed within the grace window", func(t *testing.T) {
		runDir := setup(t)
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
		if code != 0 || stderr != "" || stdout != "nothing to prune\n" {
			t.Fatalf("runs prune = (%d, %q, %q), want nothing to prune", code, stdout, stderr)
		}
		code, document, stderr := runPruneJSON(t, "0")
		if code != 0 || stderr != "" || !slices.Equal(document.KeptUnreadable, []string{runID}) || len(document.Deleted) != 0 {
			t.Fatalf("runs prune --json = (%d, %+v, %q), want %s kept unreadable", code, document, stderr, runID)
		}
		requirePresent(t, filepath.Join(runDir, "snapshot.json"))
	})

	t.Run("older than the grace window", func(t *testing.T) {
		runDir := setup(t)
		ageRunDir(t, runDir, 2*time.Hour)
		code, document, stderr := runPruneJSON(t, "0")
		if code != 0 || stderr != "" || !slices.Equal(document.Deleted, []string{runID}) {
			t.Fatalf("runs prune = (%d, %+v, %q), want %s deleted", code, document, stderr, runID)
		}
		requireGone(t, runDir)
	})
}

// TestRemoveRunDirSparesAnUnknownDirectoryChangedSinceSelection requires
// the delete-time half of the grace rule: an unknown run directory that
// was stale when listed and changed since is spared as unreadable at the
// delete, not removed.
func TestRemoveRunDirSparesAnUnknownDirectoryChangedSinceSelection(t *testing.T) {
	parent := t.TempDir()
	const runID = "20260830T101500Z-1"
	runDir := filepath.Join(parent, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	listed, err := os.Lstat(runDir)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	spared, err := removeRunDir(root, runlog.Run{RunID: runID, Outcome: "unknown", Path: runDir}, listed)
	if err != nil || spared != "unreadable" {
		t.Fatalf("removeRunDir = (%q, %v), want spared unreadable", spared, err)
	}
	requirePresent(t, runDir)
}

// TestRunsPruneDeletesEveryArtifactOfARunOnce requires that a run found
// in several places — a legacy background directory, a run directory in
// the records directory, and a flat record — is one run: prune deletes
// all three and reports its id once.
func TestRunsPruneDeletesEveryArtifactOfARunOnce(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	bgRoot := t.TempDir()
	withBackgroundRoot(t, bgRoot)
	const runID = "20260830T101500Z-1"
	legacyDir := writeRunDir(t, bgRoot, runID)
	runDir := writeRunDir(t, dir, runID)
	flat := writeListRecord(t, dir, runID, deadPID, "2026-08-30T10:15:00Z", "/ws", 1, true, "completed", "")

	code, document, stderr := runPruneJSON(t, "0")
	if code != 0 || stderr != "" || !slices.Equal(document.Deleted, []string{runID}) {
		t.Fatalf("runs prune = (%d, %+v, %q), want exit 0 deleting %s once", code, document, stderr, runID)
	}
	requireGone(t, legacyDir, runDir, flat)
}

// TestRunsPruneKeepCountsRunsAcrossLayouts requires that --keep counts
// runs, not files: the newest run, found as a legacy background
// directory and a flat record, is one kept run, and both its artifacts
// stay; the older run directory and flat record go.
func TestRunsPruneKeepCountsRunsAcrossLayouts(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	bgRoot := t.TempDir()
	withBackgroundRoot(t, bgRoot)
	const newest, middle, oldest = "20260830T103000Z-3", "20260830T102000Z-2", "20260830T101500Z-1"
	newestDir := writeRunDir(t, bgRoot, newest)
	newestFlat := writeListRecord(t, dir, newest, deadPID, "2026-08-30T10:30:00Z", "/ws", 1, true, "completed", "")
	middleDir := writeRunDir(t, dir, middle)
	oldestFlat := writeListRecord(t, dir, oldest, deadPID, "2026-08-30T10:15:00Z", "/ws", 1, true, "completed", "")

	code, document, stderr := runPruneJSON(t, "1")
	if code != 0 || stderr != "" || document.KeptNewest != 1 || !slices.Equal(document.Deleted, []string{oldest, middle}) {
		t.Fatalf("runs prune = (%d, %+v, %q), want the newest run kept and the two older deleted", code, document, stderr)
	}
	requirePresent(t, newestDir, newestFlat)
	requireGone(t, middleDir, oldestFlat)
}

// onFirstRead runs fn on the first stdin Read, then answers "y": the
// prompt's ReadString is the first read of stdin, so fn lands exactly
// while the question is on screen.
type onFirstRead struct {
	fn   func() error
	done bool
	err  error
}

func (r *onFirstRead) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		if err := r.fn(); err != nil {
			r.err = err
			return 0, err
		}
	}
	return copy(p, "y\n"), io.EOF
}

// TestRunsPruneLegacyRootSwapDuringPromptDeletesOnlyThePinnedDirectory
// swaps the legacy background root while the question is on screen: the
// root is renamed aside and a new root holding a run directory with the
// same id takes its place. The retry listing matches, and the delete
// goes through the root opened before the question: the renamed-aside
// run directory is gone, the swapped-in one untouched.
func TestRunsPruneLegacyRootSwapDuringPromptDeletesOnlyThePinnedDirectory(t *testing.T) {
	withRunlogDir(t, t.TempDir())
	withStdinIsTerminal(t, true)
	bgRoot := t.TempDir()
	withBackgroundRoot(t, bgRoot)
	const runID = "20260830T101500Z-1"
	writeRunDir(t, bgRoot, runID)
	aside := t.TempDir()
	if err := os.Remove(aside); err != nil {
		t.Fatalf("vacate aside: %v", err)
	}

	stdin := &onFirstRead{fn: func() error {
		if err := os.Rename(bgRoot, aside); err != nil {
			return err
		}
		if err := os.Mkdir(bgRoot, 0o700); err != nil {
			return err
		}
		writeRunDir(t, bgRoot, runID)
		return nil
	}}
	var stdout, stderr bytes.Buffer
	code := mainWithContext(context.Background(), []string{"runs", "prune", "--keep", "0"}, stdin, &stdout, &stderr)
	if stdin.err != nil {
		t.Fatalf("swap during the prompt failed: %v", stdin.err)
	}
	if code != 0 || stdout.String() != "deleted "+runID+"\nkept 0 newest\n" {
		t.Fatalf("prune with the swap = (%d, %q, %q), want exit 0 deleting %s", code, stdout.String(), stderr.String(), runID)
	}
	requireGone(t, filepath.Join(aside, runID))
	requirePresent(t, filepath.Join(bgRoot, runID, "snapshot.json"))
}

// TestRunsPruneInteractiveRetryRejectsChangedArtifacts requires that the
// retry after the question compares every place each selected run was
// found, not only the listed entry: a flat record that appears for a
// selected run's id while the question is on screen changes the
// selection, and nothing is deleted.
func TestRunsPruneInteractiveRetryRejectsChangedArtifacts(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	withStdinIsTerminal(t, true)
	const runID = "20260830T101500Z-1"
	runDir := writeRunDir(t, dir, runID)
	flat := filepath.Join(dir, runID+".jsonl")

	stdin := &onFirstRead{fn: func() error {
		writeListRecord(t, dir, runID, deadPID, "2026-08-30T10:15:00Z", "/ws", 1, true, "completed", "")
		return nil
	}}
	var stdout, stderr bytes.Buffer
	code := mainWithContext(context.Background(), []string{"runs", "prune", "--keep", "0"}, stdin, &stdout, &stderr)
	if stdin.err != nil {
		t.Fatalf("write during the prompt failed: %v", stdin.err)
	}
	if code != 9 || stdout.String() != "" || !strings.Contains(stderr.String(), "runs prune retry: selection changed") {
		t.Fatalf("prune with a new artifact = (%d, %q, %q), want exit 9 and the retry message", code, stdout.String(), stderr.String())
	}
	requirePresent(t, filepath.Join(runDir, "snapshot.json"), flat)
}
