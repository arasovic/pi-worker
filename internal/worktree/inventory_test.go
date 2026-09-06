package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepoAt creates a one-commit repo at dir and chdirs into it.
func newRepoAt(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if o, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), e, o)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@pi-worker")
	git("config", "user.name", "pi-worker test")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	git("add", "file.txt")
	git("commit", "-q", "-m", "initial")
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Errorf("cleanup chdir: %v", err)
		}
	})
}

// --- List ---

func TestListNoManagedWorktrees(t *testing.T) {
	root := newTempRepo(t)
	got, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %#v, want none", got)
	}
}

func TestListOneExactCleanMergedPair(t *testing.T) {
	root := newTempRepo(t)
	if _, err := Prepare(context.Background(), root, "probe"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %#v, want one entry", got)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "probe")
	if got[0] != (Entry{Name: "probe", Path: wantPath, Branch: "run/probe", Dirty: false, Merged: true}) {
		t.Fatalf("entry = %#v", got[0])
	}
}

func TestListDirtyAndUnmergedReportingSorted(t *testing.T) {
	root := newTempRepo(t)
	bravoPrep, err := Prepare(context.Background(), root, "bravo")
	if err != nil {
		t.Fatalf("Prepare bravo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bravoPrep.Path, "file.txt"), []byte("one\nmodified\n"), 0o644); err != nil {
		t.Fatalf("dirty bravo: %v", err)
	}
	alphaPrep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare alpha: %v", err)
	}
	if err := os.WriteFile(filepath.Join(alphaPrep.Path, "file.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("stage alpha: %v", err)
	}
	gitRun(t, alphaPrep.Path, "add", "file.txt")
	gitRun(t, alphaPrep.Path, "commit", "-q", "-m", "advance alpha")
	got, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v, want two", got)
	}
	alphaP := filepath.Join(root, ".pi-worker", "worktrees", "alpha")
	bravoP := filepath.Join(root, ".pi-worker", "worktrees", "bravo")
	want := []Entry{
		{Name: "alpha", Path: alphaP, Branch: "run/alpha", Dirty: false, Merged: false},
		{Name: "bravo", Path: bravoP, Branch: "run/bravo", Dirty: true, Merged: true},
	}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestListIgnoresUnrelatedWorktreesAndBranches(t *testing.T) {
	root := newTempRepo(t)
	otherWorktree := filepath.Join(t.TempDir(), "other worktree")
	gitRun(t, root, "worktree", "add", "-b", "feature", otherWorktree)
	gitRun(t, root, "branch", "unrelated")
	if _, err := Prepare(context.Background(), root, "probe"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "probe")
	if len(got) != 1 || got[0] != (Entry{Name: "probe", Path: wantPath, Branch: "run/probe", Dirty: false, Merged: true}) {
		t.Fatalf("got %#v, want one probe entry", got)
	}
}

func TestListRejectsMissingCheckoutAndMismatchedBranch(t *testing.T) {
	t.Run("managed branch without checkout", func(t *testing.T) {
		root := newTempRepo(t)
		gitRun(t, root, "branch", "run/missing")
		got, err := List(context.Background(), root)
		if err == nil || !strings.Contains(err.Error(), `managed branch "run/missing" is missing its checkout`) {
			t.Fatalf("err = %v, want missing-checkout refusal", err)
		}
		if got != nil {
			t.Fatalf("got %#v, want nil", got)
		}
	})

	t.Run("managed checkout without branch", func(t *testing.T) {
		root := t.TempDir()
		withRunGitFunc(t, func(ctx context.Context, dir string, args ...string) (string, error) {
			switch strings.Join(args, " ") {
			case "rev-parse --show-toplevel":
				return root, nil
			case "rev-parse HEAD":
				return "abc123", nil
			case "worktree list --porcelain":
				return "worktree " + filepath.Join(root, ".pi-worker", "worktrees", "alpha") + "\n", nil
			case "for-each-ref --format=%(refname:short) refs/heads":
				return "run/alpha\n", nil
			case "for-each-ref --merged=abc123 --format=%(refname:short) refs/heads":
				return "run/alpha\n", nil
			default:
				t.Fatalf("unexpected git call: %q in %q", strings.Join(args, " "), dir)
				return "", nil
			}
		})
		got, err := List(context.Background(), root)
		if err == nil || !strings.Contains(err.Error(), "is missing its branch") {
			t.Fatalf("err = %v, want missing-branch refusal", err)
		}
		if got != nil {
			t.Fatalf("got %#v, want nil", got)
		}
	})

	t.Run("managed checkout with mismatched branch", func(t *testing.T) {
		root := t.TempDir()
		withRunGitFunc(t, func(ctx context.Context, dir string, args ...string) (string, error) {
			switch strings.Join(args, " ") {
			case "rev-parse --show-toplevel":
				return root, nil
			case "rev-parse HEAD":
				return "abc123", nil
			case "worktree list --porcelain":
				return "worktree " + filepath.Join(root, ".pi-worker", "worktrees", "alpha") + "\nbranch refs/heads/run/beta\n", nil
			case "for-each-ref --format=%(refname:short) refs/heads":
				return "run/beta\n", nil
			case "for-each-ref --merged=abc123 --format=%(refname:short) refs/heads":
				return "run/beta\n", nil
			default:
				t.Fatalf("unexpected git call: %q in %q", strings.Join(args, " "), dir)
				return "", nil
			}
		})
		got, err := List(context.Background(), root)
		if err == nil || !strings.Contains(err.Error(), `points to "refs/heads/run/beta"`) {
			t.Fatalf("err = %v, want mismatched-branch refusal", err)
		}
		if got != nil {
			t.Fatalf("got %#v, want nil", got)
		}
	})
}

func TestListSpacesAndSubdirectoryInvocation(t *testing.T) {
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
	if _, err := Prepare(context.Background(), subdir, "spacey"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got, err := List(context.Background(), subdir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "spacey")
	if len(got) != 1 || got[0] != (Entry{Name: "spacey", Path: wantPath, Branch: "run/spacey", Dirty: false, Merged: true}) {
		t.Fatalf("got %#v, want one spacey entry", got)
	}
}

func TestListMergedEvaluatedAgainstCallerHEAD(t *testing.T) {
	root := newTempRepo(t)
	gitRun(t, root, "branch", "-q", "-m", "caller-branch")
	if _, err := Prepare(context.Background(), root, "advance"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	advancePath := filepath.Join(root, ".pi-worker", "worktrees", "advance")
	if err := os.WriteFile(filepath.Join(advancePath, "file.txt"), []byte("one\nfrom managed\n"), 0o644); err != nil {
		t.Fatalf("write advance: %v", err)
	}
	gitRun(t, advancePath, "add", "file.txt")
	gitRun(t, advancePath, "commit", "-q", "-m", "advance the managed pair")
	if b := gitRun(t, root, "rev-parse", "--abbrev-ref", "HEAD"); b != "caller-branch" {
		t.Fatalf("caller HEAD = %q, want caller-branch", b)
	}
	got, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "advance")
	if len(got) != 1 || got[0] != (Entry{Name: "advance", Path: wantPath, Branch: "run/advance", Dirty: false, Merged: false}) {
		t.Fatalf("got %#v, want advance unmerged", got)
	}
}

func TestListCheckoutStatusFailureSurfacesPath(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, ".pi-worker", "worktrees", "alpha")
	withRunGitFunc(t, func(ctx context.Context, dir string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			return root, nil
		case "rev-parse HEAD":
			return "abc123", nil
		case "worktree list --porcelain":
			return "worktree " + root + "\nHEAD abc123\nbranch refs/heads/main\n\n" +
				"worktree " + managedPath + "\nHEAD abc123\nbranch refs/heads/run/alpha\n", nil
		case "for-each-ref --format=%(refname:short) refs/heads":
			return "main\nrun/alpha\n", nil
		case "for-each-ref --merged=abc123 --format=%(refname:short) refs/heads":
			return "main\nrun/alpha\n", nil
		case "status --porcelain=v1":
			return "", fmt.Errorf("mock status failure for %q", dir)
		default:
			t.Fatalf("unexpected git call: %q in %q", strings.Join(args, " "), dir)
			return "", nil
		}
	})
	got, err := List(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), managedPath) {
		t.Fatalf("err = %v, want it to contain managed path %q", err, managedPath)
	}
	if !strings.Contains(err.Error(), "mock status failure") {
		t.Fatalf("err = %v, want it to contain mock failure", err)
	}
	if got != nil {
		t.Fatalf("got %#v, want nil", got)
	}
}
