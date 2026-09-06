package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prepareCleanMerged creates a managed pair that is clean and merged.
func prepareCleanMerged(t *testing.T, root, name string) Entry {
	t.Helper()
	prep, err := Prepare(context.Background(), root, name)
	if err != nil {
		t.Fatalf("Prepare %s: %v", name, err)
	}
	return Entry{Name: prep.Name, Path: prep.Path, Branch: prep.Branch, Dirty: false, Merged: true}
}

// TestRemoveExactCleanMergedPair removes one managed pair and verifies
// the checkout and branch are gone while an unrelated pair is untouched.
func TestRemoveExactCleanMergedPair(t *testing.T) {
	root := newTempRepo(t)

	alpha := prepareCleanMerged(t, root, "alpha")
	bravo := prepareCleanMerged(t, root, "bravo")

	if err := Remove(context.Background(), root, alpha); err != nil {
		t.Fatalf("Remove alpha: %v", err)
	}

	// Checkout and branch for alpha must be gone.
	if _, err := os.Stat(alpha.Path); err == nil {
		t.Fatalf("alpha checkout still exists at %s", alpha.Path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat alpha checkout: unexpected error %v", err)
	}
	if gitRefExists(t, root, "refs/heads/run/alpha") {
		t.Fatal("branch run/alpha still exists")
	}

	// bravo must remain.
	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List after remove: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != bravo.Name {
		t.Fatalf("unexpected remaining entries: %#v", entries)
	}
}

// TestRemoveInvalidNameRefuses verifies validateRemoveEligibility
// rejects an invalid name before any git call.
func TestRemoveInvalidNameRefuses(t *testing.T) {
	var calls int
	withRunGitFunc(t, func(_ context.Context, _ string, _ ...string) (string, error) {
		calls++
		return "", nil
	})
	err := Remove(context.Background(), ".", Entry{Name: "BadName"})
	if err == nil || !strings.Contains(err.Error(), "invalid worktree name") {
		t.Fatalf("err = %v, want invalid-name error", err)
	}
	if calls != 0 {
		t.Fatalf("runGitFunc called %d times, want 0", calls)
	}
}

// TestRemoveDirtyExpectedRefuses verifies a dirty expected entry is
// refused before any git call.
func TestRemoveDirtyExpectedRefuses(t *testing.T) {
	var calls int
	withRunGitFunc(t, func(_ context.Context, _ string, _ ...string) (string, error) {
		calls++
		return "", nil
	})
	err := Remove(context.Background(), ".", Entry{Name: "ok", Dirty: true, Merged: true})
	if err == nil || !strings.Contains(err.Error(), "checkout is dirty") {
		t.Fatalf("err = %v, want dirty-refusal error", err)
	}
	if calls != 0 {
		t.Fatalf("runGitFunc called %d times, want 0", calls)
	}
}

// TestRemoveUnmergedExpectedRefuses verifies an unmerged expected
// entry is refused before any git call.
func TestRemoveUnmergedExpectedRefuses(t *testing.T) {
	var calls int
	withRunGitFunc(t, func(_ context.Context, _ string, _ ...string) (string, error) {
		calls++
		return "", nil
	})
	err := Remove(context.Background(), ".", Entry{Name: "ok", Dirty: false, Merged: false})
	if err == nil || !strings.Contains(err.Error(), "branch") || !strings.Contains(err.Error(), "not merged") {
		t.Fatalf("err = %v, want unmerged-refusal error", err)
	}
	if calls != 0 {
		t.Fatalf("runGitFunc called %d times, want 0", calls)
	}
}

// TestRemoveStaleExpectedPathRefuses verifies a stale Path in the
// expected entry is rejected with retry and no mutation.
func TestRemoveStaleExpectedPathRefuses(t *testing.T) {
	root := newTempRepo(t)
	ent := prepareCleanMerged(t, root, "alpha")
	stale := ent
	stale.Path = "/nonexistent/path"

	err := Remove(context.Background(), root, stale)
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want retry error", err)
	}
	// Verify nothing was removed.
	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "alpha" {
		t.Fatalf("entries changed: %#v", entries)
	}
}

// TestRemoveStaleExpectedBranchRefuses verifies a stale Branch in
// the expected entry is rejected with retry and no mutation.
func TestRemoveStaleExpectedBranchRefuses(t *testing.T) {
	root := newTempRepo(t)
	ent := prepareCleanMerged(t, root, "alpha")
	stale := ent
	stale.Branch = "run/wrong"

	err := Remove(context.Background(), root, stale)
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want retry error", err)
	}
	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "alpha" {
		t.Fatalf("entries changed: %#v", entries)
	}
}

// TestRemoveStaleExpectedDirtyRefuses verifies that a dirty checkout
// is rejected without removing the real pair. The comparison check
// catches the Dirty mismatch first and returns retry.
func TestRemoveStaleExpectedDirtyRefuses(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Make the actual checkout dirty.
	if err := os.WriteFile(filepath.Join(prep.Path, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}
	// Expected says clean; comparison catches the mismatch.
	ent := Entry{Name: prep.Name, Path: prep.Path, Branch: prep.Branch, Dirty: false, Merged: true}

	err = Remove(context.Background(), root, ent)
	if err == nil {
		t.Fatal("expected error for dirty checkout")
	}
	// Comparison catches dirty mismatch and returns changed: retry.
	if !strings.Contains(err.Error(), "changed") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want changed: retry error", err)
	}
	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "alpha" {
		t.Fatalf("entries changed: %#v", entries)
	}
}

// TestRemoveStaleExpectedMergedRefuses verifies that an unmerged branch
// is rejected without removing the real pair. The comparison check
// catches the Merged mismatch first and returns retry.
func TestRemoveStaleExpectedMergedRefuses(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	alphaPath := prep.Path
	if err := os.WriteFile(filepath.Join(alphaPath, "adv.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, alphaPath, "add", "adv.txt")
	gitRun(t, alphaPath, "commit", "-q", "-m", "advance")
	// Move main branch forward so run/alpha is not merged into HEAD.
	gitRun(t, root, "commit", "--allow-empty", "-q", "-m", "advance main")

	// Expected says merged; comparison catches the mismatch.
	ent := Entry{Name: prep.Name, Path: prep.Path, Branch: prep.Branch, Dirty: false, Merged: true}

	err = Remove(context.Background(), root, ent)
	if err == nil {
		t.Fatal("expected error for unmerged branch")
	}
	// Comparison catches merged mismatch and returns changed: retry.
	if !strings.Contains(err.Error(), "changed") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want changed: retry error", err)
	}
	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "alpha" {
		t.Fatalf("entries changed: %#v", entries)
	}
}

// TestRemoveMissingTargetReturnsRetry verifies that trying to remove
// a non-existent name returns retry without changing unrelated pairs.
func TestRemoveMissingTargetReturnsRetry(t *testing.T) {
	root := newTempRepo(t)
	keep := prepareCleanMerged(t, root, "keep")

	err := Remove(context.Background(), root, Entry{
		Name:   "ghost",
		Path:   filepath.Join(root, ".pi-worker", "worktrees", "ghost"),
		Branch: "run/ghost",
		Dirty:  false,
		Merged: true,
	})
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want retry error", err)
	}

	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != keep.Name {
		t.Fatalf("unrelated pair disturbed: %#v", entries)
	}
}

// TestRemoveSpacesAndSubdirectory creates a repo whose path contains
// spaces and invokes Remove from a nested subdirectory.
func TestRemoveSpacesAndSubdirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo with spaces")
	newRepoAt(t, dir)
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}

	subdir := filepath.Join(root, "nested", "dir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.Chdir(subdir); err != nil {
		t.Fatalf("chdir subdir: %v", err)
	}

	prep, err := Prepare(context.Background(), subdir, "spacey")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ent := Entry{Name: prep.Name, Path: prep.Path, Branch: prep.Branch, Dirty: false, Merged: true}

	if err := Remove(context.Background(), subdir, ent); err != nil {
		t.Fatalf("Remove from subdir: %v", err)
	}
	if _, err := os.Stat(ent.Path); err == nil {
		t.Fatalf("spacey checkout still exists at %s", ent.Path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat spacey checkout: unexpected error %v", err)
	}
	if gitRefExists(t, root, "refs/heads/run/spacey") {
		t.Fatal("branch run/spacey still exists")
	}
}

// TestRemoveCancelledContext verifies that an already-cancelled context
// surfaces cancellation and leaves the pair untouched.
func TestRemoveCancelledContext(t *testing.T) {
	root := newTempRepo(t)
	ent := prepareCleanMerged(t, root, "alpha")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Remove(ctx, root, ent)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// Pair must remain intact.
	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "alpha" {
		t.Fatalf("entries changed: %#v", entries)
	}
}
