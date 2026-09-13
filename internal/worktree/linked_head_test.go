package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Tests for the caller's HEAD in a linked worktree: paths resolve from
// the main worktree root, but HEAD must be read from the caller's
// directory. When the caller runs from a linked worktree, the main
// worktree's HEAD is a different commit, so base and merged checks that
// read it from the root create the checkout one commit behind and
// refuse a clean merged checkout on removal.

// newLinkedRepo builds a two-commit repository whose main worktree is
// reset to the first commit and a linked worktree is checked out at the
// second. The linked worktree's HEAD (second) is a commit the main
// worktree's HEAD (first) does not contain.
func newLinkedRepo(t *testing.T) (root, linked, first, second string) {
	t.Helper()
	root = newTempRepo(t)
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write second commit: %v", err)
	}
	gitRun(t, root, "add", "file.txt")
	gitRun(t, root, "commit", "-q", "-m", "second")
	second = gitRun(t, root, "rev-parse", "HEAD")
	first = gitRun(t, root, "rev-parse", "HEAD~1")
	gitRun(t, root, "reset", "--hard", first)
	linked = filepath.Join(filepath.Dir(root), filepath.Base(root)+"-linked")
	gitRun(t, root, "worktree", "add", linked, second)
	return root, linked, first, second
}

// TestPrepareFromLinkedWorktreeUsesCallerHEAD verifies that Prepare run
// from inside a linked worktree bases the new checkout on that
// worktree's HEAD, not on the main worktree's HEAD.
func TestPrepareFromLinkedWorktreeUsesCallerHEAD(t *testing.T) {
	root, linked, first, second := newLinkedRepo(t)
	if head := gitRun(t, root, "rev-parse", "HEAD"); head != first {
		t.Fatalf("main worktree HEAD = %q, want first commit %q", head, first)
	}
	if head := gitRun(t, linked, "rev-parse", "HEAD"); head != second {
		t.Fatalf("linked worktree HEAD = %q, want second commit %q", head, second)
	}

	got, err := Prepare(context.Background(), linked, "probe")
	if err != nil {
		t.Fatalf("Prepare from linked worktree: %v", err)
	}
	if got.Head != second {
		t.Fatalf("Prepared.Head = %q, want the caller's HEAD %q (main worktree HEAD is %q)", got.Head, second, first)
	}
	if head := gitRun(t, got.Path, "rev-parse", "HEAD"); head != second {
		t.Fatalf("checkout HEAD = %q, want the caller's HEAD %q", head, second)
	}
}

// TestListFromLinkedWorktreeUsesCallerHEAD verifies that List run from
// inside a linked worktree judges Merged against that worktree's HEAD:
// a clean managed pair prepared there sits at the caller's HEAD, which
// the main worktree's HEAD does not contain, and must still be merged.
func TestListFromLinkedWorktreeUsesCallerHEAD(t *testing.T) {
	_, linked, first, second := newLinkedRepo(t)
	prep, err := Prepare(context.Background(), linked, "probe")
	if err != nil {
		t.Fatalf("Prepare from linked worktree: %v", err)
	}
	// The pair must start at the caller's HEAD, or the Merged assertion
	// below would measure the wrong checkout.
	if prep.Head != second {
		t.Fatalf("setup: Prepared.Head = %q, want the caller's HEAD %q", prep.Head, second)
	}

	got, err := List(context.Background(), linked)
	if err != nil {
		t.Fatalf("List from linked worktree: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %#v, want the probe entry", got)
	}
	if !got[0].Merged {
		t.Fatalf("Merged = false for a branch at the caller's HEAD %q (main worktree HEAD is %q): %#v", second, first, got[0])
	}
	if got[0].Dirty {
		t.Fatalf("Dirty = true for an untouched checkout: %#v", got[0])
	}
}
