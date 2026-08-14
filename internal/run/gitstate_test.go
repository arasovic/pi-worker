package run

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
)

// runGit executes one git command in dir and fails the test on any
// error, returning combined output.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// isolateGitConfig keeps temporary-repository tests independent of the
// host user's git configuration and commit-signing setup.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
}

// newGitRepo creates an isolated repository in a fresh temporary
// directory with one committed file and returns the directory.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	isolateGitConfig(t)
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@pi-worker")
	runGit(t, dir, "config", "user.name", "pi-worker test")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, dir, "add", "file.txt")
	runGit(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func TestDefaultGitInspectorNotARepositoryReturnsNil(t *testing.T) {
	state, err := NewDefaultGitInspector().Inspect(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state != nil {
		t.Fatalf("state = %#v, want nil outside a git work tree", state)
	}
}

func TestDefaultGitInspectorCleanRepository(t *testing.T) {
	dir := newGitRepo(t)
	state, err := NewDefaultGitInspector().Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state == nil {
		t.Fatalf("inspect returned nil inside a git work tree")
	}
	if want := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")); state.Head != want {
		t.Fatalf("head = %q, want %q", state.Head, want)
	}
	if want := strings.TrimSpace(runGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD")); state.Branch != want || state.Branch == "" {
		t.Fatalf("branch = %q, want %q", state.Branch, want)
	}
	if state.Dirty {
		t.Fatalf("clean repository reported dirty")
	}
	if state.Stashes != 0 {
		t.Fatalf("stashes = %d, want 0", state.Stashes)
	}
}

func TestDefaultGitInspectorDirtyWorkingTree(t *testing.T) {
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	state, err := NewDefaultGitInspector().Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state == nil || !state.Dirty {
		t.Fatalf("modified working tree not reported dirty: %#v", state)
	}
}

func TestDefaultGitInspectorUnbornBranchLeavesHeadEmpty(t *testing.T) {
	dir := t.TempDir()
	isolateGitConfig(t)
	runGit(t, dir, "init", "-q")
	state, err := NewDefaultGitInspector().Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state == nil {
		t.Fatalf("inspect returned nil inside a git work tree")
	}
	if state.Head != "" {
		t.Fatalf("head = %q, want empty on an unborn branch", state.Head)
	}
	// An unborn branch never reports HEAD as a branch name; newer git
	// versions leave Branch empty too, older ones report the default
	// branch name.
	if state.Branch == "HEAD" {
		t.Fatalf("branch = %q, want empty (HEAD is unborn)", state.Branch)
	}
	if state.Dirty {
		t.Fatalf("empty unborn repository reported dirty")
	}
	if state.Stashes != 0 {
		t.Fatalf("stashes = %d, want 0", state.Stashes)
	}
}

func TestDefaultGitInspectorDetachedHEADLeavesBranchEmpty(t *testing.T) {
	dir := newGitRepo(t)
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "checkout", "-q", "--detach", head)
	state, err := NewDefaultGitInspector().Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state == nil {
		t.Fatalf("inspect returned nil inside a git work tree")
	}
	if state.Head != head {
		t.Fatalf("head = %q, want %q", state.Head, head)
	}
	if state.Branch != "" {
		t.Fatalf("branch = %q, want empty when detached", state.Branch)
	}
}

func TestDefaultGitInspectorCountsStashEntries(t *testing.T) {
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, dir, "stash", "push", "-q", "-m", "probe")
	state, err := NewDefaultGitInspector().Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state == nil || state.Stashes != 1 {
		t.Fatalf("stashes = %#v, want 1 after one stash push", state)
	}
	runGit(t, dir, "stash", "drop", "-q")
	state, err = NewDefaultGitInspector().Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("inspect after drop: %v", err)
	}
	if state == nil || state.Stashes != 0 {
		t.Fatalf("stashes = %#v, want 0 after a stash drop", state)
	}
}

// scriptedGitInspector is a concurrency-safe fake GitInspector for
// controller tests. It returns before on the first Inspect call and
// after on the second, records every invocation with its context and
// directory, and can be configured to fail either call.
type scriptedGitInspector struct {
	mu     sync.Mutex
	ctxs   []context.Context
	dirs   []string
	before *GitState
	after  *GitState
	errs   []error
}

func (g *scriptedGitInspector) Inspect(ctx context.Context, dir string) (*GitState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ctxs = append(g.ctxs, ctx)
	g.dirs = append(g.dirs, dir)
	call := len(g.ctxs) - 1
	if call < len(g.errs) && g.errs[call] != nil {
		return nil, g.errs[call]
	}
	if call == 0 {
		return g.before, nil
	}
	return g.after, nil
}

func (g *scriptedGitInspector) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.ctxs)
}

func (g *scriptedGitInspector) contextsSeen() []context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]context.Context(nil), g.ctxs...)
}

func (g *scriptedGitInspector) dirsSeen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.dirs...)
}

func TestControllerGitNotARepositoryLeavesGitNil(t *testing.T) {
	worker := newScriptedWorker()
	inspector := &scriptedGitInspector{}
	req := validRequest("a", "b")
	req.Workspace = t.TempDir()
	result, err := New(worker, WithGitInspector(inspector)).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if result.Git != nil {
		t.Fatalf("non-git workspace carried git state: %#v", result.Git)
	}
	if len(result.Workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(result.Workers))
	}
	if worker.callCount() != 2 {
		t.Fatalf("worker invoked %d times, want 2", worker.callCount())
	}
	// A nil before state skips the after state entirely.
	if inspector.callCount() != 1 {
		t.Fatalf("inspect ran %d times, want once for a non-git workspace", inspector.callCount())
	}
}

func TestControllerGitNilWhenOnlyDirtyChanged(t *testing.T) {
	worker := newScriptedWorker()
	inspector := &scriptedGitInspector{
		before: &GitState{Head: "same", Branch: "main", Stashes: 0},
		after:  &GitState{Head: "same", Branch: "main", Dirty: true, Stashes: 0},
	}
	result, err := New(worker, WithGitInspector(inspector)).Run(context.Background(), validRequest("a"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Git != nil {
		t.Fatalf("dirty-only change carried git state: %#v", result.Git)
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
}

func TestControllerGitReportsStateMoves(t *testing.T) {
	tests := []struct {
		name   string
		before *GitState
		after  *GitState
	}{
		{
			name:   "head moved",
			before: &GitState{Head: "old", Branch: "main", Stashes: 0},
			after:  &GitState{Head: "new", Branch: "main", Stashes: 0},
		},
		{
			name:   "branch changed",
			before: &GitState{Head: "same", Branch: "main", Stashes: 0},
			after:  &GitState{Head: "same", Branch: "feature", Stashes: 0},
		},
		{
			name:   "stash count changed",
			before: &GitState{Head: "same", Branch: "main", Stashes: 1},
			after:  &GitState{Head: "same", Branch: "main", Stashes: 0},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newScriptedWorker()
			inspector := &scriptedGitInspector{before: test.before, after: test.after}
			result, err := New(worker, WithGitInspector(inspector)).Run(context.Background(), validRequest("a"))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.Git == nil {
				t.Fatalf("state move carried no git change")
			}
			if result.Git.Before.Head != test.before.Head || result.Git.After.Head != test.after.Head ||
				result.Git.Before.Branch != test.before.Branch || result.Git.After.Branch != test.after.Branch ||
				result.Git.Before.Stashes != test.before.Stashes || result.Git.After.Stashes != test.after.Stashes {
				t.Fatalf("git change = %#v, want before %#v after %#v", result.Git, test.before, test.after)
			}
			if result.Git.Before.Dirty || result.Git.After.Dirty {
				t.Fatalf("git change carried dirty flags that were not set: %#v", result.Git)
			}
			if inspector.callCount() != 2 {
				t.Fatalf("inspect ran %d times, want before and after", inspector.callCount())
			}
			if dirs := inspector.dirsSeen(); len(dirs) != 2 || dirs[0] != "/workspace" || dirs[1] != "/workspace" {
				t.Fatalf("inspection dirs = %v, want the workspace twice", dirs)
			}
		})
	}
}

func TestControllerGitAfterStateCollectedOnExpiredContext(t *testing.T) {
	// A run that timed out mid-checkout is exactly the run whose git
	// state a caller most needs: the after state must still be
	// collected, under a fresh context independent of the expired parent.
	worker := newScriptedWorker()
	worker.ignoreCtx = true
	inspector := &scriptedGitInspector{
		before: &GitState{Head: "same", Branch: "main", Stashes: 1},
		after:  &GitState{Head: "same", Branch: "main", Stashes: 0},
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result, err := New(worker, WithGitInspector(inspector)).Run(ctx, validRequest("a", "b"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunTimedOut {
		t.Fatalf("status = %q, want timed-out", result.Status)
	}
	if result.Git == nil || result.Git.After.Stashes != 0 || result.Git.Before.Stashes != 1 {
		t.Fatalf("expired run lost its after state: %#v", result.Git)
	}
	ctxs := inspector.contextsSeen()
	if len(ctxs) != 2 {
		t.Fatalf("inspect ran %d times, want before and after", len(ctxs))
	}
	deadline, ok := ctxs[1].Deadline()
	if !ok {
		t.Fatalf("after inspection has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("after inspection deadline is %v away, want a fresh 5s budget", remaining)
	}
}

func TestControllerGitAfterStateContextIsLiveDespiteDeadParent(t *testing.T) {
	// The after state runs under a fresh context by construction, not by
	// checking the parent first: an inspector that observes the context
	// it is handed must see Err() == nil even when the parent context
	// was already cancelled or expired before Run returned. The
	// cancelled-during-inspection case is the race the fresh-context
	// rule exists for — a deadline expiring exactly as the after state
	// starts.
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	cancelled, cancelCancelled := context.WithCancel(context.Background())
	cancelCancelled()
	midRun, cancelMidRun := context.WithCancel(context.Background())
	defer cancelMidRun()
	for _, test := range []struct {
		name   string
		ctx    context.Context
		cancel context.CancelFunc
	}{
		{name: "expired parent", ctx: expired},
		{name: "cancelled parent", ctx: cancelled},
		{name: "parent cancelled during after-state inspection", ctx: midRun, cancel: cancelMidRun},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker := newScriptedWorker()
			worker.ignoreCtx = true
			inspector := &liveAfterContextInspector{
				scripted: &scriptedGitInspector{
					before: &GitState{Head: "same", Branch: "main", Stashes: 1},
					after:  &GitState{Head: "same", Branch: "main", Stashes: 0},
				},
				cancel: test.cancel,
			}
			result, err := New(worker, WithGitInspector(inspector)).Run(test.ctx, validRequest("a"))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.Git == nil {
				t.Fatalf("lost the after state on a %s", test.name)
			}
			if inspector.afterErr != nil {
				t.Fatalf("after inspection context was done when handed to the inspector: %v", inspector.afterErr)
			}
		})
	}
}

// liveAfterContextInspector wraps scriptedGitInspector and records
// whether the context handed to the after-state inspection was live at
// that moment. The optional cancel fires exactly then, simulating a
// parent deadline that expires while the after state starts; the
// observation is made after that cancel, which is the point of the
// fresh-context rule.
type liveAfterContextInspector struct {
	scripted *scriptedGitInspector
	cancel   context.CancelFunc
	afterErr error
	calls    int
}

func (l *liveAfterContextInspector) Inspect(ctx context.Context, dir string) (*GitState, error) {
	l.calls++
	if l.calls == 2 {
		if l.cancel != nil {
			l.cancel()
		}
		l.afterErr = ctx.Err()
	}
	return l.scripted.Inspect(ctx, dir)
}

func TestControllerGitInspectorErrorLeavesGitNil(t *testing.T) {
	worker := newScriptedWorker()
	inspector := &scriptedGitInspector{
		before: &GitState{Head: "a", Branch: "main", Stashes: 1},
		after:  &GitState{Head: "b", Branch: "main", Stashes: 1},
		errs:   []error{nil, errors.New("git failure")},
	}
	result, err := New(worker, WithGitInspector(inspector)).Run(context.Background(), validRequest("a"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Git != nil {
		t.Fatalf("inspector error carried git state: %#v", result.Git)
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if len(result.Workers) != 1 || result.Workers[0].Status != pi.StatusCompleted {
		t.Fatalf("workers = %#v, want one completed worker", result.Workers)
	}
	if worker.callCount() != 1 {
		t.Fatalf("worker invoked %d times, want 1", worker.callCount())
	}
}

func TestControllerGitBeforeErrorSkipsAfterState(t *testing.T) {
	worker := newScriptedWorker()
	inspector := &scriptedGitInspector{
		before: &GitState{Head: "a", Branch: "main"},
		errs:   []error{errors.New("git failure")},
	}
	result, err := New(worker, WithGitInspector(inspector)).Run(context.Background(), validRequest("a"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Git != nil {
		t.Fatalf("before error carried git state: %#v", result.Git)
	}
	if inspector.callCount() != 1 {
		t.Fatalf("inspect ran %d times, want once when the before state fails", inspector.callCount())
	}
}
