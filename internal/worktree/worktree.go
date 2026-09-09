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
	Name   string `json:"name"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
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

// resolveMainRoot resolves the main worktree root from dir. Managed
// worktrees always live in the main worktree's .pi-worker/worktrees/,
// so root resolution must not depend on the caller's checkout:
// git rev-parse --show-toplevel returns the *current* worktree's top
// level when run inside a linked worktree, which once nested new
// managed worktrees under the caller's checkout and made the outer
// worktree's own branch appear to have no checkout. Instead this uses
// git rev-parse --git-common-dir, which from any checkout names the
// shared .git directory that always lives in the main worktree; the
// root is that path with its trailing .git component removed. One
// shell-out per resolution.
//
// Inside a git submodule the common directory does not end in .git:
// git rev-parse --git-common-dir returns something like
// <superproject>/.git/modules/sub while --show-toplevel returns the
// submodule's own top level. Stripping a trailing .git there would
// leave the superproject's internal git storage as the "root", so the
// stripped value is only used when the common directory actually ends
// in a .git component; otherwise the submodule's own top level (the
// pre-common-dir behavior) is correct.
func resolveMainRoot(ctx context.Context, dir string) (string, error) {
	commonDir, err := runGitFunc(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(commonDir) {
		// git reports a relative path against the directory it ran in.
		commonDir = filepath.Join(dir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	sep := string(os.PathSeparator)
	if strings.HasSuffix(commonDir, sep+".git") {
		return strings.TrimSuffix(commonDir, sep+".git"), nil
	}
	// The common directory is git storage that is not <root>/.git, so
	// no repository root can be derived from it. Measured shapes:
	// a submodule reports <superproject>/.git/modules/<name>, and a
	// repository created with --separate-git-dir reports the external
	// metadata directory. Fall back to the top level of the checkout
	// that dir belongs to, which is what this package did before.
	//
	// Known limit, measured: in the --separate-git-dir layout a linked
	// worktree still resolves to itself, so a managed worktree created
	// from one nests as #191 described. git offers no better answer
	// there — git worktree list names the metadata directory, not the
	// user's main checkout — so this is git's limit, not a missing case.
	return runGitFunc(ctx, dir, "rev-parse", "--show-toplevel")
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

	root, err := resolveMainRoot(ctx, cwd)
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
