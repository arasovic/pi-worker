package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	o, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, o)
	}
	return strings.TrimSpace(string(o))
}

func gitRefExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// newTempRepo creates a one-commit repo, chdirs into it, returns
// the symlink-resolved root (macOS /var → /private/var).
func newTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
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
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	return r
}

func withRunGitFunc(t *testing.T, fn func(context.Context, string, ...string) (string, error)) {
	t.Helper()
	orig := runGitFunc
	runGitFunc = fn
	t.Cleanup(func() { runGitFunc = orig })
}

func TestValidName(t *testing.T) {
	accepted := []string{"a", "z", "0", "9", "abc", "run-2", "a-b-c",
		strings.Repeat("x", 64)}
	for _, n := range accepted {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	rejected := []struct{ n, w string }{
		{"", "empty"}, {"Foo", "upper"}, {"a/b", "slash"}, {"a.b", "dot"},
		{"a b", "space"}, {"a_b", "under"}, {"-x", "lead-hyphen"},
		{"x-", "trail-hyphen"}, {strings.Repeat("x", 65), "long"},
		{"../x", "traversal"}, {"a..b", "dots"}, {"a//b", "slashes"},
		{"a\\b", "backslash"}, {"a@b", "at"}, {"a+b", "plus"},
	}
	for _, tc := range rejected {
		if ValidName(tc.n) {
			t.Errorf("ValidName(%q) (%s) = true, want false", tc.n, tc.w)
		}
	}
}

func TestInvalidNameRefusedBeforeGit(t *testing.T) {
	var calls int
	withRunGitFunc(t, func(_ context.Context, _ string, _ ...string) (string, error) {
		calls++
		return "", nil
	})
	_, err := Prepare(context.Background(), ".", "Foo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsRefusal(err) {
		t.Fatalf("err = %v, want refusal", err)
	}
	want := `invalid worktree name "Foo": use 1 to 64 characters of lowercase letters, digits and hyphens, starting and ending with a letter or digit`
	if err.Error() != want {
		t.Fatalf("err = %q, want exact message", err.Error())
	}
	if calls != 0 {
		t.Fatalf("runGitFunc called %d times, want 0", calls)
	}
}

func TestPrepareFullRoundTrip(t *testing.T) {
	root := newTempRepo(t)
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	head := gitRun(t, root, "rev-parse", "HEAD")
	got, err := Prepare(context.Background(), sub, "probe")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Name != "probe" {
		t.Fatalf("Name = %q, want probe", got.Name)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "probe")
	if got.Path != wantPath {
		t.Fatalf("Path = %q, want %q", got.Path, wantPath)
	}
	if got.Branch != "run/probe" {
		t.Fatalf("Branch = %q, want run/probe", got.Branch)
	}
	if got.Head != head {
		t.Fatalf("Head = %q, want %q", got.Head, head)
	}
	if b := gitRun(t, wantPath, "rev-parse", "--abbrev-ref", "HEAD"); b != "run/probe" {
		t.Fatalf("checkout branch = %q, want run/probe", b)
	}
	if h := gitRun(t, wantPath, "rev-parse", "HEAD"); h != head {
		t.Fatalf("checkout HEAD = %q, want %q", h, head)
	}
	if c, e := os.ReadFile(filepath.Join(wantPath, "file.txt")); e != nil || string(c) != "one\n" {
		t.Fatalf("checkout file.txt = %q, %v; want committed content", c, e)
	}
	if !gitRefExists(t, root, "refs/heads/run/probe") {
		t.Fatal("branch run/probe not created")
	}
}

func TestPrepareExcludesUncommittedChanges(t *testing.T) {
	root := newTempRepo(t)
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}
	got, err := Prepare(context.Background(), root, "clean")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, e := os.Stat(filepath.Join(got.Path, "dirty.txt")); e == nil {
		t.Fatal("uncommitted dirty.txt leaked into worktree")
	}
}

func TestPrepareExistingPathRefuses(t *testing.T) {
	root := newTempRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".pi-worker", "worktrees", "taken"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	refs := gitRun(t, root, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	_, err := Prepare(context.Background(), root, "taken")
	if err == nil || !IsRefusal(err) {
		t.Fatalf("err = %v, want refusal", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %q", err.Error())
	}
	if gitRefExists(t, root, "refs/heads/run/taken") {
		t.Fatal("branch created despite refusal")
	}
	if after := gitRun(t, root, "for-each-ref", "--format=%(refname:short)", "refs/heads"); after != refs {
		t.Fatalf("branches changed: %q → %q", refs, after)
	}
}

func TestExistingPathRefusedBeforeHEADResolution(t *testing.T) {
	root := newTempRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".pi-worker", "worktrees", "taken"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var calls []string
	withRunGitFunc(t, func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return root, nil
	})
	_, err := Prepare(context.Background(), root, "taken")
	if err == nil || !IsRefusal(err) {
		t.Fatalf("err = %v, want refusal", err)
	}
	for _, c := range calls {
		if c == "rev-parse HEAD" {
			t.Fatalf("HEAD resolved before path check; calls = %v", calls)
		}
	}
}

func TestPrepareExistingBranchRefuses(t *testing.T) {
	root := newTempRepo(t)
	gitRun(t, root, "branch", "run/taken")
	snap := gitRun(t, root, "status", "--porcelain=v1")
	_, err := Prepare(context.Background(), root, "taken")
	if err == nil || !IsRefusal(err) {
		t.Fatalf("err = %v, want refusal", err)
	}
	if !strings.Contains(err.Error(), "branch run/taken already exists") {
		t.Fatalf("err = %q", err.Error())
	}
	if _, e := os.Stat(filepath.Join(root, ".pi-worker", "worktrees", "taken")); e == nil {
		t.Fatal("worktree dir created despite refusal")
	}
	if after := gitRun(t, root, "status", "--porcelain=v1"); after != snap {
		t.Fatalf("workdir changed: %q → %q", snap, after)
	}
}

func TestPrepareOutsideGitRefuses(t *testing.T) {
	plain := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(plain); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Errorf("cleanup chdir: %v", err)
		}
	})
	_, err = Prepare(context.Background(), plain, "x")
	if err == nil || !IsRefusal(err) {
		t.Fatalf("err = %v, want refusal", err)
	}
	want := "--worktree requires the current directory to be inside a git work tree"
	if err.Error() != want {
		t.Fatalf("err = %q, want exact message %q", err.Error(), want)
	}
}

func TestPrepareContextNotRefusal(t *testing.T) {
	newTempRepo(t)
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
		want error
	}{
		{"cancelled", func() context.Context {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			return c
		}, context.Canceled},
		{"expired", func() context.Context {
			c, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
			defer cancel()
			return c
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Prepare(tc.ctx(), ".", "x")
			if err == nil {
				t.Fatal("expected error")
			}
			if IsRefusal(err) {
				t.Fatalf("%s must not be a refusal: %v", tc.name, err)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPrepareResolvesHeadOnceAndPassesHash(t *testing.T) {
	root := newTempRepo(t)
	fake := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var calls []string
	withRunGitFunc(t, func(_ context.Context, _ string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		calls = append(calls, cmd)
		switch {
		case cmd == "rev-parse --show-toplevel":
			return root, nil
		case cmd == "rev-parse HEAD":
			return fake, nil
		case cmd == "rev-parse --verify --quiet refs/heads/run/inspect":
			return "", fmt.Errorf("not found")
		case strings.HasPrefix(cmd, "worktree add "):
			parts := strings.Split(cmd, " ")
			if got := parts[len(parts)-1]; got != fake {
				t.Errorf("worktree add last arg = %q, want %q", got, fake)
			}
			return "", nil
		default:
			t.Fatalf("unexpected git call: %q", cmd)
			return "", nil
		}
	})
	got, err := Prepare(context.Background(), root, "inspect")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Head != fake {
		t.Fatalf("Head = %q, want %q", got.Head, fake)
	}
	n := 0
	for _, c := range calls {
		if c == "rev-parse HEAD" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("HEAD resolved %d times, want 1 (calls: %v)", n, calls)
	}
}
