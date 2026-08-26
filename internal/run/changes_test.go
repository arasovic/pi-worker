package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
)

// changesMutatingWorker is a fake worker that applies workspace mutations
// between the controller's before and after inspections and reports a
// completed result, the same shape stashMutatingWorker uses.
type changesMutatingWorker struct {
	mutate func(dir string) error
}

func (w *changesMutatingWorker) Run(ctx context.Context, req pi.WorkerRequest) pi.WorkerResult {
	if err := w.mutate(req.Workspace); err != nil {
		return pi.WorkerResult{Status: pi.StatusError, Error: err.Error()}
	}
	return pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "ok"}
}

// gitCommit stages and commits every change in dir, the way a run that
// commits its own work does; the change manifest must still report the
// files because its base is the before-state HEAD, not the current one.
func gitCommit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "work")
}

func runWithChanges(t *testing.T, worker pi.Worker, dir string) Result {
	t.Helper()
	req := validRequest("a")
	req.Workspace = dir
	result, err := New(worker, WithGitInspector(NewDefaultGitInspector())).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return result
}

func TestControllerChangesModifiedTrackedFile(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || changes.Truncated || len(changes.Files) != 1 {
		t.Fatalf("changes = %#v, want one file", changes)
	}
	file := changes.Files[0]
	if file.Path != "file.txt" || file.Status != "modified" || file.Added != 2 || file.Deleted != 0 || file.Binary {
		t.Fatalf("file = %#v, want file.txt modified +2/-0", file)
	}
}

func TestControllerChangesAddedUntrackedFile(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "new.txt"), []byte("alpha\nbeta\n"), 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
		t.Fatalf("changes = %#v, want one measured file", changes)
	}
	file := changes.Files[0]
	if file.Path != "new.txt" || file.Status != "added" || file.Added != 2 || file.Deleted != 0 || file.Binary {
		t.Fatalf("file = %#v, want new.txt added +2/-0", file)
	}
}

func TestControllerChangesEmptyUntrackedFileRecorded(t *testing.T) {
	// An empty new file still prints a record with zero counts: git
	// diff --no-index against /dev/null reports a new file even when
	// both sides are empty, exiting non-zero as its normal outcome, and
	// the manifest must carry the path rather than drop it. The path
	// also counts toward TotalFiles.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 || changes.Truncated {
		t.Fatalf("changes = %#v, want the empty file counted", changes)
	}
	file := changes.Files[0]
	if file.Path != "empty.txt" || file.Status != "added" || file.Added != 0 || file.Deleted != 0 || file.Binary {
		t.Fatalf("file = %#v, want empty.txt added +0/-0", file)
	}
}

func TestControllerChangesDeletedTrackedFile(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.Remove(filepath.Join(dir, "file.txt"))
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
		t.Fatalf("changes = %#v, want one measured file", changes)
	}
	file := changes.Files[0]
	if file.Path != "file.txt" || file.Status != "deleted" || file.Added != 0 || file.Deleted != 1 || file.Binary {
		t.Fatalf("file = %#v, want file.txt deleted +0/-1", file)
	}
}

func TestControllerChangesWorkTheRunCommittedStillReported(t *testing.T) {
	// The base of the manifest is the before-state HEAD, so a run that
	// commits its work must still list every changed file even though
	// HEAD now sits on top of the run's commit.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x\ny\nz\n"), 0o644); err != nil {
			return err
		}
		commit := exec.Command("git", "commit", "-q", "-am", "work")
		commit.Dir = dir
		return commit.Run()
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 2 || len(changes.Files) != 2 {
		t.Fatalf("changes = %#v, want the two committed files", changes)
	}
	byPath := map[string]FileChange{}
	for _, file := range changes.Files {
		byPath[file.Path] = file
	}
	if file, ok := byPath["file.txt"]; !ok || file.Status != "modified" || file.Added != 1 || file.Deleted != 0 {
		t.Fatalf("file.txt = %#v, want committed modification +1/-0", byPath["file.txt"])
	}
	if file, ok := byPath["new.txt"]; !ok || file.Status != "added" || file.Added != 3 || file.Deleted != 0 {
		t.Fatalf("new.txt = %#v, want committed addition +3/-0", byPath["new.txt"])
	}
}

func TestControllerChangesDirtyBeforeStateUntouchedPathAbsent(t *testing.T) {
	// A dirty before-state is measured, not omitted: the before-dirty
	// file carries the base forward, and a worker that never touched it
	// leaves its stamp unchanged, so the path is subtracted out of the
	// manifest entirely.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil {
		t.Fatalf("changes = nil, want a measured manifest on a dirty before-state")
	}
	if changes.Omitted != "" {
		t.Fatalf("omitted = %q, want a measured manifest", changes.Omitted)
	}
	if changes.TotalFiles != 0 || len(changes.Files) != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want the untouched dirty path absent", changes)
	}
}

func TestControllerChangesDirtyBeforeStateModifiedFurther(t *testing.T) {
	// A worker that modifies an already-dirty path further lists the
	// path with its counts against the before-state HEAD and marks it
	// dirtyBefore: the counts include the pre-run work that was already
	// there, so the run's share cannot be separated out.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\nbeta\n"), 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 || changes.Truncated {
		t.Fatalf("changes = %#v, want the one modified file", changes)
	}
	file := changes.Files[0]
	if file.Path != "file.txt" || file.Status != "modified" || file.Added != 2 || file.Deleted != 1 || !file.DirtyBefore {
		t.Fatalf("file = %#v, want file.txt modified +2/-1 with dirtyBefore", file)
	}
}

func TestControllerChangesDirtyBeforeStateRevertedToCommit(t *testing.T) {
	// A worker that reverts an already-dirty tracked file to its
	// committed content drops out of the HEAD diff entirely — the file
	// matches the base — yet the caller's uncommitted work was
	// destroyed. The dirty union keeps the path in the manifest with
	// zero counts and dirtyBefore true, so the destruction is reported
	// instead of going silent.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("two\nthree\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 || changes.Truncated {
		t.Fatalf("changes = %#v, want the reverted path in the manifest", changes)
	}
	file := changes.Files[0]
	if file.Path != "file.txt" || file.Status != "modified" || file.Added != 0 || file.Deleted != 0 || !file.DirtyBefore {
		t.Fatalf("file = %#v, want file.txt modified +0/-0 with dirtyBefore", file)
	}
}

func TestControllerChangesDirtyBeforeStateRestoredToPreRunContent(t *testing.T) {
	// A worker that restores an already-dirty file to its exact pre-run
	// content leaves the stamp unchanged — same size, same modification
	// time — so the path is subtracted and absent from the manifest.
	// The deliberate, accepted false negative: net change is zero and
	// the stamp cannot see a write that leaves no trace.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty content\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// The stamp the snapshot will record is the dirty file's own: size
	// and modification time as they sit right before the run.
	info, err := os.Lstat(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	stampTime := info.ModTime()
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty content\n"), 0o644); err != nil {
			return err
		}
		// Restore the modification time too, so the stamp matches: the
		// run wrote nothing the manifest can see, by construction.
		return os.Chtimes(filepath.Join(dir, "file.txt"), stampTime, stampTime)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 0 || len(changes.Files) != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want the restored path absent", changes)
	}
}

func TestControllerChangesDirtyBeforeStateUntrackedUntouchedAbsent(t *testing.T) {
	// An already-dirty untracked file the worker never touched is
	// subtracted like a tracked one: the run contributed nothing to it.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 0 || len(changes.Files) != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want the untouched untracked path absent", changes)
	}
}

func TestControllerChangesDirtySubmoduleUntouchedAbsent(t *testing.T) {
	// A submodule's contents are another repository's business: a
	// submodule already dirty before the run that the run never touched
	// must be absent from the manifest, on a run that changed nothing.
	// The assertion is on the manifest's contents — an untouched
	// submodule must not appear as a changed path at all, not merely
	// that some count changed. Before the fix the measurement diff and
	// the dirty snapshot listed the submodule, and the write check then
	// named it an undeclared write.
	dir := newGitRepo(t)
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	isolateGitConfig(t)
	runGit(t, sub, "init", "-q")
	runGit(t, sub, "config", "user.email", "test@pi-worker")
	runGit(t, sub, "config", "user.name", "pi-worker test")
	if err := os.WriteFile(filepath.Join(sub, "subfile.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write subfile: %v", err)
	}
	runGit(t, sub, "add", "subfile.txt")
	runGit(t, sub, "commit", "-q", "-m", "sub init")
	subHead := strings.TrimSpace(runGit(t, sub, "rev-parse", "HEAD"))
	// Register the submodule as a gitlink in the superproject with
	// plumbing only — git submodule add would need the file protocol
	// tweak — then commit the gitlink and dirty the submodule's work
	// tree, which makes the superproject report ` M sub`.
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+subHead+",sub")
	runGit(t, dir, "commit", "-q", "-m", "add submodule")
	if err := os.WriteFile(filepath.Join(sub, "subfile.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("dirty subfile: %v", err)
	}
	if out := runGit(t, dir, "status", "--porcelain"); !strings.Contains(out, " M sub") {
		t.Fatalf("test setup: superproject status = %q, want the dirty submodule reported", out)
	}
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	for _, file := range changes.Files {
		if file.Path == "sub" {
			t.Fatalf("manifest lists the untouched dirty submodule: %#v", changes.Files)
		}
	}
	if changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want the untouched dirty submodule absent entirely", changes)
	}
}

func TestControllerChangesUntrackedVisibleDespiteShowUntrackedFilesNo(t *testing.T) {
	// Dirtiness must not depend on the repository's display preference:
	// git status --porcelain with status.showUntrackedFiles=no hides
	// untracked files, so the before-state would be recorded as clean
	// while an untracked file sits in the tree, and the run would then
	// be attributed that file. The inspection forces the setting, so
	// the before-state is dirty, the file is stamped, and the untouched
	// path is subtracted from the manifest.
	dir := newGitRepo(t)
	runGit(t, dir, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// Prove the setting actually hides the file from the display:
	// without this the test would pass vacuously.
	if out := runGit(t, dir, "status", "--porcelain"); out != "" {
		t.Fatalf("test setup: status --porcelain = %q, want empty with the hiding setting", out)
	}
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 0 || len(changes.Files) != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want the untouched untracked path absent", changes)
	}
}

func TestControllerChangesChmodPlusXOnDirtyFileReported(t *testing.T) {
	// chmod +x changes neither size nor modification time, so a stamp of
	// size plus mtime alone would match and the run's change would
	// disappear from the manifest. The stamp records the executable bit
	// — the one mode bit git tracks — so a chmod that makes a file
	// executable registers while a chmod between two non-executable
	// modes does not.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.Chmod(filepath.Join(dir, "file.txt"), 0o755)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 || changes.Truncated {
		t.Fatalf("changes = %#v, want the chmodded file in the manifest", changes)
	}
	file := changes.Files[0]
	if file.Path != "file.txt" || file.Status != "modified" || file.Added != 1 || file.Deleted != 1 || !file.DirtyBefore {
		t.Fatalf("file = %#v, want file.txt modified +1/-1 with dirtyBefore", file)
	}
}

func TestControllerChangesDirtyBeforeStateDeletedStillDeletedAbsent(t *testing.T) {
	// A path deleted before the run and still deleted carries an absent
	// stamp before and after, so it is subtracted: the deletion belongs
	// to the caller's pre-run state, not to the run.
	dir := newGitRepo(t)
	if err := os.Remove(filepath.Join(dir, "file.txt")); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 0 || len(changes.Files) != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want the still-deleted path absent", changes)
	}
}

func TestControllerChangesDirtyBeforeStateDeletedThenRestoredReported(t *testing.T) {
	// A path deleted before the run that the worker restores to its
	// committed content moved its stamp from absent to present, so it
	// is a candidate: it appears in neither the tracked diff nor the
	// untracked listing, and the fallback gives it a zero-count
	// dirtyBefore entry rather than silence.
	dir := newGitRepo(t)
	if err := os.Remove(filepath.Join(dir, "file.txt")); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 || changes.Truncated {
		t.Fatalf("changes = %#v, want the restored path in the manifest", changes)
	}
	file := changes.Files[0]
	if file.Path != "file.txt" || file.Status != "modified" || file.Added != 0 || file.Deleted != 0 || !file.DirtyBefore {
		t.Fatalf("file = %#v, want file.txt modified +0/-0 with dirtyBefore", file)
	}
}

func TestControllerChangesDirtyBeforeStateUntrackedDeletedReported(t *testing.T) {
	// An untracked file the run deletes was never in the base tree, so
	// base presence cannot decide its status: the stamp moved, the path
	// appears in neither the tracked diff nor the untracked listing, and
	// the fallback must call the gone path deleted, not added — a path
	// that does not exist any more was not added.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.Remove(filepath.Join(dir, "stray.txt"))
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 || changes.Truncated {
		t.Fatalf("changes = %#v, want the deleted path in the manifest", changes)
	}
	file := changes.Files[0]
	if file.Path != "stray.txt" || file.Status != "deleted" || file.Added != 0 || file.Deleted != 0 || !file.DirtyBefore {
		t.Fatalf("file = %#v, want stray.txt deleted +0/-0 with dirtyBefore", file)
	}
}

func TestControllerChangesCleanBeforeStateUnchanged(t *testing.T) {
	// A clean before-state behaves exactly as before: no subtraction,
	// no dirtyBefore markings, and the measured result is identical to
	// the pre-fix manifest.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 || changes.Truncated {
		t.Fatalf("changes = %#v, want one file", changes)
	}
	file := changes.Files[0]
	if file.Path != "file.txt" || file.Status != "modified" || file.Added != 2 || file.Deleted != 0 || file.DirtyBefore {
		t.Fatalf("file = %#v, want file.txt modified +2/-0 without dirtyBefore", file)
	}
}

func TestControllerChangesUnbornHeadOmitted(t *testing.T) {
	dir := t.TempDir()
	isolateGitConfig(t)
	runGit(t, dir, "init", "-q")
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil {
		t.Fatalf("changes = nil, want an omitted manifest on an unborn HEAD")
	}
	if changes.Omitted != reasonUnbornHead {
		t.Fatalf("omitted = %q, want %q", changes.Omitted, reasonUnbornHead)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the reason", changes)
	}
}

func TestControllerChangesNothingMeasuredZero(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil {
		t.Fatalf("changes = nil, want a measured manifest")
	}
	if changes.Omitted != "" || changes.TotalFiles != 0 || len(changes.Files) != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want measured-zero", changes)
	}
	// The #14 failure class: measured zero must serialize as totalFiles
	// present with zero, never as an absent field.
	data, err := json.Marshal(changes)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := document["totalFiles"]; !present {
		t.Fatalf("measured zero serialized without totalFiles: %s", data)
	}
	if document["totalFiles"] != float64(0) {
		t.Fatalf("totalFiles = %v, want 0", document["totalFiles"])
	}
	if _, present := document["omitted"]; present {
		t.Fatalf("measured zero serialized with omitted: %s", data)
	}
	if _, present := document["files"]; present {
		t.Fatalf("measured zero serialized with files: %s", data)
	}
}

func TestControllerChangesDirtyBeforeStateMeasuredZeroSerializesAsMeasured(t *testing.T) {
	// The #14 failure class, on the fixed side: a dirty before-state
	// with nothing touched now measures zero, and measured zero must
	// serialize as totalFiles present with zero — never as an absent
	// field, and never with an omission reason beside it.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithChanges(t, newScriptedWorker(), dir)
	data, err := json.Marshal(result.Changes)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if document["totalFiles"] != float64(0) {
		t.Fatalf("totalFiles = %v, want 0", document["totalFiles"])
	}
	if _, present := document["omitted"]; present {
		t.Fatalf("measured zero serialized with omitted: %s", data)
	}
	if _, present := document["files"]; present {
		t.Fatalf("measured zero serialized with files: %s", data)
	}
}

func TestControllerChangesBinaryFiles(t *testing.T) {
	t.Run("untracked binary added", func(t *testing.T) {
		dir := newGitRepo(t)
		result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0xff, 0x00}, 0o644)
		}}, dir)
		changes := result.Changes
		if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
			t.Fatalf("changes = %#v, want one measured file", changes)
		}
		file := changes.Files[0]
		if file.Path != "blob.bin" || file.Status != "added" || !file.Binary || file.Added != 0 || file.Deleted != 0 {
			t.Fatalf("file = %#v, want blob.bin binary added with zero counts", file)
		}
	})
	t.Run("tracked binary modified", func(t *testing.T) {
		dir := newGitRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "logo.bin"), []byte{0x89, 0x50, 0x4e, 0x47, 0x00}, 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		gitCommit(t, dir)
		result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "logo.bin"), []byte{0xff, 0x00, 0x89, 0x50}, 0o644)
		}}, dir)
		changes := result.Changes
		if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
			t.Fatalf("changes = %#v, want one measured file", changes)
		}
		file := changes.Files[0]
		if file.Path != "logo.bin" || file.Status != "modified" || !file.Binary || file.Added != 0 || file.Deleted != 0 {
			t.Fatalf("file = %#v, want logo.bin binary modified with zero counts", file)
		}
	})
	t.Run("tracked binary deleted", func(t *testing.T) {
		dir := newGitRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "logo.bin"), []byte{0x89, 0x50, 0x4e, 0x47, 0x00}, 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		gitCommit(t, dir)
		result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
			return os.Remove(filepath.Join(dir, "logo.bin"))
		}}, dir)
		changes := result.Changes
		if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
			t.Fatalf("changes = %#v, want one measured file", changes)
		}
		file := changes.Files[0]
		if file.Path != "logo.bin" || file.Status != "deleted" || !file.Binary || file.Added != 0 || file.Deleted != 0 {
			t.Fatalf("file = %#v, want logo.bin binary deleted with zero counts", file)
		}
	})
}

func TestControllerChangesNoFinalNewlineAdded(t *testing.T) {
	// For files the run itself produces, the field asserts exactly what
	// it measures: the last byte is not a newline. A file ending in a
	// carriage return is flagged, an empty file is not (there is no last
	// byte), and a file ending in a newline carries no field.
	tests := []struct {
		name    string
		content []byte
		want    bool
	}{
		{name: "no final newline", content: []byte("no newline"), want: true},
		{name: "ends in carriage return", content: []byte("ends\r"), want: true},
		{name: "empty", content: nil, want: false},
		{name: "ends in newline", content: []byte("ends\n"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := newGitRepo(t)
			result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "new.txt"), test.content, 0o644)
			}}, dir)
			changes := result.Changes
			if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
				t.Fatalf("changes = %#v, want one measured file", changes)
			}
			file := changes.Files[0]
			if file.Path != "new.txt" || file.Status != statusAdded {
				t.Fatalf("file = %#v, want new.txt added", file)
			}
			if file.NoFinalNewline != test.want {
				t.Fatalf("noFinalNewline = %v, want %v", file.NoFinalNewline, test.want)
			}
		})
	}
}

func TestControllerChangesNoFinalNewlineModified(t *testing.T) {
	// A modified tracked file carries the field when the run left its
	// last byte a non-newline, and no field when it ends in a newline.
	// The manifest cannot say who made it that way — that is what the
	// descriptive-not-a-verdict contract is about — only that it is so.
	tests := []struct {
		name    string
		content []byte
		want    bool
	}{
		{name: "no final newline", content: []byte("two"), want: true},
		{name: "ends in newline", content: []byte("two\n"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := newGitRepo(t)
			result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "file.txt"), test.content, 0o644)
			}}, dir)
			changes := result.Changes
			if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
				t.Fatalf("changes = %#v, want one measured file", changes)
			}
			file := changes.Files[0]
			if file.Path != "file.txt" || file.Status != statusModified {
				t.Fatalf("file = %#v, want file.txt modified", file)
			}
			if file.NoFinalNewline != test.want {
				t.Fatalf("noFinalNewline = %v, want %v", file.NoFinalNewline, test.want)
			}
		})
	}
}

func TestControllerChangesNoFinalNewlineSymlinkExcluded(t *testing.T) {
	// The regular-file guard exists for the symlink case: opening a
	// symlink follows it and measures the last byte of a different
	// file, so without the guard the entry for the link would report a
	// property of its target. The binary field staying false proves git
	// measured it as text and the guard is what kept the field off.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("content"), 0o644); err != nil {
			return err
		}
		return os.Symlink("target.txt", filepath.Join(dir, "link.txt"))
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 2 || len(changes.Files) != 2 {
		t.Fatalf("changes = %#v, want the two measured files", changes)
	}
	var link *FileChange
	for i := range changes.Files {
		if changes.Files[i].Path == "link.txt" {
			link = &changes.Files[i]
		}
	}
	if link == nil {
		t.Fatalf("manifest = %#v, want the symlink listed", changes.Files)
	}
	if link.Binary {
		t.Fatalf("symlink = %#v, want git to count it as text", *link)
	}
	if link.NoFinalNewline {
		t.Fatalf("symlink = %#v, want no noFinalNewline field on a symlink", *link)
	}
}

func TestControllerChangesNoFinalNewlineDeletedAbsent(t *testing.T) {
	// A deleted file has no content to examine: the Lstat fails and the
	// field is simply not written, like every other best-effort miss.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.Remove(filepath.Join(dir, "file.txt"))
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
		t.Fatalf("changes = %#v, want one measured file", changes)
	}
	file := changes.Files[0]
	if file.Path != "file.txt" || file.Status != statusDeleted {
		t.Fatalf("file = %#v, want file.txt deleted", file)
	}
	if file.NoFinalNewline {
		t.Fatalf("file = %#v, want no field on a deleted file", file)
	}
}

func TestControllerChangesNoFinalNewlineBinaryExcluded(t *testing.T) {
	// An untracked binary file that was measured carries no field: git
	// reports it binary and the guard skips the read entirely, so the
	// field cannot ride on an entry whose counts are already unreadable.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0xff, 0x00}, 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
		t.Fatalf("changes = %#v, want one measured file", changes)
	}
	file := changes.Files[0]
	if file.Path != "blob.bin" || !file.Binary {
		t.Fatalf("file = %#v, want blob.bin binary", file)
	}
	if file.NoFinalNewline {
		t.Fatalf("file = %#v, want no field on a binary entry", file)
	}
}

func TestControllerChangesNoFinalNewlineNormalFileAbsentFromJSON(t *testing.T) {
	// A normal file ending in a newline must serialize without the
	// noFinalNewline key at all: omitempty is pinned by a test rather
	// than by assumption, so a future struct change that drops the tag
	// fails here instead of silently growing every entry.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("two\n"), 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 || len(changes.Files) != 1 {
		t.Fatalf("changes = %#v, want one measured file", changes)
	}
	if changes.Files[0].NoFinalNewline {
		t.Fatalf("file = %#v, want no field on a newline-terminated file", changes.Files[0])
	}
	data, err := json.Marshal(changes)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	entries, ok := document["files"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("files = %#v, want one serialized entry", document["files"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("entry = %#v, want a JSON object", entries[0])
	}
	if _, present := entry["noFinalNewline"]; present {
		t.Fatalf("entry = %s, want no noFinalNewline key on a newline-terminated file", data)
	}
}

func TestMeasureNoFinalNewlineUnreadableFileLeavesManifestIntact(t *testing.T) {
	// A file that cannot be read gets no field, and the manifest it
	// rides on stays exactly as it was: no error, no omission reason,
	// and no other field touched. The guard is exercised directly
	// because a full run cannot reach this state — git itself refuses to
	// hash an unreadable file, which omits the whole manifest before the
	// measurement ever ran.
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(path, 0o644)
	changes := &Changes{
		Omitted:    "",
		Files:      []FileChange{{Path: "locked.txt", Status: statusAdded, Added: 1, Deleted: 0}},
		TotalFiles: 1,
	}
	for i := range changes.Files {
		measureNoFinalNewline(dir, &changes.Files[i])
	}
	document := changes.Files[0]
	if document.NoFinalNewline {
		t.Fatalf("file = %#v, want no field on an unreadable file", document)
	}
	if changes.Omitted != "" {
		t.Fatalf("omitted = %q, want the manifest free of any omission reason", changes.Omitted)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 {
		t.Fatalf("changes = %#v, want the manifest untouched", changes)
	}
	if document.Path != "locked.txt" || document.Status != statusAdded || document.Added != 1 || document.Deleted != 0 || document.Binary || document.DirtyBefore {
		t.Fatalf("file = %#v, want every other field intact", document)
	}
}

func TestControllerChangesPathsWithSpaceAndNonASCII(t *testing.T) {
	// -z output carries paths literally: a space and a multi-byte UTF-8
	// character must round-trip through the actual git commands.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "spaced file.txt"), []byte("a\nb\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "café_naïve.txt"), []byte("ünïcode\n"), 0o644)
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 2 || len(changes.Files) != 2 {
		t.Fatalf("changes = %#v, want the two special paths measured", changes)
	}
	byPath := map[string]FileChange{}
	for _, file := range changes.Files {
		byPath[file.Path] = file
	}
	if file, ok := byPath["spaced file.txt"]; !ok || file.Status != "added" || file.Added != 2 || file.Deleted != 0 {
		t.Fatalf("spaced file.txt = %#v, want added +2/-0", byPath["spaced file.txt"])
	}
	if file, ok := byPath["café_naïve.txt"]; !ok || file.Status != "added" || file.Added != 1 || file.Deleted != 0 {
		t.Fatalf("café_naïve.txt = %#v, want added +1/-0", byPath["café_naïve.txt"])
	}
}

func TestControllerChangesCapsAtHundredEntries(t *testing.T) {
	dir := newGitRepo(t)
	const total = 120
	for i := 0; i < total; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(path, []byte("line\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	gitCommit(t, dir)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		for i := 0; i < total; i++ {
			path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
			if err := os.WriteFile(path, []byte("changed\n"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != total {
		t.Fatalf("totalFiles = %d, want %d", changes.TotalFiles, total)
	}
	if len(changes.Files) != maxChangeFiles {
		t.Fatalf("files = %d entries, want the %d cap", len(changes.Files), maxChangeFiles)
	}
	if !changes.Truncated {
		t.Fatalf("truncated = false, want true when the cap dropped entries")
	}
	// Every file carries the same churn, so the tie-break orders the
	// kept entries by path; the cap must keep the first hundred.
	if changes.Files[0].Path != "f000.txt" {
		t.Fatalf("first kept = %q, want f000.txt", changes.Files[0].Path)
	}
	if changes.Files[maxChangeFiles-1].Path != "f099.txt" {
		t.Fatalf("last kept = %q, want f099.txt", changes.Files[maxChangeFiles-1].Path)
	}
	for i, file := range changes.Files {
		if file.Added != 1 || file.Deleted != 1 || file.Status != "modified" {
			t.Fatalf("file %d = %#v, want modified +1/-1", i, file)
		}
	}
}

func TestControllerChangesUntrackedBeyondCapDroppedUnmeasured(t *testing.T) {
	// The cap trims the untracked listing before any churn is known, in
	// git ls-files order, because measuring churn costs one command per
	// file: an untracked file beyond position maxChangeFiles never
	// appears among the kept entries however large it is. TotalFiles
	// still counts it. The measured entries are ordered by churn
	// descending then by path, so the first kept path is the highest-
	// churn measured file, not a dropped one.
	dir := newGitRepo(t)
	const dropped = 3
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		for i := 0; i < maxChangeFiles; i++ {
			path := filepath.Join(dir, fmt.Sprintf("a%03d.txt", i))
			if err := os.WriteFile(path, []byte("line\n"), 0o644); err != nil {
				return err
			}
		}
		// The z-files sort after the a-files in git ls-files order, so the
		// cap drops them, and their hundred-line churn would put them
		// first among the kept entries had they been measured at all.
		for i := 0; i < dropped; i++ {
			path := filepath.Join(dir, fmt.Sprintf("z%03d.txt", i))
			if err := os.WriteFile(path, []byte(strings.Repeat("x\n", 100)), 0o644); err != nil {
				return err
			}
		}
		return nil
	}}, dir)
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != maxChangeFiles+dropped {
		t.Fatalf("totalFiles = %d, want %d", changes.TotalFiles, maxChangeFiles+dropped)
	}
	if !changes.Truncated {
		t.Fatalf("truncated = false, want true when the cap dropped entries")
	}
	if len(changes.Files) != maxChangeFiles {
		t.Fatalf("files = %d entries, want the %d cap", len(changes.Files), maxChangeFiles)
	}
	// Every kept entry is one of the listing's first hundred files, each
	// +1/-0; churn order on equal churn is path order. Had the dropped
	// files been measured, their +100/-0 records would top the list.
	if changes.Files[0].Path != "a000.txt" || changes.Files[0].Added != 1 || changes.Files[0].Status != "added" {
		t.Fatalf("first kept = %#v, want a000.txt added +1/-0", changes.Files[0])
	}
	for _, file := range changes.Files {
		if file.Added != 1 || file.Deleted != 0 || file.Status != "added" || strings.HasPrefix(file.Path, "z") {
			t.Fatalf("file = %#v, want an added +1/-0 file from the first hundred of the listing", file)
		}
	}
}

func TestControllerChangesWorkTreeNotConfirmedOmitted(t *testing.T) {
	// A workspace whose work tree cannot be confirmed — the directory is
	// not a git work tree, git is missing entirely, or the guard failed
	// for a transient reason — must omit with a stated reason, never
	// with an absent field: a consumer cannot tell a run that changed
	// nothing from a run that could not be measured, and the reason must
	// not claim to know which of the three causes it is because the code
	// does not know. This is distinct from reasonMeasurementFail, which
	// covers a guard that passed and a later command that failed.
	dir := t.TempDir()
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil {
		t.Fatalf("changes = nil, want an omitted manifest when the work tree cannot be confirmed")
	}
	if changes.Omitted != reasonWorkTreeUnconfirmed {
		t.Fatalf("omitted = %q, want %q", changes.Omitted, reasonWorkTreeUnconfirmed)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the reason", changes)
	}
}

func TestControllerDeadContextOmissionDistinctFromNonGitWorkspace(t *testing.T) {
	// A context already done when the before-state inspection ran must
	// omit with the context-already-done reason, while a live context in
	// the same non-work-tree workspace must omit with the
	// work-tree-unconfirmed reason. The two must stay distinguishable:
	// absence is never readable as a measured result, and the
	// work-tree-unconfirmed omission is never mislabeled as the
	// context one.
	dir := t.TempDir()
	t.Run("live context", func(t *testing.T) {
		result := runWithChanges(t, newScriptedWorker(), dir)
		changes := result.Changes
		if changes == nil {
			t.Fatalf("changes = nil, want the work-tree-unconfirmed omission outside a git work tree")
		}
		if changes.Omitted != reasonWorkTreeUnconfirmed {
			t.Fatalf("omitted = %q, want %q", changes.Omitted, reasonWorkTreeUnconfirmed)
		}
		if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
			t.Fatalf("changes = %#v, want no measured fields alongside the reason", changes)
		}
	})
	t.Run("dead context", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		req := validRequest("a")
		req.Workspace = dir
		result, err := New(newScriptedWorker(), WithGitInspector(NewDefaultGitInspector())).Run(ctx, req)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		changes := result.Changes
		if changes == nil {
			t.Fatalf("changes = nil, want an omitted manifest on a context already done")
		}
		if changes.Omitted != reasonContextDone {
			t.Fatalf("omitted = %q, want %q", changes.Omitted, reasonContextDone)
		}
		if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
			t.Fatalf("changes = %#v, want no measured fields alongside the reason", changes)
		}
	})
}

func TestControllerChangesBeforeInspectionErrorOmitted(t *testing.T) {
	// An inspection error before any worker starts must omit the
	// manifest with the measurement-failed reason — only a workspace
	// that was never a git work tree is silent — and the reason string
	// is what the caller reads, not the presence of the field.
	worker := newScriptedWorker()
	inspector := &scriptedGitInspector{
		errs: []error{errors.New("git failure")},
	}
	result, err := New(worker, WithGitInspector(inspector)).Run(context.Background(), validRequest("a"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	changes := result.Changes
	if changes == nil {
		t.Fatalf("changes = nil, want an omitted manifest on a before-inspection error")
	}
	if changes.Omitted != reasonMeasurementFail {
		t.Fatalf("omitted = %q, want %q", changes.Omitted, reasonMeasurementFail)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the reason", changes)
	}
}

func TestControllerChangesMeasurementFailureOmitted(t *testing.T) {
	// measureChanges' own failure return: a real git command failure
	// after the workspace was confirmed to be a git work tree must
	// omit with the reason rather than leaving the field nil or
	// guessing. The worker removes the workspace itself, so every
	// manifest command fails to start; the after-state inspection is
	// the one silent no-op, like a workspace that was never a git work
	// tree.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.RemoveAll(dir)
	}}, dir)
	changes := result.Changes
	if changes == nil {
		t.Fatalf("changes = nil, want an omitted manifest on a measurement failure")
	}
	if changes.Omitted != reasonMeasurementFail {
		t.Fatalf("omitted = %q, want %q", changes.Omitted, reasonMeasurementFail)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the reason", changes)
	}
}

func TestControllerChangesNotGatedByGitTripwire(t *testing.T) {
	// The manifest exists precisely for the files the git tripwire
	// deliberately ignores: a run that only leaves modified files behind
	// carries changes and no git object.
	dir := newGitRepo(t)
	result := runWithChanges(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("two\n"), 0o644)
	}}, dir)
	if result.Git != nil {
		t.Fatalf("git = %#v, want nil for a dirty-only difference", result.Git)
	}
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 {
		t.Fatalf("changes = %#v, want the modified file measured despite the nil git object", changes)
	}
}

func TestControllerChangesMeasuredOnExpiredContext(t *testing.T) {
	// A run that timed out mid-edit is exactly the run whose changes a
	// caller most needs: the manifest must still be measured, under a
	// context independent of the expired parent, against the before
	// state recorded while the parent was still alive. The deadline
	// fires while the worker is still holding the workspace, the way a
	// real run that consumed its whole budget mid-edit would.
	dir := newGitRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	worker := &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("left behind\n"), 0o644); err != nil {
			return err
		}
		// Still mid-edit when the deadline fires: wait the parent out
		// instead of returning, so the run reports timed-out.
		<-ctx.Done()
		return nil
	}}
	req := validRequest("a")
	req.Workspace = dir
	result, err := New(worker, WithGitInspector(NewDefaultGitInspector())).Run(ctx, req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunTimedOut {
		t.Fatalf("status = %q, want timed-out", result.Status)
	}
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || changes.TotalFiles != 1 {
		t.Fatalf("changes = %#v, want the mid-edit file measured on an expired run", changes)
	}
	if changes.Files[0].Path != "new.txt" || changes.Files[0].Status != "added" {
		t.Fatalf("file = %#v, want new.txt added", changes.Files[0])
	}
}
