package worktree

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// RemoveUntouched removes exactly the managed checkout and its local
// branch described by expected. Callers must supply a Prepared value
// they have independently proven was never started; the function
// itself verifies the worktree and branch are still untouched by
// re-inventorying, checking HEADs, and atomically compare-and-deleting
// the branch ref. It cannot know whether a worker started.
func RemoveUntouched(ctx context.Context, cwd string, expected Prepared) error {
	// Validate name before any Git call.
	if !ValidName(expected.Name) {
		return fmt.Errorf("invalid worktree name %q", expected.Name)
	}

	// Resolve the repository root from cwd.
	root, err := runGitFunc(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("resolve repository root: %w", ctxErr)
		}
		return fmt.Errorf("resolve repository root: %v", err)
	}

	// Compute exact managed path and branch; require expected identity.
	wantPath := filepath.Join(root, ".pi-worker", "worktrees", expected.Name)
	wantBranch := "run/" + expected.Name
	if expected.Path != wantPath {
		return fmt.Errorf("expected path %q does not match computed %q", expected.Path, wantPath)
	}
	if expected.Branch != wantBranch {
		return fmt.Errorf("expected branch %q does not match computed %q", expected.Branch, wantBranch)
	}

	// Head must be non-empty for an untouched worktree.
	if expected.Head == "" {
		return fmt.Errorf("expected head is empty for worktree %q", expected.Name)
	}

	// List immediately before mutation to reconfirm the snapshot.
	freshList, err := List(ctx, cwd)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	var fresh *Entry
	for i := range freshList {
		if freshList[i].Name == expected.Name {
			v := freshList[i]
			fresh = &v
			break
		}
	}
	if fresh == nil {
		return fmt.Errorf("worktree %q not found: retry", expected.Name)
	}

	// Require exact Path, Branch, Dirty=false. Ignore Merged because
	// the caller HEAD may have moved or changed branch while the run
	// waited.
	if fresh.Path != expected.Path || fresh.Branch != expected.Branch {
		return fmt.Errorf("worktree %q changed: retry", expected.Name)
	}
	if fresh.Dirty {
		return fmt.Errorf("refuse to remove worktree %q: checkout is dirty", expected.Name)
	}

	// Reconfirm checkout HEAD still equals expected.Head.
	checkoutHead, err := runGitFunc(ctx, fresh.Path, "rev-parse", "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("resolve checkout HEAD: %w", ctxErr)
		}
		return fmt.Errorf("worktree %q changed: retry", expected.Name)
	}
	if checkoutHead != expected.Head {
		return fmt.Errorf("worktree %q changed: retry", expected.Name)
	}

	// Reconfirm branch ref HEAD still equals expected.Head.
	branchRef := "refs/heads/" + fresh.Branch
	branchHead, err := runGitFunc(ctx, root, "rev-parse", "--verify", "--quiet", branchRef)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("resolve branch HEAD: %w", ctxErr)
		}
		return fmt.Errorf("worktree %q changed: retry", expected.Name)
	}
	if branchHead != expected.Head {
		return fmt.Errorf("worktree %q changed: retry", expected.Name)
	}

	// All checks passed: remove the checkout without force.
	if _, err := runGitFunc(ctx, root, "worktree", "remove", fresh.Path); err != nil {
		return fmt.Errorf("remove worktree %q: %w", fresh.Path, err)
	}

	// Atomically delete the branch only if HEAD still matches.
	// Passing expected.Head as the old value makes deletion fail rather
	// than delete if another process moves the branch between validation
	// and deletion.
	branchRefDelete := "refs/heads/" + fresh.Branch
	if _, err := runGitFunc(ctx, root, "update-ref", "-d", branchRefDelete, expected.Head); err != nil {
		branchErr := err
		// Attempt one restoration using WithoutCancel with a 10-second timeout.
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, restoreErr := runGitFunc(restoreCtx, root, "worktree", "add", fresh.Path, fresh.Branch); restoreErr != nil {
			return fmt.Errorf("delete branch %q: %w; restore checkout %q from branch %q failed: %v", fresh.Branch, branchErr, fresh.Path, fresh.Branch, restoreErr)
		}
		return fmt.Errorf("delete branch %q: %w; checkout %q restored from branch %q", fresh.Branch, branchErr, fresh.Path, fresh.Branch)
	}
	return nil
}
