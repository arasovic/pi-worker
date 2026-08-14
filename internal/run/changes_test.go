package run

import (
	"context"
	"encoding/json"
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

func TestControllerChangesDirtyBeforeStateOmitted(t *testing.T) {
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithChanges(t, newScriptedWorker(), dir)
	changes := result.Changes
	if changes == nil {
		t.Fatalf("changes = nil, want an omitted manifest on a dirty before-state")
	}
	if changes.Omitted != reasonDirtyBeforeState {
		t.Fatalf("omitted = %q, want %q", changes.Omitted, reasonDirtyBeforeState)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the reason", changes)
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

func TestControllerChangesOmittedNeverLooksLikeMeasuredZero(t *testing.T) {
	// The other half of the #14 failure class: an omitted manifest must
	// carry its reason in the document, so a consumer can tell it from a
	// measured zero even though both serialize totalFiles as zero.
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
	if document["omitted"] != reasonDirtyBeforeState {
		t.Fatalf("omitted = %v, want %q in %s", document["omitted"], reasonDirtyBeforeState, data)
	}
	if _, present := document["files"]; present {
		t.Fatalf("omitted manifest serialized files: %s", data)
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

func TestControllerChangesNotInsideGitWorkTreeStaysNil(t *testing.T) {
	dir := t.TempDir()
	result := runWithChanges(t, newScriptedWorker(), dir)
	if result.Changes != nil {
		t.Fatalf("changes = %#v, want nil outside a git work tree", result.Changes)
	}
}

func TestControllerDeadContextOmissionDistinctFromNonGitWorkspace(t *testing.T) {
	// A context already done when the before-state inspection ran must
	// omit with a stated reason, while a live context in the same
	// non-work-tree workspace stays the one silent nil case. The two
	// must stay distinguishable: absence is never readable as a
	// measured result, and a genuine non-work-tree absence is never
	// mislabeled as this omission.
	dir := t.TempDir()
	t.Run("live context", func(t *testing.T) {
		result := runWithChanges(t, newScriptedWorker(), dir)
		if result.Changes != nil {
			t.Fatalf("changes = %#v, want nil outside a git work tree", result.Changes)
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
