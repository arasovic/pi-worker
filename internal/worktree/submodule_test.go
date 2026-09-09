package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrepareInsideSubmoduleUsesSubmoduleRoot guards the submodule
// edge case: Prepare run from inside a git submodule must create the
// managed worktree under the submodule's own top level, not under the
// superproject's internal git storage. Inside a submodule
// git rev-parse --git-common-dir returns
// <superproject>/.git/modules/sub, so root resolution must fall back
// to git rev-parse --show-toplevel when the common directory is the
// git storage itself.
//
// This test is not a regression test for the #191 fix: it passes under
// the pre-#191 implementation too, because a submodule common directory
// does not end in .git and so never triggered the suffix strip. Its
// purpose is to pin the fallback behavior the edge case needs.
func TestPrepareInsideSubmoduleUsesSubmoduleRoot(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
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

	base := t.TempDir()

	// Upstream repo to serve as the submodule source.
	lib := filepath.Join(base, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	git(lib, "init", "-q")
	git(lib, "config", "user.email", "test@pi-worker")
	git(lib, "config", "user.name", "pi-worker test")
	if err := os.WriteFile(filepath.Join(lib, "lib.txt"), []byte("lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(lib, "add", "lib.txt")
	git(lib, "commit", "-q", "-m", "lib initial")

	// Superproject with the submodule added under sub/.
	root := filepath.Join(base, "outer")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git(root, "init", "-q")
	git(root, "config", "user.email", "test@pi-worker")
	git(root, "config", "user.name", "pi-worker test")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(root, "add", "file.txt")
	git(root, "commit", "-q", "-m", "initial")
	git(root, "-c", "protocol.file.allow=always", "submodule", "add", lib, "sub")
	git(root, "commit", "-q", "-m", "add submodule")

	sub := filepath.Join(root, "sub")
	// Sanity: this really is a submodule checkout with the shape that
	// motivated the fallback.
	if got := git(sub, "rev-parse", "--git-common-dir"); !strings.Contains(got, ".git/modules/") {
		t.Fatalf("expected submodule common dir under .git/modules/, got %q", got)
	}
	wantRoot, err := filepath.EvalSymlinks(sub)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	if got := git(sub, "rev-parse", "--show-toplevel"); got != wantRoot {
		t.Fatalf("submodule top level = %q, want %q", got, wantRoot)
	}

	got, err := Prepare(context.Background(), sub, "probe")
	if err != nil {
		t.Fatalf("Prepare from inside submodule: %v", err)
	}
	wantPath := filepath.Join(wantRoot, ".pi-worker", "worktrees", "probe")
	if got.Path != wantPath {
		t.Fatalf("Path = %q, want %q (submodule top level)", got.Path, wantPath)
	}
	if strings.Contains(got.Path, "/.git/") {
		t.Fatalf("managed worktree created inside git storage: %q", got.Path)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("managed checkout missing at %s: %v", wantPath, err)
	}
}
