package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #241: a repository reached through a symlink has two spellings of the
// same directory — the caller's, which Prepare records, and the
// physical one, which git reports. Removing an untouched checkout
// compares those spellings, so it must compare directory identity.

// symlinkedRepo builds a one-commit repository in a real directory and
// returns its physical path plus a symlink that names the same
// directory. Reaching the repository through the symlink is what pi-
// worker does on macOS, where /var and /tmp are symlinks.
func symlinkedRepo(t *testing.T) (repo, link string) {
	t.Helper()
	// Start from the physical spelling of the temporary directory so the
	// test symlink is the only symlink left in the path.
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	repo = filepath.Join(real, "repo")
	newRepoAt(t, repo)
	link = filepath.Join(real, "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlink %s to %s: %v", link, repo, err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatalf("stat %s: %v", link, err)
	}
	repoInfo, err := os.Stat(repo)
	if err != nil {
		t.Fatalf("stat %s: %v", repo, err)
	}
	if !os.SameFile(linkInfo, repoInfo) {
		t.Skipf("%q is not the same directory as %q", link, repo)
	}
	return repo, link
}

// prepareThroughLink prepares "alpha" in the symlinked repository and
// requires that the condition #241 measured is actually reproduced
// here: Prepare records the caller's spelling, git lists the physical
// one, and the inventory still finds the pair.
func prepareThroughLink(t *testing.T, repo, link string) Prepared {
	t.Helper()
	prep, err := Prepare(context.Background(), link, "alpha")
	if err != nil {
		t.Fatalf("Prepare through %s: %v", link, err)
	}
	wantPath := filepath.Join(link, ".pi-worker", "worktrees", "alpha")
	if prep.Path != wantPath {
		t.Fatalf("Prepare Path = %q, want the caller's spelling %q", prep.Path, wantPath)
	}
	entries, err := List(context.Background(), link)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %#v, want the alpha entry", entries)
	}
	if entries[0].Name != "alpha" || entries[0].Branch != "run/alpha" {
		t.Fatalf("entry = %#v, want alpha on run/alpha", entries[0])
	}
	if entries[0].Path == prep.Path {
		// The platform cannot reproduce two spellings, so this test would
		// pass without exercising the comparison it guards.
		t.Skipf("git lists %q, the spelling Prepare recorded; cannot reproduce #241 here", entries[0].Path)
	}
	if !sameDir(entries[0].Path, statIfExists(prep.Path)) {
		t.Fatalf("List path %q and Prepare path %q are not the same directory", entries[0].Path, prep.Path)
	}
	return prep
}

// requirePairGone fails unless the checkout is gone under both
// spellings and its branch ref is gone.
func requirePairGone(t *testing.T, repo, link string, prep Prepared) {
	t.Helper()
	for _, path := range []string{
		prep.Path,
		filepath.Join(repo, ".pi-worker", "worktrees", prep.Name),
	} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("checkout still exists at %s", path)
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
	}
	if gitRefExists(t, repo, "refs/heads/"+prep.Branch) {
		t.Fatalf("branch %s still exists", prep.Branch)
	}
	entries, err := List(context.Background(), link)
	if err != nil {
		t.Fatalf("List after removal: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("inventory still lists %#v", entries)
	}
}

// requirePairIntact fails unless the checkout and its branch remain.
func requirePairIntact(t *testing.T, repo string, prep Prepared) {
	t.Helper()
	if _, err := os.Stat(prep.Path); err != nil {
		t.Fatalf("checkout gone: %v", err)
	}
	if !gitRefExists(t, repo, "refs/heads/"+prep.Branch) {
		t.Fatal("branch gone")
	}
}

// TestRemoveUntouchedSymlinkedRoot gives back an untouched checkout of
// a repository reached through a symlink.
func TestRemoveUntouchedSymlinkedRoot(t *testing.T) {
	repo, link := symlinkedRepo(t)
	prep := prepareThroughLink(t, repo, link)

	if err := RemoveUntouched(context.Background(), link, prep); err != nil {
		t.Fatalf("RemoveUntouched through symlinked root: %v", err)
	}
	requirePairGone(t, repo, link, prep)
}

// TestRemoveUntouchedSymlinkedRootDifferentCallerSpelling prepares
// through the symlink and gives the pair back when the retry resolves
// the repository under its physical spelling instead, so the recorded
// path differs from the computed one as well.
func TestRemoveUntouchedSymlinkedRootDifferentCallerSpelling(t *testing.T) {
	repo, link := symlinkedRepo(t)
	prep := prepareThroughLink(t, repo, link)

	wantPath := filepath.Join(repo, ".pi-worker", "worktrees", "alpha")
	if prep.Path == wantPath {
		t.Skipf("caller spelling %q already matches computed %q", prep.Path, wantPath)
	}
	if err := RemoveUntouched(context.Background(), repo, prep); err != nil {
		t.Fatalf("RemoveUntouched from physical root with symlinked expectation: %v", err)
	}
	requirePairGone(t, repo, link, prep)
}

// TestRemoveUntouchedSymlinkedRootStillRefusesChanged verifies that
// comparing by identity does not weaken the content reconfirmations: a
// dirty checkout and a moved HEAD are still refused through a symlink.
func TestRemoveUntouchedSymlinkedRootStillRefusesChanged(t *testing.T) {
	t.Run("dirty checkout", func(t *testing.T) {
		repo, link := symlinkedRepo(t)
		prep := prepareThroughLink(t, repo, link)

		if err := os.WriteFile(filepath.Join(prep.Path, "dirty.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write dirty: %v", err)
		}
		err := RemoveUntouched(context.Background(), link, prep)
		if err == nil || !strings.Contains(err.Error(), "dirty") {
			t.Fatalf("err = %v, want dirty refusal", err)
		}
		requirePairIntact(t, repo, prep)
	})

	t.Run("moved HEAD", func(t *testing.T) {
		repo, link := symlinkedRepo(t)
		prep := prepareThroughLink(t, repo, link)

		if err := os.WriteFile(filepath.Join(prep.Path, "adv.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		gitRun(t, prep.Path, "add", "adv.txt")
		gitRun(t, prep.Path, "commit", "-q", "-m", "advance")

		err := RemoveUntouched(context.Background(), link, prep)
		if err == nil || !strings.Contains(err.Error(), "changed: retry") {
			t.Fatalf("err = %v, want retry for advanced HEAD", err)
		}
		requirePairIntact(t, repo, prep)
	})
}

// TestRemoveSymlinkedRoot gives back a clean merged checkout of a
// repository reached through a symlink: Remove compares the same two
// spellings as RemoveUntouched.
func TestRemoveSymlinkedRoot(t *testing.T) {
	repo, link := symlinkedRepo(t)
	prep := prepareThroughLink(t, repo, link)
	ent := Entry{Name: prep.Name, Path: prep.Path, Branch: prep.Branch, Dirty: false, Merged: true}

	if err := Remove(context.Background(), link, ent); err != nil {
		t.Fatalf("Remove through symlinked root: %v", err)
	}
	requirePairGone(t, repo, link, prep)
}
