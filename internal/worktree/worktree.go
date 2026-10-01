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

// samePath reports whether two spellings name the same directory on
// disk. It is the #229 identity comparison, reused for two paths
// instead of a path and a stat: git lists the physical spelling of a
// repository reached through a symlink (/private/var/...), while a
// caller that resolved the root through the symlink holds the other
// spelling (/var/...). Only a directory that exists has an identity, so
// when the first spelling cannot be statted the pair is compared
// literally and is equal only when both name the same clean path — that
// keeps two different missing paths apart and lets the caller's own
// missing-target check report a checkout that is already gone.
func samePath(a, b string) bool {
	info := statIfExists(a)
	if info == nil {
		// No identity to compare: a or b is gone, so only the exact
		// spelling can claim they are the same place.
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return sameDir(b, info)
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
// worktree's own branch appear to have no checkout.
//
// When dir is the main checkout its private git directory and the
// common directory name the same place (git rev-parse --git-dir equals
// --git-common-dir), so the main root is simply this checkout's
// --show-toplevel. That branch is what keeps a repository whose
// metadata lives outside the checkout (git init --separate-git-dir)
// resolving to the checkout instead of to the metadata directory; the
// linked-worktree logic below cannot see the checkout at all.
//
// In a linked worktree the two directories differ: --git-dir names the
// worktree's private directory under the common one, which names the
// main worktree's .git; the root is that path with its trailing .git
// component removed. Two shell-outs per resolution, plus
// --show-toplevel when dir is the main checkout.
//
// Inside a git submodule the two directories are also equal, so the
// main-checkout branch above returns the submodule's own top level.
// Were it not caught there, the fallback below would still be correct:
// git rev-parse --git-common-dir returns something like
// <superproject>/.git/modules/sub, which does not end in a .git
// component, so stripping would leave the superproject's internal git
// storage as the "root"; the fallback returns --show-toplevel instead.
func resolveMainRoot(ctx context.Context, dir string) (string, error) {
	gitDir, err := runGitFunc(ctx, dir, "rev-parse", "--git-dir")
	if err != nil {
		return "", err
	}
	commonDir, err := runGitFunc(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	// git reports paths relative to the directory it ran in; make both
	// absolute the same way before comparing them.
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	gitDir = filepath.Clean(gitDir)
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(dir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if gitDir == commonDir {
		// dir is the main checkout: its git directory is not a linked
		// worktree's private directory, so the main root is its top level.
		top, err := runGitFunc(ctx, dir, "rev-parse", "--show-toplevel")
		if err != nil {
			return "", err
		}
		// A repository reached through a symlink has two spellings of its
		// top level; keep the caller's when the common directory is this
		// checkout's own .git so the recorded path matches how the user
		// reached it. When the metadata lives outside the checkout
		// (--separate-git-dir) the stripped common directory is storage,
		// not the checkout, so --show-toplevel is the only answer.
		if strings.HasSuffix(commonDir, string(os.PathSeparator)+".git") {
			stripped := strings.TrimSuffix(commonDir, string(os.PathSeparator)+".git")
			if sameDir(stripped, statIfExists(top)) {
				return stripped, nil
			}
		}
		return top, nil
	}
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
// the repository root from cwd, resolves the exact HEAD hash once from
// the caller's directory — not the main worktree's HEAD, which differs
// when cwd is a linked worktree — refuses an existing path or branch
// before any mutation, and creates the worktree at
// <root>/.pi-worker/worktrees/<name> on branch run/<name> from the
// resolved hash. A cancelled or expired context wraps ctx.Err and is
// never a refusal. No cleanup is attempted on failure.
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

	head, err := runGitFunc(ctx, cwd, "rev-parse", "HEAD")
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
