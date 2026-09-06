package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRemoveWorktreeRemoveFailureReturnsError verifies that when
// injected git worktree remove fails, the error is returned, branch
// deletion is never attempted, and both checkout and branch remain.
func TestRemoveWorktreeRemoveFailureReturnsError(t *testing.T) {
	root := newTempRepo(t)
	ent := prepareCleanMerged(t, root, "alpha")

	var mu sync.Mutex
	var calls []string
	withRunGitFunc(t, func(_ context.Context, dir string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		mu.Lock()
		calls = append(calls, cmd)
		mu.Unlock()
		// Let real git handle List()-related calls.
		if strings.HasPrefix(cmd, "rev-parse") ||
			strings.HasPrefix(cmd, "worktree list") ||
			strings.HasPrefix(cmd, "for-each-ref") {
			return runGit(context.Background(), root, args...)
		}
		// status must run from the worktree path.
		if strings.HasPrefix(cmd, "status ") {
			return runGit(context.Background(), dir, args...)
		}
		if cmd == "worktree remove "+ent.Path {
			return "", fmt.Errorf("injected remove failure")
		}
		t.Fatalf("unexpected git call after remove failure: %q", cmd)
		return "", nil
	})

	err := Remove(context.Background(), root, ent)
	if err == nil || !strings.Contains(err.Error(), "injected remove failure") {
		t.Fatalf("err = %v, want injected remove failure", err)
	}

	// Checkout and branch must remain.
	if _, err := os.Stat(ent.Path); err != nil {
		t.Fatalf("checkout gone after failed remove: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+ent.Branch) {
		t.Fatal("branch deleted despite failed worktree remove")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.Contains(c, "branch -d") {
			t.Fatalf("branch -d called despite failed remove; calls = %v", calls)
		}
	}
}

// TestRemoveBranchDeleteFailureRestores verifies that when worktree
// remove succeeds but branch -d fails, exactly one restoration with
// the exact path and branch is attempted. The restoration uses a live
// bounded context even if the caller context is cancelled. The pair
// is left restored.
func TestRemoveBranchDeleteFailureRestores(t *testing.T) {
	root := newTempRepo(t)
	ent := prepareCleanMerged(t, root, "alpha")

	// Cancel caller context inside the branch-delete mock to prove
	// restoration uses a live bounded context derived from WithoutCancel.
	var mu sync.Mutex
	var restoreCalls []string
	var restoreCtxLive bool
	var restoreCtxHasDeadline bool
	ctx, cancel := context.WithCancel(context.Background())
	withRunGitFunc(t, func(ctx context.Context, dir string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		// Let real git handle List()-related calls.
		if strings.HasPrefix(cmd, "rev-parse") ||
			strings.HasPrefix(cmd, "worktree list") ||
			strings.HasPrefix(cmd, "for-each-ref") {
			return runGit(context.Background(), root, args...)
		}
		if strings.HasPrefix(cmd, "status ") {
			return runGit(context.Background(), dir, args...)
		}
		if cmd == "worktree remove "+ent.Path {
			return runGit(context.Background(), root, args...)
		}
		mu.Lock()
		if cmd == "branch -d "+ent.Branch {
			restoreCalls = append(restoreCalls, cmd)
			mu.Unlock()
			// Cancel the caller context right here.
			cancel()
			return "", fmt.Errorf("injected branch-delete failure")
		}
		if cmd == "worktree add "+ent.Path+" "+ent.Branch {
			restoreCalls = append(restoreCalls, cmd)
			restoreCtxLive = ctx.Err() == nil
			_, restoreCtxHasDeadline = ctx.Deadline()
			mu.Unlock()
			return runGit(context.Background(), root, args...)
		}
		mu.Unlock()
		t.Fatalf("unexpected git call: %q", cmd)
		return "", nil
	})

	err := Remove(ctx, root, ent)
	if err == nil {
		t.Fatal("expected error for branch-delete failure")
	}
	if !strings.Contains(err.Error(), "injected branch-delete failure") {
		t.Fatalf("err = %v, want branch-delete failure", err)
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Fatalf("err = %v, want restoration acknowledgement", err)
	}

	// Pair must be restored.
	if _, err := os.Stat(ent.Path); err != nil {
		t.Fatalf("checkout missing after restore: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+ent.Branch) {
		t.Fatal("branch missing after restore")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(restoreCalls) != 2 {
		t.Fatalf("restore calls = %v, want branch -d + worktree add", restoreCalls)
	}

	// Verify the context was live at the time of the restoration call.
	if !restoreCtxLive {
		t.Fatal("restore context was not live at call time")
	}
	if !restoreCtxHasDeadline {
		t.Fatal("restore context had no deadline at call time")
	}
}

// TestRemoveBranchDeleteAndRestoreBothFail verifies that when both
// branch -d and the subsequent worktree add fail, both errors are
// reported and the error does not claim restoration.
func TestRemoveBranchDeleteAndRestoreBothFail(t *testing.T) {
	root := newTempRepo(t)
	ent := prepareCleanMerged(t, root, "alpha")

	withRunGitFunc(t, func(_ context.Context, dir string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		// Let real git handle List()-related calls.
		if strings.HasPrefix(cmd, "rev-parse") ||
			strings.HasPrefix(cmd, "worktree list") ||
			strings.HasPrefix(cmd, "for-each-ref") {
			return runGit(context.Background(), root, args...)
		}
		if strings.HasPrefix(cmd, "status ") {
			return runGit(context.Background(), dir, args...)
		}
		if cmd == "worktree remove "+ent.Path {
			return runGit(context.Background(), root, args...)
		}
		if cmd == "branch -d "+ent.Branch {
			return "", fmt.Errorf("branch-delete error")
		}
		if cmd == "worktree add "+ent.Path+" "+ent.Branch {
			return "", fmt.Errorf("restore error")
		}
		t.Fatalf("unexpected git call: %q", cmd)
		return "", nil
	})

	err := Remove(context.Background(), root, ent)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "branch-delete error") {
		t.Fatalf("err = %v, want branch-delete error", err)
	}
	if !strings.Contains(msg, "restore") {
		t.Fatalf("err = %v, want restore error", err)
	}
	if strings.Contains(msg, "restored") {
		t.Fatalf("err = %v, must not claim restoration when restore failed", err)
	}
}

// TestRemoveListFailurePreventsCommands verifies that a List failure
// prevents any remove or branch-delete command.
func TestRemoveListFailurePreventsCommands(t *testing.T) {
	ent := Entry{Name: "alpha", Path: "/x", Branch: "run/alpha", Merged: true}

	var mu sync.Mutex
	var calls []string
	withRunGitFunc(t, func(_ context.Context, _ string, args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		cmd := strings.Join(args, " ")
		calls = append(calls, cmd)
		if cmd == "rev-parse --show-toplevel" {
			return t.TempDir(), nil
		}
		return "", fmt.Errorf("injected list failure")
	})

	err := Remove(context.Background(), ".", ent)
	if err == nil || !strings.Contains(err.Error(), "injected list failure") {
		t.Fatalf("err = %v, want injected list failure", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.Contains(c, "worktree remove") || strings.Contains(c, "branch -d") {
			t.Fatalf("mutation attempted before List success; calls = %v", calls)
		}
	}
}

// TestRemoveMalformedListPreventsCommands verifies that a List result
// that finds no matching entry returns retry and prevents any remove
// or branch-delete command.
func TestRemoveMalformedListPreventsCommands(t *testing.T) {
	root := newTempRepo(t)
	ent := Entry{Name: "alpha", Path: filepath.Join(root, ".pi-worker", "worktrees", "alpha"), Branch: "run/alpha", Merged: true}

	var mu sync.Mutex
	var calls []string
	withRunGitFunc(t, func(_ context.Context, dir string, args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		cmd := strings.Join(args, " ")
		calls = append(calls, cmd)
		// Let real git handle List() calls.
		if strings.HasPrefix(cmd, "rev-parse") ||
			strings.HasPrefix(cmd, "worktree list") ||
			strings.HasPrefix(cmd, "for-each-ref") {
			return runGit(context.Background(), root, args...)
		}
		if strings.HasPrefix(cmd, "status ") {
			return runGit(context.Background(), dir, args...)
		}
		return "", fmt.Errorf("unexpected call after List: %q", cmd)
	})

	err := Remove(context.Background(), root, ent)
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want retry for missing entry", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.Contains(c, "worktree remove") || strings.Contains(c, "branch -d") {
			t.Fatalf("mutation attempted despite missing entry; calls = %v", calls)
		}
	}
}
