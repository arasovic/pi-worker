package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
)

// gitRun executes one git command in dir and fails the test on any
// error, returning the trimmed output. Tests use it to inspect the
// checkouts the product created; it never changes repository state.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// gitRefExists reports whether a ref exists in dir's repository. A
// non-zero exit is the normal "missing ref" answer for rev-parse
// --verify --quiet, not a test failure.
func gitRefExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// canonicalRepo resolves newGitWorkspace's directory to the path git
// itself reports: on macOS the /var temp root is a symlink, git
// resolves it to /private/var, and the product's checkout paths come
// from git's resolution. The expected path is then built literally
// with filepath.Join on the canonical root.
func canonicalRepo(t *testing.T, repo string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	return resolved
}

// chdirPlain makes a fresh temporary directory outside any repository
// the process working directory for the duration of the test, the
// non-git sibling of newGitWorkspace: --worktree must refuse it.
func chdirPlain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	return dir
}

// completedResult is the scripted worker answer for a successful run in
// these tests; the model matches what the CLI receives from --model.
func completedResult() pi.WorkerResult {
	return pi.WorkerResult{Model: "acme/model", Explanation: "done", Status: pi.StatusCompleted}
}

// TestParseRunArgsRejectsInvalidWorktreeNames pins the argv-time name
// rule: a bad name is a usage error like every other one, exiting 2
// with the rejection text and the usage block, before anything runs.
func TestParseRunArgsRejectsInvalidWorktreeNames(t *testing.T) {
	rejected := []string{
		"Foo",
		"a/b",
		"-x",
		"x-",
		"x.y",
		"",
		strings.Repeat("x", 65),
	}
	for _, name := range rejected {
		t.Run("rejected "+name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, []string{"run", "--worktree", name, "--task", "work"}, "")
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr %q)", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			want := "pi-worker: invalid worktree name \"" + name + "\": use 1 to 64 characters of lowercase letters, digits and hyphens, starting and ending with a letter or digit\n"
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
			}
			if !strings.Contains(stderr, "usage: pi-worker") {
				t.Fatalf("stderr = %q, want the usage block", stderr)
			}
		})
	}
}

// TestParseRunArgsAcceptsValidWorktreeNames pins the accepted spellings
// end to end: each name parses, the checkout is created, and the run
// completes.
func TestParseRunArgsAcceptsValidWorktreeNames(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	for _, name := range []string{"x", "version-compare", "run-2"} {
		t.Run("accepted "+name, func(t *testing.T) {
			installFakeWorker(t, completedResult())
			code, _, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", name}, "")
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			checkout := filepath.Join(repo, ".pi-worker", "worktrees", name)
			if _, err := os.Stat(checkout); err != nil {
				t.Fatalf("checkout %s: %v", checkout, err)
			}
		})
	}
}

// TestRunWorktreeCreatesPrivateCheckout pins the whole shape of one
// worktree run: the worker is given the checkout, the checkout is a
// real git work tree on branch run/probe, and stderr carries the
// creation line verbatim — the only trace a killed run leaves, so it
// gets its own assertion (trap 6).
func TestRunWorktreeCreatesPrivateCheckout(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	fake := installFakeWorker(t, completedResult())
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	// The expected path is built literally in the test, never through
	// the product's own path helper.
	checkout := filepath.Join(repo, ".pi-worker", "worktrees", "probe")
	wantLine := "pi-worker: worktree " + checkout + " on branch run/probe\n"
	if stderr != wantLine {
		t.Fatalf("stderr = %q, want exactly %q", stderr, wantLine)
	}
	request, ok := fake.requestForWorker(1)
	if !ok {
		t.Fatalf("no request recorded for worker 1")
	}
	if request.Workspace != checkout {
		t.Fatalf("worker workspace = %q, want %q", request.Workspace, checkout)
	}
	// The directory is a real checkout of the source repo's HEAD, on
	// the branch the creation line named.
	if branch := gitRun(t, checkout, "rev-parse", "--abbrev-ref", "HEAD"); branch != "run/probe" {
		t.Fatalf("checkout branch = %q, want run/probe", branch)
	}
	if content, err := os.ReadFile(filepath.Join(checkout, "file.txt")); err != nil || string(content) != "one\n" {
		t.Fatalf("checkout file.txt = %q, %v; want the committed file from HEAD", content, err)
	}
	requireChangesTail(t, stdout, "worker 1: done\n")
}

// TestRunJSONWorktreeObject pins the document shape: a --worktree run's
// JSON result carries a worktree object with exactly the keys path and
// branch. The absence of the key without the flag is pinned by the
// existing exact-key assertion in json_contract_test, which must stay
// green unmodified.
func TestRunJSONWorktreeObject(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	installFakeWorker(t, completedResult())
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	worktree, ok := document["worktree"].(map[string]any)
	if !ok {
		t.Fatalf("worktree = %#v, want object", document["worktree"])
	}
	assertExactJSONKeys(t, worktree, "path", "branch")
	checkout := filepath.Join(repo, ".pi-worker", "worktrees", "probe")
	if worktree["path"] != checkout {
		t.Fatalf("worktree.path = %v, want %q", worktree["path"], checkout)
	}
	if worktree["branch"] != "run/probe" {
		t.Fatalf("worktree.branch = %v, want run/probe", worktree["branch"])
	}
}

// TestRunWorktreeRefusesTakenName pins the leftover-directory refusal:
// a second run with the same name exits 2, prints the refusal alone —
// no usage block, no JSON document — and never calls the worker. The
// refusal happens before the run record is written and before any
// worker starts, and it removes nothing: the checkout that took the
// name is still there afterwards.
func TestRunWorktreeRefusesTakenName(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	installFakeWorker(t, completedResult())
	if code, _, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, ""); code != 0 {
		t.Fatalf("first run exit = %d, stderr = %q", code, stderr)
	}
	// A fresh fake for the refused run, so the assertion below really
	// proves the refused run never called the worker.
	refused := installFakeWorker(t, completedResult())
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, "")
	if code != 2 {
		t.Fatalf("second run exit = %d, want 2 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no JSON document", stdout)
	}
	checkout := filepath.Join(repo, ".pi-worker", "worktrees", "probe")
	want := "pi-worker: worktree " + checkout + " already exists; collect it or choose another name\n"
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
	}
	if strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr = %q, want no usage block", stderr)
	}
	if refused.callCount() != 0 {
		t.Fatalf("worker calls = %d, want 0 for the refused run", refused.callCount())
	}
	// The refusal must not remove what it found.
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("checkout %s removed by the refusal: %v", checkout, err)
	}
}

// TestRunWorktreeRefusesLeftoverBranch pins the second leftover shape:
// the checkout directory removed but the branch kept, the run is still
// refused, naming the branch — written as the literal run/probe, never
// built from the product's own prefix — and the refusal removes
// nothing: the branch that took the name is still there afterwards.
func TestRunWorktreeRefusesLeftoverBranch(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	fake := installFakeWorker(t, completedResult())
	if code, _, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, ""); code != 0 {
		t.Fatalf("first run exit = %d, stderr = %q", code, stderr)
	}
	checkout := filepath.Join(repo, ".pi-worker", "worktrees", "probe")
	if err := os.RemoveAll(checkout); err != nil {
		t.Fatalf("remove checkout: %v", err)
	}
	if !gitRefExists(t, repo, "refs/heads/run/probe") {
		t.Fatalf("test setup: branch run/probe must still exist")
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, "")
	if code != 2 {
		t.Fatalf("second run exit = %d, want 2 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no JSON document", stdout)
	}
	want := "pi-worker: branch run/probe already exists; collect it or choose another name\n"
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
	}
	if fake.callCount() != 1 {
		t.Fatalf("worker calls = %d, want 1 (the first run only)", fake.callCount())
	}
	// The refusal must not remove what it found.
	if !gitRefExists(t, repo, "refs/heads/run/probe") {
		t.Fatalf("branch run/probe removed by the refusal")
	}
}

// TestRunWorktreeRefusesGitCollision pins the refusal for the states
// only git can see: a branch literally named "run" makes git refuse to
// create refs/heads/run/probe, neither pre-check names it, and the
// failed git worktree add must refuse the run exactly like the
// pre-checks do — exit 2, the message carrying git's own refusal text
// and no usage block, nothing on stdout, no worker, and no run record.
// The record-directory assertion is what pins that the refusal happens
// before the run record is written: nothing may claim the run started.
func TestRunWorktreeRefusesGitCollision(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	gitRun(t, repo, "branch", "run")
	fake := installFakeWorker(t, completedResult())
	logDir := t.TempDir()
	originalDir := runlogDir
	runlogDir = func() (string, error) { return logDir, nil }
	t.Cleanup(func() { runlogDir = originalDir })

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no JSON document", stdout)
	}
	// The message names the checkout and carries git's own words, so
	// the caller can see why the checkout could not be created.
	checkout := filepath.Join(repo, ".pi-worker", "worktrees", "probe")
	if !strings.Contains(stderr, "create worktree "+checkout) {
		t.Fatalf("stderr = %q, want it to name the checkout", stderr)
	}
	if !strings.Contains(stderr, "cannot lock ref 'refs/heads/run/probe'") {
		t.Fatalf("stderr = %q, want git's own refusal text", stderr)
	}
	if strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr = %q, want no usage block", stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker calls = %d, want 0 for the refused run", fake.callCount())
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("read record dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("record dir = %v, want empty: the refusal happens before any run record is written", entries)
	}
}

// TestPrepareWorktreeExpiredContextIsNotARefusal pins the one
// exception to prepareWorktree's refusals: a context already past its
// deadline or already cancelled makes the first git command fail, and
// that failure must come back carrying the context's own error — never
// as the "inside a git work tree" refusal, which would be untrue: the
// caller may well be inside a git work tree, the run just ended.
func TestPrepareWorktreeExpiredContextIsNotARefusal(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	tests := []struct {
		name  string
		ready func() context.Context
		want  error
	}{
		{
			name: "deadline already passed",
			ready: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
				defer cancel()
				return ctx
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "already cancelled",
			ready: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := prepareWorktree(test.ready(), repo, "probe")
			if err == nil {
				t.Fatalf("prepareWorktree returned nil error, want one carrying %v", test.want)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want errors.Is(err, %v) to hold", err, test.want)
			}
			if worktreeRefused(err) {
				t.Fatalf("err = %v, want no refusal: the run context ended, the caller cannot fix anything", err)
			}
		})
	}
}

// TestRunWorktreePreparationExitPrecedence pins the three observable exits
// from worktree preparation: a caller-fixable refusal is 2, an expired run
// is 7, and a cancelled run is 8. The cases exercise distinct preparation
// outcomes and confirm that each keeps its own exit classification.
func TestRunWorktreePreparationExitPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		context    func() context.Context
		wantCode   int
		wantOutput string
	}{
		{
			name: "refusal",
			context: func() context.Context {
				return context.Background()
			},
			wantCode:   2,
			wantOutput: "already exists; collect it or choose another name",
		},
		{
			name: "deadline",
			context: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
				defer cancel()
				return ctx
			},
			wantCode:   7,
			wantOutput: "resolve repository root",
		},
		{
			name: "cancellation",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantCode:   8,
			wantOutput: "resolve repository root",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := canonicalRepo(t, newGitWorkspace(t))
			if test.name == "refusal" {
				path := filepath.Join(repo, ".pi-worker", "worktrees", "probe")
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatalf("make leftover worktree directory: %v", err)
				}
			}
			fake := installFakeWorker(t, completedResult())
			code, stdout, stderr := runCLIWithContext(t, test.context(), []string{
				"run", "--model", "acme/model", "--task", "work", "--worktree", "probe",
			}, "")
			if code != test.wantCode {
				t.Fatalf("exit = %d, want %d; stderr = %q", code, test.wantCode, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, test.wantOutput) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, test.wantOutput)
			}
			if fake.callCount() != 0 {
				t.Fatalf("worker calls = %d, want 0 during preparation", fake.callCount())
			}
		})
	}
}

// TestRunWorktreeLeftBehind pins that nothing is ever removed: the
// checkout and its branch still exist after the run finished.
func TestRunWorktreeLeftBehind(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	installFakeWorker(t, completedResult())
	if code, _, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, ""); code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}
	checkout := filepath.Join(repo, ".pi-worker", "worktrees", "probe")
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("checkout %s gone after the run: %v", checkout, err)
	}
	if !gitRefExists(t, repo, "refs/heads/run/probe") {
		t.Fatalf("branch run/probe missing after the run")
	}
}

// TestRunWorktreeFailedRunKeepsCheckout pins that a failed run leaves
// its checkout behind exactly like a successful one: a worker that
// fails must not take the checkout or its branch with it, or the next
// run with the same name would silently get a fresh start instead of
// the refusal a leftover earns.
func TestRunWorktreeFailedRunKeepsCheckout(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	installFakeWorker(t, pi.WorkerResult{Model: "acme/model", Status: pi.StatusFailed, Error: "agent failed"})
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, "")
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (stderr %q)", code, stderr)
	}
	checkout := filepath.Join(repo, ".pi-worker", "worktrees", "probe")
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("checkout %s gone after the failed run: %v", checkout, err)
	}
	if !gitRefExists(t, repo, "refs/heads/run/probe") {
		t.Fatalf("branch run/probe missing after the failed run")
	}
}

// TestRunWorktreeRequiresGitWorkTree pins the outside-a-repository
// refusal: --worktree exits 2 with the work-tree message, alone, with
// no usage block and no JSON document.
func TestRunWorktreeRequiresGitWorkTree(t *testing.T) {
	chdirPlain(t)
	installFakeWorker(t, completedResult())
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--worktree", "probe"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no JSON document", stdout)
	}
	want := "pi-worker: --worktree requires the current directory to be inside a git work tree\n"
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
	}
	if strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr = %q, want no usage block", stderr)
	}
}
