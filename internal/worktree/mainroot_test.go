package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestPrepareLinkedWorktreeOfSeparateGitDir pins the external-git-
// directory layout (git init --separate-git-dir): the managed worktree
// must land inside a real working tree and never beside or inside git
// storage. The metadata directory is deliberately named with a .git
// suffix, the shape most likely to fool a filename-based root
// derivation.
//
// Measured, so the test's limits are stated rather than assumed: here
// the common directory is the external metadata directory, which does
// not end in a .git *component*, so root resolution falls back to the
// caller's own checkout. That is the same answer the pre-#191 code
// gave, so this test does not fail without the #191 fix — it guards
// against a future root derivation that invents a path in this layout.
func TestPrepareLinkedWorktreeOfSeparateGitDir(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()

	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+os.Getenv("HOME"))
		o, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, o)
		}
		return strings.TrimSpace(string(o))
	}

	// Main checkout with its git metadata in base/repository.git.
	main := filepath.Join(base, "main")
	meta := filepath.Join(base, "repository.git")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q", "--separate-git-dir="+meta)
	git(main, "config", "user.email", "test@pi-worker")
	git(main, "config", "user.name", "pi-worker test")
	if err := os.WriteFile(filepath.Join(main, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(main, "add", "file.txt")
	git(main, "commit", "-q", "-m", "initial")

	linked := filepath.Join(base, "linked")
	git(main, "worktree", "add", "-b", "feature", linked)

	got, err := Prepare(context.Background(), linked, "probe")
	if err != nil {
		t.Fatalf("Prepare from linked worktree of separate-git-dir repo: %v", err)
	}

	// The created checkout must be inside a directory that git itself
	// reports as a work tree — not inside or beside git storage.
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("eval symlinks base: %v", err)
	}
	rel, err := filepath.Rel(resolvedBase, got.Path)
	if err != nil {
		t.Fatalf("rel base: %v", err)
	}
	if strings.HasPrefix(rel, "repository.git") || strings.HasPrefix(rel, "..") {
		t.Fatalf("managed path %q is outside any working tree (rel %q)", got.Path, rel)
	}
	resolved, err := filepath.EvalSymlinks(got.Path)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	listOut := git(main, "worktree", "list", "--porcelain")
	if !strings.Contains(listOut, "worktree "+resolved) {
		t.Fatalf("managed path %q is not a work tree of the repository:\n%s", got.Path, listOut)
	}
	if _, err := os.Stat(filepath.Join(got.Path, "file.txt")); err != nil {
		t.Fatalf("checkout missing file.txt at %s: %v", got.Path, err)
	}
}
