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

// TestRemoveUntouchedWorktreeRemoveFailure verifies that a worktree
// remove failure after all validation returns the error, never calls
// update-ref, and leaves checkout and branch intact.
func TestRemoveUntouchedWorktreeRemoveFailure(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "alpha")
	wantBranch := "run/alpha"

	var mu sync.Mutex
	var calls []string
	withRunGitFunc(t, func(ctx context.Context, dir string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		mu.Lock()
		calls = append(calls, cmd)
		mu.Unlock()
		// Let real git handle all validation: rev-parse, List(), checkout HEAD.
		if strings.HasPrefix(cmd, "rev-parse") ||
			strings.HasPrefix(cmd, "worktree list") ||
			strings.HasPrefix(cmd, "for-each-ref") {
			return runGit(context.Background(), root, args...)
		}
		if strings.HasPrefix(cmd, "status ") {
			return runGit(context.Background(), dir, args...)
		}
		// Inject failure at the worktree remove step.
		if cmd == "worktree remove "+wantPath {
			return "", fmt.Errorf("injected worktree remove failure")
		}
		t.Fatalf("unexpected git call after injected failure: %q", cmd)
		return "", nil
	})

	err = RemoveUntouched(context.Background(), root, prep)
	if err == nil || !strings.Contains(err.Error(), "injected worktree remove failure") {
		t.Fatalf("err = %v, want injected worktree remove failure", err)
	}

	// Checkout and branch must remain.
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("checkout gone after failed worktree remove: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+wantBranch) {
		t.Fatal("branch deleted despite failed worktree remove")
	}

	// update-ref must never have been called.
	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.Contains(c, "update-ref") {
			t.Fatalf("update-ref called despite failed worktree remove; calls = %v", calls)
		}
	}
}

// TestRemoveUntouchedUpdateRefFailureRestores verifies that a
// successful worktree removal followed by an update-ref failure
// (as if the branch moved) triggers one restoration. The command
// contains the exact full ref and recorded old hash, no branch -D is
// used, and the pair is restored.
func TestRemoveUntouchedUpdateRefFailureRestores(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "alpha")
	wantBranch := "run/alpha"
	branchRef := "refs/heads/" + wantBranch

	// Cancel the caller context inside the update-ref mock to prove
	// restoration uses a live bounded context from WithoutCancel.
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	var calls []string
	var restoreCalls []string
	var restoreCtxLive bool
	var restoreCtxHasDeadline bool

	withRunGitFunc(t, func(callCtx context.Context, dir string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		// Let real git handle validation: rev-parse, List(), checkout HEAD.
		if strings.HasPrefix(cmd, "rev-parse") ||
			strings.HasPrefix(cmd, "worktree list") ||
			strings.HasPrefix(cmd, "for-each-ref") {
			return runGit(context.Background(), root, args...)
		}
		if strings.HasPrefix(cmd, "status ") {
			return runGit(context.Background(), dir, args...)
		}
		// Let real git handle worktree removal.
		if cmd == "worktree remove "+wantPath {
			return runGit(context.Background(), root, args...)
		}
		mu.Lock()
		calls = append(calls, cmd)
		mu.Unlock()
		// Inject failure at update-ref.
		if cmd == "update-ref -d "+branchRef+" "+prep.Head {
			cancel() // Cancel caller context during update-ref.
			return "", fmt.Errorf("injected update-ref failure: branch moved")
		}
		// Capture exactly one restoration call.
		if cmd == "worktree add "+wantPath+" "+wantBranch {
			mu.Lock()
			restoreCalls = append(restoreCalls, cmd)
			restoreCtxLive = callCtx.Err() == nil
			_, restoreCtxHasDeadline = callCtx.Deadline()
			mu.Unlock()
			return runGit(context.Background(), root, args...)
		}
		t.Fatalf("unexpected git call: %q", cmd)
		return "", nil
	})

	err = RemoveUntouched(ctx, root, prep)
	if err == nil {
		t.Fatal("expected error for update-ref failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "injected update-ref failure: branch moved") {
		t.Fatalf("err = %v, want update-ref failure message", err)
	}
	if !strings.Contains(msg, "restored") {
		t.Fatalf("err = %v, want restoration acknowledgement", err)
	}

	// Pair must be restored.
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("checkout missing after restore: %v", err)
	}
	if !gitRefExists(t, root, "refs/heads/"+wantBranch) {
		t.Fatal("branch missing after restore")
	}

	mu.Lock()
	defer mu.Unlock()

	// No branch -D command is used.
	for _, c := range calls {
		if strings.Contains(c, "branch -D") {
			t.Fatalf("branch -D used; calls = %v", calls)
		}
	}

	// Exactly one restoration.
	if len(restoreCalls) != 1 {
		t.Fatalf("restore calls = %v, want exactly one worktree add", restoreCalls)
	}
	if restoreCalls[0] != "worktree add "+wantPath+" "+wantBranch {
		t.Fatalf("restore call = %q, want %q", restoreCalls[0], "worktree add "+wantPath+" "+wantBranch)
	}

	// Restoration context is live and has a deadline at call time.
	if !restoreCtxLive {
		t.Fatal("restore context was not live at call time")
	}
	if !restoreCtxHasDeadline {
		t.Fatal("restore context had no deadline at call time")
	}
}

// TestRemoveUntouchedUpdateRefAndRestoreBothFail verifies that when
// both update-ref and the restoration worktree add fail, both errors
// are reported and the error does not claim restoration.
func TestRemoveUntouchedUpdateRefAndRestoreBothFail(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", "alpha")
	wantBranch := "run/alpha"
	branchRef := "refs/heads/" + wantBranch

	withRunGitFunc(t, func(_ context.Context, dir string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		if strings.HasPrefix(cmd, "rev-parse") ||
			strings.HasPrefix(cmd, "worktree list") ||
			strings.HasPrefix(cmd, "for-each-ref") {
			return runGit(context.Background(), root, args...)
		}
		if strings.HasPrefix(cmd, "status ") {
			return runGit(context.Background(), dir, args...)
		}
		if cmd == "worktree remove "+wantPath {
			return runGit(context.Background(), root, args...)
		}
		if cmd == "update-ref -d "+branchRef+" "+prep.Head {
			return "", fmt.Errorf("update-ref error")
		}
		if cmd == "worktree add "+wantPath+" "+wantBranch {
			return "", fmt.Errorf("restore error")
		}
		t.Fatalf("unexpected git call: %q", cmd)
		return "", nil
	})

	err = RemoveUntouched(context.Background(), root, prep)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "update-ref error") {
		t.Fatalf("err = %v, want update-ref error", err)
	}
	if !strings.Contains(msg, "restore error") {
		t.Fatalf("err = %v, want restore error", err)
	}
	if strings.Contains(msg, "restored") {
		t.Fatalf("err = %v, must not claim restoration when restore failed", err)
	}
}

// TestRemoveUntouchedBranchHEADMismatchReturnsRetry verifies that
// when the branch ref HEAD differs from the expected Head recorded at
// creation, RemoveUntouched returns retry with no worktree remove or
// update-ref command issued.
func TestRemoveUntouchedBranchHEADMismatchReturnsRetry(t *testing.T) {
	root := newTempRepo(t)
	prep, err := Prepare(context.Background(), root, "alpha")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wantBranch := "run/alpha"
	branchRef := "refs/heads/" + wantBranch

	// Advance the branch by committing in the worktree so branch HEAD
	// no longer equals the recorded Head. The worktree checkout itself
	// stays at the recorded Head.
	if err := os.WriteFile(filepath.Join(prep.Path, "adv.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, prep.Path, "add", "adv.txt")
	gitRun(t, prep.Path, "commit", "-q", "-m", "advance")

	// Record actual branch HEAD which now differs from prep.Head.
	actualBranchHead := gitRun(t, root, "rev-parse", "--verify", "--quiet", branchRef)
	if actualBranchHead == prep.Head {
		t.Fatalf("branch HEAD did not move: both are %s", prep.Head)
	}

	var mu sync.Mutex
	var calls []string
	withRunGitFunc(t, func(ctx context.Context, dir string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		mu.Lock()
		calls = append(calls, cmd)
		mu.Unlock()
		// Route rev-parse by directory: from root use real git for
		// branch HEAD (returns moved hash), from worktree path
		// return the recorded Head so checkout validation passes.
		if strings.HasPrefix(cmd, "rev-parse") {
			if dir == root {
				return runGit(context.Background(), root, args...)
			}
			return prep.Head, nil
		}
		// Let real git handle List() and status.
		if strings.HasPrefix(cmd, "worktree list") ||
			strings.HasPrefix(cmd, "for-each-ref") {
			return runGit(context.Background(), root, args...)
		}
		if strings.HasPrefix(cmd, "status ") {
			return runGit(context.Background(), dir, args...)
		}
		t.Fatalf("unexpected git call after branch HEAD check: %q", cmd)
		return "", nil
	})

	err = RemoveUntouched(context.Background(), root, prep)
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %v, want retry error", err)
	}

	// No mutation commands issued.
	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.Contains(c, "worktree remove") {
			t.Fatalf("worktree remove called despite branch HEAD mismatch; calls = %v", calls)
		}
		if strings.Contains(c, "update-ref") {
			t.Fatalf("update-ref called despite branch HEAD mismatch; calls = %v", calls)
		}
	}
}
