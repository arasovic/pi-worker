package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// worktreeError is the refusal shape prepareWorktree returns. usage
// marks the refusals that are the caller's to fix — the current
// directory is not inside a git work tree, or the name or branch is
// already taken — and runCommand exits 2 for them. A failure to
// actually create the checkout is not the caller's fault and is
// reported as an internal error, exit 9.
type worktreeError struct {
	msg   string
	usage bool
}

func (e *worktreeError) Error() string { return e.msg }

// validWorktreeName reports whether name is a legal --worktree name: 1
// to 64 characters of lowercase letters, digits and hyphens, starting
// and ending with a letter or digit. The check runs at argv-parse time,
// so a bad name is a usage error like every other one.
func validWorktreeName(name string) bool {
	if len(name) < 1 || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(name)-1:
		default:
			return false
		}
	}
	return true
}

// prepareWorktree gives one run a private checkout of its own. It
// resolves the repository root from cwd — os.Getwd may be a
// subdirectory of the repository, and the checkout must live under the
// root, never under the current subdirectory — refuses a name or
// branch that a leftover checkout already took, and creates the
// checkout at <root>/.pi-worker/worktrees/<name> on branch run/<name>
// from HEAD. The checkout is made from HEAD: uncommitted work in the
// caller's tree is not carried into it. Nothing is ever removed on any
// path: a leftover checkout or branch is reported by the next run that
// collides with its name, never cleaned up.
func prepareWorktree(ctx context.Context, cwd, name string) (path, branch string, err error) {
	root, err := runGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", &worktreeError{msg: "--worktree requires the current directory to be inside a git work tree", usage: true}
	}
	path = filepath.Join(root, ".pi-worker", "worktrees", name)
	branch = "run/" + name
	// A leftover directory with no branch refuses the run. git worktree
	// add would refuse an existing directory itself, but the refusal
	// must be ours, must name the leftover, and must happen before the
	// run record is written and before any worker starts.
	if _, err := os.Lstat(path); err == nil {
		return "", "", &worktreeError{msg: fmt.Sprintf("worktree %s already exists; collect it or choose another name", path), usage: true}
	} else if !os.IsNotExist(err) {
		// A path that can be created but not inspected is a creation
		// failure, not a taken name.
		return "", "", fmt.Errorf("create worktree %s: %v", path, err)
	}
	// A leftover branch with no directory refuses too: git would catch
	// the branch itself, but the refusal must name the leftover.
	if _, err := runGit(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return "", "", &worktreeError{msg: fmt.Sprintf("branch %s already exists; collect it or choose another name", branch), usage: true}
	}
	if _, err := runGit(ctx, root, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		return "", "", fmt.Errorf("create worktree %s: %v", path, err)
	}
	return path, branch, nil
}

// runGit runs one git command in dir and returns its captured stdout.
// A start failure, a non-zero exit, or a context that expires or is
// cancelled while the command runs is returned as an error carrying the
// command's stderr, so a failed worktree add reports why git refused.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// worktreeRefused reports whether err is a prepareWorktree refusal the
// caller must fix (exit 2) rather than an internal creation failure.
func worktreeRefused(err error) bool {
	var refusal *worktreeError
	return errors.As(err, &refusal) && refusal.usage
}
