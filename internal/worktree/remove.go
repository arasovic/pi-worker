package worktree

import (
	"context"
	"fmt"
	"time"
)

// Remove removes exactly the managed checkout and its local branch
// described by expected, but only when still safe. It refuses invalid
// names, any expected checkout marked dirty, or any expected branch
// marked unmerged without mutating state. Immediately before mutation
// it re-inventories via List, finds the same name, and requires the
// full observable snapshot to match exactly: name, path, branch,
// dirty, merged. A missing or changed target returns a retry error
// with no mutation. It reconfirms the fresh target is clean and
// merged, then runs git worktree remove on the exact path without
// force, and only after that succeeds deletes the exact branch with
// git branch -d without force. If that branch delete fails the helper
// makes one bounded, non-force attempt to restore the checkout with
// git worktree add on the exact path and branch and still returns an
// error.
func Remove(ctx context.Context, cwd string, expected Entry) error {
	if err := ValidateRemove(expected); err != nil {
		return err
	}

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
	if fresh.Name != expected.Name || fresh.Path != expected.Path || fresh.Branch != expected.Branch || fresh.Dirty != expected.Dirty || fresh.Merged != expected.Merged {
		return fmt.Errorf("worktree %q changed: retry", expected.Name)
	}

	if err := ValidateRemove(*fresh); err != nil {
		return err
	}

	if _, err := runGitFunc(ctx, cwd, "worktree", "remove", fresh.Path); err != nil {
		return fmt.Errorf("remove worktree %q: %w", fresh.Path, err)
	}

	if _, err := runGitFunc(ctx, cwd, "branch", "-d", fresh.Branch); err != nil {
		branchErr := err
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, restoreErr := runGitFunc(restoreCtx, cwd, "worktree", "add", fresh.Path, fresh.Branch); restoreErr != nil {
			return fmt.Errorf("delete branch %q: %w; restore checkout %q from branch %q failed: %v", fresh.Branch, branchErr, fresh.Path, fresh.Branch, restoreErr)
		}
		return fmt.Errorf("delete branch %q: %w; checkout %q restored from branch %q", fresh.Branch, branchErr, fresh.Path, fresh.Branch)
	}
	return nil
}

// ValidateRemove checks pre-conditions that must hold before any
// mutation is attempted. It refuses invalid names, any entry marked
// dirty, or any entry whose branch is marked unmerged.
func ValidateRemove(wt Entry) error {
	if !ValidName(wt.Name) {
		return fmt.Errorf("invalid worktree name %q", wt.Name)
	}
	if wt.Dirty {
		return fmt.Errorf("refuse to remove worktree %q: checkout is dirty", wt.Name)
	}
	if !wt.Merged {
		return fmt.Errorf("refuse to remove worktree %q: branch %q is not merged", wt.Name, wt.Branch)
	}
	return nil
}
