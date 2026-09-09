package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Tests for the canonical repository root: managed worktrees must be
// created, listed and removed under the main worktree's
// .pi-worker/worktrees/ no matter which checkout the command runs in.

// TestPrepareFromLinkedWorktreeUsesMainRoot verifies that Prepare run
// from inside a linked worktree computes the managed path under the
// main worktree root, not under the caller's checkout.
func TestPrepareFromLinkedWorktreeUsesMainRoot(t *testing.T) {
	root := newTempRepo(t)
	linked := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-linked")
	gitRun(t, root, "worktree", "add", "-b", "feature", linked)

	if err := os.Chdir(linked); err != nil {
		t.Fatalf("chdir linked: %v", err)
	}

	got, err := Prepare(context.Background(), linked, "probe")
	if err != nil {
		t.Fatalf("Prepare from linked worktree: %v", err)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "probe")
	if got.Path != wantPath {
		t.Fatalf("Path = %q, want %q (main worktree root)", got.Path, wantPath)
	}
	if _, err := os.Stat(filepath.Join(linked, ".pi-worker")); err == nil {
		t.Fatal("nested .pi-worker created under the linked worktree")
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("managed checkout missing at %s: %v", wantPath, err)
	}
}

// TestListFromLinkedWorktreeMatchesMainRoot verifies that List run
// from inside a linked worktree resolves the same root as List run
// from the main worktree, so managed branches do not appear to be
// missing their checkouts.
func TestListFromLinkedWorktreeMatchesMainRoot(t *testing.T) {
	root := newTempRepo(t)
	if _, err := Prepare(context.Background(), root, "probe"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	linked := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-linked")
	gitRun(t, root, "worktree", "add", "-b", "feature", linked)

	fromMain, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List from main: %v", err)
	}
	fromLinked, err := List(context.Background(), linked)
	if err != nil {
		t.Fatalf("List from linked worktree: %v", err)
	}
	if len(fromLinked) != 1 || fromLinked[0] != fromMain[0] {
		t.Fatalf("List from linked = %#v, want %#v", fromLinked, fromMain)
	}
}

// TestRemoveUntouchedFromLinkedWorktree verifies that RemoveUntouched
// run from inside a linked worktree computes the same expected path as
// from the main worktree and removes the pair.
func TestRemoveUntouchedFromLinkedWorktree(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	linked := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-linked")
	gitRun(t, root, "worktree", "add", "-b", "feature", linked)

	if err := RemoveUntouched(context.Background(), linked, prep); err != nil {
		t.Fatalf("RemoveUntouched from linked worktree: %v", err)
	}
	if _, err := os.Stat(prep.Path); err == nil {
		t.Fatalf("checkout still exists at %s", prep.Path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat checkout: unexpected error %v", err)
	}
	if gitRefExists(t, root, "refs/heads/run/alpha") {
		t.Fatal("branch run/alpha still exists")
	}
}
