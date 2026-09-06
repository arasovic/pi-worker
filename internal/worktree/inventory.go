package worktree

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry describes one managed private worktree: a checkout at
// <root>/.pi-worker/worktrees/<name> on branch run/<name>.
type Entry struct {
	Name   string
	Path   string
	Branch string
	Dirty  bool
	Merged bool
}

// entryRef is the path/branch pair parsed from git worktree list
// output for one managed checkout.
type entryRef struct {
	path   string
	branch string
}

// List inventories the exact private worktrees managed under
// <root>/.pi-worker/worktrees/<name> on branch run/<name>. It never
// mutates repository state.
func List(ctx context.Context, cwd string) ([]Entry, error) {
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

	// Every managed checkout must have a matching branch and vice versa.
	for name, ref := range worktrees {
		if _, ok := branches[name]; !ok {
			return nil, fmt.Errorf("managed checkout %q is missing branch %q", ref.path, ref.branch)
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

	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		ref := worktrees[name]
		dirty, err := checkoutHasChanges(ctx, ref.path)
		if err != nil {
			return nil, fmt.Errorf("inspect worktree %q: %w", ref.path, err)
		}
		_, merged := mergedBranches[name]
		entries = append(entries, Entry{
			Name:   name,
			Path:   ref.path,
			Branch: ref.branch,
			Dirty:  dirty,
			Merged: merged,
		})
	}
	return entries, nil
}

// parseManagedWorktreeList parses the bounded porcelain output of
// git worktree list and returns the managed entries keyed by name.
func parseManagedWorktreeList(root, output string) (map[string]entryRef, error) {
	const maxEntries = 4096
	const maxLine = 64 * 1024

	managedDir := filepath.Join(root, ".pi-worker", "worktrees")
	entries := make(map[string]entryRef)
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 4096), maxLine)

	type pending struct {
		path        string
		branch      string
		sawPath     bool
		sawBranch   bool
		sawBare     bool
		sawLocked   bool
		sawPrunable bool
	}
	cur := pending{}
	count := 0
	flush := func() error {
		if !cur.sawPath {
			if cur.sawBranch {
				return fmt.Errorf("malformed git worktree output: entry missing worktree path")
			}
			return nil
		}
		count++
		if count > maxEntries {
			return fmt.Errorf("malformed git worktree output: too many entries")
		}
		name, managed, err := managedNameFromPath(managedDir, cur.path)
		if err != nil {
			return err
		}
		if !managed {
			return nil
		}
		if !cur.sawBranch {
			return fmt.Errorf("managed checkout %q is missing its branch", cur.path)
		}
		if cur.sawBare {
			return fmt.Errorf("managed checkout %q is bare", cur.path)
		}
		if cur.sawPrunable {
			return fmt.Errorf("managed checkout %q is prunable", cur.path)
		}
		if cur.sawLocked {
			return fmt.Errorf("managed checkout %q is locked", cur.path)
		}
		expected := "refs/heads/run/" + name
		if cur.branch != expected {
			return fmt.Errorf("managed checkout %q points to %q, want %q", cur.path, cur.branch, expected)
		}
		if _, dup := entries[name]; dup {
			return fmt.Errorf("duplicate managed checkout %q", cur.path)
		}
		entries[name] = entryRef{path: cur.path, branch: "run/" + name}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			cur = pending{}
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			if cur.sawPath {
				return nil, fmt.Errorf("malformed git worktree output: duplicate worktree line %q", line)
			}
			cur.sawPath = true
			cur.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			if cur.sawBranch {
				return nil, fmt.Errorf("malformed git worktree output: duplicate branch line %q", line)
			}
			cur.sawBranch = true
			cur.branch = strings.TrimPrefix(line, "branch ")
		case strings.HasPrefix(line, "HEAD "):
			// Ignored.
		case line == "detached":
			// Unrelated entries may be detached; managed detached checkouts
			// lack the exact branch and are refused via the missing-branch check.
		case line == "bare":
			cur.sawBare = true
		case strings.HasPrefix(line, "locked"):
			cur.sawLocked = true
		case strings.HasPrefix(line, "prunable"):
			cur.sawPrunable = true
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

// parseManagedBranchNames parses for-each-ref output and returns the
// set of managed run/<name> branch names.
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
		if !ValidName(name) {
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

// managedNameFromPath extracts the worktree name from a checkout
// path that must be a direct child of managedDir.
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
	if !ValidName(rel) {
		return "", false, fmt.Errorf("managed checkout path %q has invalid name %q", path, rel)
	}
	return rel, true, nil
}

// checkoutHasChanges reports whether the worktree at path has
// uncommitted changes.
func checkoutHasChanges(ctx context.Context, path string) (bool, error) {
	out, err := runGitFunc(ctx, path, "status", "--porcelain=v1")
	if err != nil {
		return false, err
	}
	return out != "", nil
}
