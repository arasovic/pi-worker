package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// worktreeError is the refusal shape prepareWorktree returns. usage
// marks the refusals that are the caller's to fix — the current
// directory is not inside a git work tree, a name or branch that is
// already taken, or a checkout git itself refuses to create — and
// runCommand exits 2 for them. The one failure that is not a refusal
// is a run context that expired or was cancelled while a git command
// ran; prepareWorktree returns that as an error wrapping the
// context's own error, so the caller reports the timeout or
// cancellation it is.
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

type managedWorktree struct {
	name   string
	path   string
	branch string
	dirty  bool
	merged bool
}

var runGitFunc = runGit

// listManagedWorktrees inventories the exact private worktrees the
// CLI manages under <root>/.pi-worker/worktrees/<name> on branch
// run/<name>. It never mutates repository state.
func listManagedWorktrees(ctx context.Context, cwd string) ([]managedWorktree, error) {
	root, err := runGitFunc(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	head, err := runGitFunc(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve caller HEAD: %w", err)
	}

	worktreeOutput, err := runGitFunc(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	worktrees, err := parseManagedWorktreeList(root, worktreeOutput)
	if err != nil {
		return nil, err
	}

	branchOutput, err := runGitFunc(ctx, root, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, fmt.Errorf("list run branches: %w", err)
	}
	branches, err := parseManagedBranchNames(branchOutput)
	if err != nil {
		return nil, err
	}

	for name, checkout := range worktrees {
		if _, ok := branches[name]; !ok {
			return nil, fmt.Errorf("managed checkout %q is missing branch %q", checkout.path, checkout.branch)
		}
	}
	for name := range branches {
		if _, ok := worktrees[name]; !ok {
			return nil, fmt.Errorf("managed branch %q is missing its checkout", "run/"+name)
		}
	}

	mergedOutput, err := runGitFunc(ctx, root, "for-each-ref", "--merged="+head, "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, fmt.Errorf("list merged run branches: %w", err)
	}
	mergedBranches, err := parseManagedBranchNames(mergedOutput)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(worktrees))
	for name := range worktrees {
		names = append(names, name)
	}
	sort.Strings(names)

	managed := make([]managedWorktree, 0, len(names))
	for _, name := range names {
		checkout := worktrees[name]
		dirty, err := checkoutHasChanges(ctx, checkout.path)
		if err != nil {
			return nil, fmt.Errorf("inspect worktree %q: %w", checkout.path, err)
		}
		_, merged := mergedBranches[name]
		managed = append(managed, managedWorktree{
			name:   name,
			path:   checkout.path,
			branch: checkout.branch,
			dirty:  dirty,
			merged: merged,
		})
	}
	return managed, nil
}

type managedWorktreeRef struct {
	path   string
	branch string
}

func parseManagedWorktreeList(root, output string) (map[string]managedWorktreeRef, error) {
	const maxEntries = 4096
	const maxLine = 64 * 1024

	managedDir := filepath.Join(root, ".pi-worker", "worktrees")
	entries := make(map[string]managedWorktreeRef)
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 4096), maxLine)

	type entry struct {
		path        string
		branch      string
		sawPath     bool
		sawBranch   bool
		sawDetached bool
		sawBare     bool
		sawLocked   bool
		sawPrunable bool
	}
	current := entry{}
	count := 0
	flush := func() error {
		if !current.sawPath {
			if current.sawBranch {
				return fmt.Errorf("malformed git worktree output: entry missing worktree path")
			}
			return nil
		}
		count++
		if count > maxEntries {
			return fmt.Errorf("malformed git worktree output: too many entries")
		}
		name, managed, err := managedNameFromPath(managedDir, current.path)
		if err != nil {
			return err
		}
		if !managed {
			return nil
		}
		if !current.sawBranch {
			return fmt.Errorf("managed checkout %q is missing its branch", current.path)
		}
		if current.sawBare {
			return fmt.Errorf("managed checkout %q is bare", current.path)
		}
		if current.sawPrunable {
			return fmt.Errorf("managed checkout %q is prunable", current.path)
		}
		if current.sawLocked {
			return fmt.Errorf("managed checkout %q is locked", current.path)
		}
		expected := "refs/heads/run/" + name
		if current.branch != expected {
			return fmt.Errorf("managed checkout %q points to %q, want %q", current.path, current.branch, expected)
		}
		if _, dup := entries[name]; dup {
			return fmt.Errorf("duplicate managed checkout %q", current.path)
		}
		entries[name] = managedWorktreeRef{path: current.path, branch: "run/" + name}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			current = entry{}
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			if current.sawPath {
				return nil, fmt.Errorf("malformed git worktree output: duplicate worktree line %q", line)
			}
			current.sawPath = true
			current.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			if current.sawBranch {
				return nil, fmt.Errorf("malformed git worktree output: duplicate branch line %q", line)
			}
			current.sawBranch = true
			current.branch = strings.TrimPrefix(line, "branch ")
		case strings.HasPrefix(line, "HEAD "):
			// Ignored.
		case line == "detached":
			current.sawDetached = true
		case line == "bare":
			current.sawBare = true
		case strings.HasPrefix(line, "locked"):
			current.sawLocked = true
		case strings.HasPrefix(line, "prunable"):
			current.sawPrunable = true
		default:
			return nil, fmt.Errorf("malformed git worktree output: unexpected line %q", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse git worktree output: %w", err)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return entries, nil
}

func parseManagedBranchNames(output string) (map[string]struct{}, error) {
	const maxEntries = 4096
	const maxLine = 64 * 1024

	names := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 4096), maxLine)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		count++
		if count > maxEntries {
			return nil, fmt.Errorf("malformed git ref output: too many entries")
		}
		if !strings.HasPrefix(line, "run/") {
			continue
		}
		name := strings.TrimPrefix(line, "run/")
		if !validWorktreeName(name) {
			return nil, fmt.Errorf("managed branch %q has invalid name %q", line, name)
		}
		if _, dup := names[name]; dup {
			return nil, fmt.Errorf("duplicate managed branch %q", line)
		}
		names[name] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse git ref output: %w", err)
	}
	return names, nil
}

func managedNameFromPath(managedDir, path string) (name string, managed bool, err error) {
	if !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("malformed git worktree output: path %q is not absolute", path)
	}
	if filepath.Clean(path) != path {
		return "", false, fmt.Errorf("malformed git worktree output: path %q is not clean", path)
	}
	rel, err := filepath.Rel(managedDir, path)
	if err != nil {
		return "", false, fmt.Errorf("malformed git worktree output: path %q: %w", path, err)
	}
	if rel == "." {
		return "", false, fmt.Errorf("managed checkout path %q is missing a name", path)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false, nil
	}
	if strings.ContainsRune(rel, os.PathSeparator) {
		return "", false, fmt.Errorf("managed checkout path %q is nested; want <root>/.pi-worker/worktrees/<name>", path)
	}
	if !validWorktreeName(rel) {
		return "", false, fmt.Errorf("managed checkout path %q has invalid name %q", path, rel)
	}
	return rel, true, nil
}

func checkoutHasChanges(ctx context.Context, path string) (bool, error) {
	out, err := runGitFunc(ctx, path, "status", "--porcelain=v1")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// removeManagedWorktree removes exactly the managed checkout and its
// exact local branch described by expected, but only when still safe.
// It refuses invalid names, any expected checkout marked dirty, or any
// expected branch marked unmerged without mutating state. Immediately
// before mutation it re-inventories from cwd via listManagedWorktrees,
// finds the same name, and requires the full observable snapshot to
// match exactly: name, path, branch, dirty, merged. A missing or
// changed target returns a retry error with no mutation. It reconfirms
// the fresh target is clean and merged, then runs git worktree remove
// on the exact path without force, and only after that succeeds deletes
// the exact branch with git branch -d without force.
func removeManagedWorktree(ctx context.Context, cwd string, expected managedWorktree) error {
	if !validWorktreeName(expected.name) {
		return fmt.Errorf("invalid worktree name %q", expected.name)
	}
	if expected.dirty {
		return fmt.Errorf("refuse to remove worktree %q: checkout is dirty", expected.name)
	}
	if !expected.merged {
		return fmt.Errorf("refuse to remove worktree %q: branch %q is not merged", expected.name, expected.branch)
	}
	freshList, err := listManagedWorktrees(ctx, cwd)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	var fresh *managedWorktree
	for i := range freshList {
		if freshList[i].name == expected.name {
			v := freshList[i]
			fresh = &v
			break
		}
	}
	if fresh == nil {
		return fmt.Errorf("worktree %q not found: retry", expected.name)
	}
	if fresh.name != expected.name || fresh.path != expected.path || fresh.branch != expected.branch || fresh.dirty != expected.dirty || fresh.merged != expected.merged {
		return fmt.Errorf("worktree %q changed: retry", expected.name)
	}
	if fresh.dirty {
		return fmt.Errorf("refuse to remove worktree %q: checkout is dirty", fresh.name)
	}
	if !fresh.merged {
		return fmt.Errorf("refuse to remove worktree %q: branch %q is not merged", fresh.name, fresh.branch)
	}
	if _, err := runGitFunc(ctx, cwd, "worktree", "remove", fresh.path); err != nil {
		return fmt.Errorf("remove worktree %q: %w", fresh.path, err)
	}
	if _, err := runGitFunc(ctx, cwd, "branch", "-d", fresh.branch); err != nil {
		return fmt.Errorf("delete branch %q: %w", fresh.branch, err)
	}
	return nil
}

// prepareWorktree gives one run a private checkout of its own. It
// resolves the repository root from cwd — os.Getwd may be a
// subdirectory of the repository, and the checkout must live under the
// root, never under the current subdirectory — refuses a name or
// branch that a leftover checkout already took, and creates the
// checkout at <root>/.pi-worker/worktrees/<name> on branch run/<name>
// from HEAD. A checkout git itself refuses to create is refused the
// same way, with git's own words in the message; only a run context
// that expired or was cancelled while a git command ran is something
// else — an error wrapping the context's own error, never a refusal.
// The checkout is made from HEAD: uncommitted work in the caller's
// tree is not carried into it. Nothing is ever removed on any path: a
// leftover checkout or branch is reported by the next run that
// collides with its name, never cleaned up.
func prepareWorktree(ctx context.Context, cwd, name string) (path, branch string, err error) {
	root, err := runGitFunc(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", fmt.Errorf("resolve repository root: %w", ctxErr)
		}
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
	// the branch itself, but the refusal must name the leftover. A git
	// failure here is the normal "no such ref" answer, with one
	// exception: a run context that expired or was cancelled is the
	// run's own end, never a green light to keep going.
	if _, err := runGitFunc(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return "", "", &worktreeError{msg: fmt.Sprintf("branch %s already exists; collect it or choose another name", branch), usage: true}
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", fmt.Errorf("create worktree %s: %w", path, ctxErr)
	}
	// git is the final authority: a worktree add it refuses — a ref
	// collision no pre-check names, a path problem, anything — is a
	// refusal too, carrying git's own words so the caller can see why.
	// The one exception is the expired or cancelled run context, which
	// is returned as the context's own error, never a refusal.
	if _, err := runGitFunc(ctx, root, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", fmt.Errorf("create worktree %s: %w", path, ctxErr)
		}
		return "", "", &worktreeError{msg: fmt.Sprintf("create worktree %s: %v", path, err), usage: true}
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
// caller must fix (exit 2) rather than an expired or cancelled run
// context or an internal creation failure.
func worktreeRefused(err error) bool {
	var refusal *worktreeError
	return errors.As(err, &refusal) && refusal.usage
}
