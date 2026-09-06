// Package worktree creates private git worktrees for runs.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Prepared describes a freshly created worktree.
type Prepared struct {
	Name   string
	Path   string
	Branch string
	Head   string
}

// refusal is the error type Prepare returns for problems the caller
// must fix — not inside a git work tree, name or branch already taken,
// or a git-add refusal. IsRefusal classifies it.
type refusal struct {
	msg string
}

func (e *refusal) Error() string { return e.msg }

// IsRefusal reports whether err is a Prepare refusal the caller must
// fix rather than a cancelled/expired context or an internal error.
func IsRefusal(err error) bool {
	var r *refusal
	return errors.As(err, &r)
}

// ValidName reports whether name is a legal worktree name: 1 to 64
// characters of lowercase letters, digits and hyphens, starting and
// ending with a letter or digit.
func ValidName(name string) bool {
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

// runGitFunc is the seam for testing. It defaults to runGit.
var runGitFunc = runGit

// runGit runs one git command in dir and returns its captured stdout.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Prepare creates a private worktree for name under cwd. It resolves
// the repository root from cwd, resolves the exact HEAD hash once,
// refuses an existing path or branch before any mutation, and creates
// the worktree at <root>/.pi-worker/worktrees/<name> on branch
// run/<name> from the resolved hash. A cancelled or expired context
// wraps ctx.Err and is never a refusal. No cleanup is attempted on
// failure.
func Prepare(ctx context.Context, cwd, name string) (Prepared, error) {
	if !ValidName(name) {
		return Prepared{}, &refusal{msg: fmt.Sprintf("invalid worktree name %q: use 1 to 64 characters of lowercase letters, digits and hyphens, starting and ending with a letter or digit", name)}
	}

	root, err := runGitFunc(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Prepared{}, fmt.Errorf("resolve repository root: %w", ctxErr)
		}
		return Prepared{}, &refusal{msg: "--worktree requires the current directory to be inside a git work tree"}
	}

	path := filepath.Join(root, ".pi-worker", "worktrees", name)
	branch := "run/" + name

	// Refuse existing directory.
	if _, err := os.Lstat(path); err == nil {
		return Prepared{}, &refusal{msg: fmt.Sprintf("worktree %s already exists; collect it or choose another name", path)}
	} else if !os.IsNotExist(err) {
		return Prepared{}, fmt.Errorf("create worktree %s: %v", path, err)
	}

	// Refuse existing branch.
	if _, err := runGitFunc(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return Prepared{}, &refusal{msg: fmt.Sprintf("branch %s already exists; collect it or choose another name", branch)}
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return Prepared{}, fmt.Errorf("create worktree %s: %w", path, ctxErr)
	}

	head, err := runGitFunc(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Prepared{}, fmt.Errorf("resolve HEAD: %w", ctxErr)
		}
		return Prepared{}, &refusal{msg: fmt.Sprintf("resolve HEAD: %v", err)}
	}

	// git is the final authority; a refusal it returns is ours too.
	if _, err := runGitFunc(ctx, root, "worktree", "add", "-b", branch, path, head); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Prepared{}, fmt.Errorf("create worktree %s: %w", path, ctxErr)
		}
		return Prepared{}, &refusal{msg: fmt.Sprintf("create worktree %s: %v", path, err)}
	}

	return Prepared{Name: name, Path: path, Branch: branch, Head: head}, nil
}
