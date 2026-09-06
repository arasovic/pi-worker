package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoveUntouchedSuccess removes a clean untouched worktree whose
// managed branch is not merged into caller HEAD. Remove would refuse
// (Merged=false) but RemoveUntouched ignores Merged and removes the
// exact pair.
func TestRemoveUntouchedSuccess(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Move main onto an orphan branch so run/alpha is no longer merged
	// into caller HEAD.
	gitRun(t, root, "checkout", "--orphan", "unrelated")
	gitRun(t, root, "commit", "--allow-empty", "-q", "-m", "orphan")

	// Verify the fresh inventory reports Merged=false.
	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Merged {
		t.Fatal("expected Merged=false after orphan")
	}

	// Remove would refuse because branch is not merged into HEAD.
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "alpha")
	wantBranch := "run/alpha"
	err = Remove(context.Background(), root, Entry{
		Name:   prep.Name,
		Path:   wantPath,
		Branch: wantBranch,
		Dirty:  false,
		Merged: false,
	})
	if err == nil || !strings.Contains(err.Error(), "not merged") {
		t.Fatalf("Remove should refuse unmerged: %v", err)
	}

	// Pair must still exist.
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("checkout gone after refused Remove: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+wantBranch) {
		t.Fatal("branch gone after refused Remove")
	}

	// RemoveUntouched succeeds because it ignores Merged.
	if err := RemoveUntouched(context.Background(), root, prep); err != nil {
		t.Fatalf("RemoveUntouched: %v", err)
	}

	if _, err := os.Stat(wantPath); err == nil {
		t.Fatalf("checkout still exists at %s", wantPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat checkout: unexpected error %v", err)
	}
	if gitRefExists(t, root, "refs/heads/"+wantBranch) {
		t.Fatal("branch still exists")
	}
}

// TestRemoveUntouchedDirtyRefused verifies that a dirty prepared
// checkout is refused and remains intact.
func TestRemoveUntouchedDirtyRefused(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Make the checkout dirty.
	if err := os.WriteFile(filepath.Join(prep.Path, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}

	err = RemoveUntouched(context.Background(), root, prep)
	if err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("err = %v, want dirty refusal", err)
	}

	// Checkout and branch remain.
	if _, err := os.Stat(prep.Path); err != nil {
		t.Fatalf("checkout gone: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+prep.Branch) {
		t.Fatal("branch gone")
	}
}

// TestRemoveUntouchedCheckoutHeadAdvanced verifies that when the
// managed branch HEAD has advanced from the recorded creation Head,
// RemoveUntouched refuses and the pair remains intact.
func TestRemoveUntouchedCheckoutHeadAdvanced(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Advance the managed branch by committing in the checkout.
	if err := os.WriteFile(filepath.Join(prep.Path, "adv.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, prep.Path, "add", "adv.txt")
	gitRun(t, prep.Path, "commit", "-q", "-m", "advance")

	// The recorded Head is stale; RemoveUntouched must refuse.
	err = RemoveUntouched(context.Background(), root, prep)
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want retry for advanced HEAD", err)
	}

	// Pair remains intact.
	if _, err := os.Stat(prep.Path); err != nil {
		t.Fatalf("checkout gone: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+prep.Branch) {
		t.Fatal("branch gone")
	}
}

// TestRemoveUntouchedInvalidNameRefuses verifies that an invalid
// name returns an error and makes zero Git calls.
func TestRemoveUntouchedInvalidNameRefuses(t *testing.T) {
	var calls int
	withRunGitFunc(t, func(_ context.Context, _ string, _ ...string) (string, error) {
		calls++
		return "", nil
	})
	err := RemoveUntouched(context.Background(), ".", Prepared{Name: "BadName"})
	if err == nil || !strings.Contains(err.Error(), "invalid worktree name") {
		t.Fatalf("err = %v, want invalid-name error", err)
	}
	if calls != 0 {
		t.Fatalf("runGitFunc called %d times, want 0", calls)
	}
}

// TestRemoveUntouchedMismatchedPath verifies that a mismatched Path
// is rejected and the real pair remains.
func TestRemoveUntouchedMismatchedPath(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	bad := prep
	bad.Path = "/nonexistent/path"

	err = RemoveUntouched(context.Background(), root, bad)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want path-mismatch error", err)
	}

	if _, err := os.Stat(prep.Path); err != nil {
		t.Fatalf("checkout gone: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+prep.Branch) {
		t.Fatal("branch gone")
	}
}

// TestRemoveUntouchedMismatchedBranch verifies that a mismatched
// Branch is rejected and the real pair remains.
func TestRemoveUntouchedMismatchedBranch(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	bad := prep
	bad.Branch = "run/wrong"

	err = RemoveUntouched(context.Background(), root, bad)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want branch-mismatch error", err)
	}

	if _, err := os.Stat(prep.Path); err != nil {
		t.Fatalf("checkout gone: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+prep.Branch) {
		t.Fatal("branch gone")
	}
}

// TestRemoveUntouchedEmptyHead verifies that an empty Head returns
// without removing the real pair.
func TestRemoveUntouchedEmptyHead(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	bad := prep
	bad.Head = ""

	err = RemoveUntouched(context.Background(), root, bad)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want empty-head error", err)
	}

	if _, err := os.Stat(prep.Path); err != nil {
		t.Fatalf("checkout gone: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+prep.Branch) {
		t.Fatal("branch gone")
	}
}

// TestRemoveUntouchedMissingTargetReturnsRetry verifies that a
// non-existent name returns retry with no mutation.
func TestRemoveUntouchedMissingTargetReturnsRetry(t *testing.T) {
	root := newTempRepo(t)
	keep := prepareCleanMerged(t, root, "keep")

	err := RemoveUntouched(context.Background(), root, Prepared{
		Name:   "ghost",
		Path:   filepath.Join(root, ".pi-worker", "worktrees", "ghost"),
		Branch: "run/ghost",
		Head:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want retry error", err)
	}

	// keep must remain.
	entries, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != keep.Name {
		t.Fatalf("unrelated pair disturbed: %#v", entries)
	}
}

// TestRemoveUntouchedLockedWorktreeRefused verifies that a locked
// managed worktree is refused and remains intact. List() returns an
// error for locked worktrees.
func TestRemoveUntouchedLockedWorktreeRefused(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Lock the managed worktree.
	gitRun(t, root, "worktree", "lock", prep.Path)

	err = RemoveUntouched(context.Background(), root, prep)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v, want locked refusal", err)
	}

	// Checkout and branch remain.
	if _, err := os.Stat(prep.Path); err != nil {
		t.Fatalf("checkout gone: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+prep.Branch) {
		t.Fatal("branch gone")
	}
}

// TestRemoveUntouchedCancelledContext verifies that an
// already-cancelled context surfaces cancellation and leaves the
// pair intact.
func TestRemoveUntouchedCancelledContext(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = RemoveUntouched(ctx, root, prep)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// Pair remains intact.
	if _, err := os.Stat(prep.Path); err != nil {
		t.Fatalf("checkout gone: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+prep.Branch) {
		t.Fatal("branch gone")
	}
}
