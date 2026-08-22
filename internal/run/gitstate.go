package run

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// GitState is the git state of a workspace at one instant.
type GitState struct {
	Head    string `json:"head"`
	Branch  string `json:"branch,omitempty"`
	Dirty   bool   `json:"dirty"`
	Stashes int    `json:"stashes"`
	// StashEntries identifies each stash entry as "<sha> <subject>". It is
	// the comparison key and never appears in the result document, which
	// keeps the change additive: the serialized shape of an existing field
	// does not move.
	StashEntries []string `json:"-"`
}

// GitStashChange names the stash entries a run added and removed. Entries
// are compared by identity, not by position or count, so an index shift is
// not a change and a drop paired with a push is not silence.
type GitStashChange struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// GitChange is the workspace git state before and after a run.
type GitChange struct {
	Before GitState        `json:"before"`
	After  GitState        `json:"after"`
	Stash  *GitStashChange `json:"stash,omitempty"`
}

// GitInspector reads the git state of a workspace directory.
type GitInspector interface {
	// Inspect returns nil, nil when the work tree could not be
	// confirmed: dir is not inside a git work tree, git is missing
	// entirely, or the guard failed for a transient reason. The three
	// collapse into one result on purpose — the caller cannot tell
	// them apart — and the change manifest reports a single stated
	// omission reason for all of them instead of treating the field as
	// absent.
	Inspect(ctx context.Context, dir string) (*GitState, error)
}

// DefaultGitInspector executes read-only git commands directly with
// os/exec: no shell and no assembled command strings.
type DefaultGitInspector struct{}

// NewDefaultGitInspector returns the os/exec-backed inspector.
func NewDefaultGitInspector() GitInspector {
	return &DefaultGitInspector{}
}

// Inspect collects the git state of dir at one instant. The guard runs
// first: when the work tree cannot be confirmed — the guard command
// fails (dir is not inside a git work tree, git is missing, or the
// failure was transient) or prints anything other than "true" —
// Inspect returns nil, nil and the caller decides what a stated
// omission means, never treating the manifest as silently absent.
// After the guard, HEAD comes from rev-parse HEAD and is left empty
// when that fails — the unborn-branch case, which is not an error. The
// branch is rev-parse --abbrev-ref HEAD trimmed, with the literal
// "HEAD" meaning detached and left empty; an unborn branch makes that
// command fail after printing the same literal "HEAD", which is the
// same unborn-HEAD case and equally not an error. Dirty is true when
// git status --porcelain prints anything; the status command forces
// status.showUntrackedFiles=all so a repository configured to hide
// untracked files from that display still reports a tree that is
// genuinely dirty — dirtiness must not depend on the user's display
// preference. The stash list comes from git stash list --format=%H %gs,
// one "<sha> <subject>" identity per non-empty line in git's order
// (newest first); Stashes is the number of entries. A failure of any
// command after the guard other than the unborn-HEAD case is returned
// as an error.
func (i *DefaultGitInspector) Inspect(ctx context.Context, dir string) (*GitState, error) {
	inside, err := gitOutput(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		return nil, nil
	}
	state := &GitState{}
	if head, err := gitOutput(ctx, dir, "rev-parse", "HEAD"); err == nil {
		state.Head = strings.TrimSpace(head)
	}
	branch, err := gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		// An unborn branch makes this command fail after printing the
		// literal "HEAD" on stdout, same as a detached HEAD. That is the
		// unborn-HEAD case, not an error; any other failure is.
		if strings.TrimSpace(branch) != "HEAD" {
			return nil, fmt.Errorf("git branch: %w", err)
		}
	} else if trimmed := strings.TrimSpace(branch); trimmed != "HEAD" {
		state.Branch = trimmed
	}
	porcelain, err := gitOutput(ctx, dir, "-c", "status.showUntrackedFiles=all", "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	state.Dirty = porcelain != ""
	stash, err := gitOutput(ctx, dir, "stash", "list", "--format=%H %gs")
	if err != nil {
		return nil, fmt.Errorf("git stash list: %w", err)
	}
	state.StashEntries = nonEmptyLines(stash)
	state.Stashes = len(state.StashEntries)
	return state, nil
}

// gitOutput runs one read-only git command in dir and returns its
// captured stdout. A start failure, a non-zero exit, or a context that
// expires or is cancelled while the command runs is returned as an
// error; the command's stderr never reaches the caller.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The captured stdout stays meaningful on failure: an unborn
		// branch's rev-parse prints the literal "HEAD" before failing.
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// nonEmptyLines returns the non-empty lines of output in order.
func nonEmptyLines(output string) []string {
	lines := []string{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		if trimmed := strings.TrimSpace(scanner.Text()); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// stashDiff names the stash entries a run added and removed: entries
// present in after but not before are Added, entries present in before
// but not after are Removed, each in git stash list order (newest
// first). The sha makes every entry unique, so a plain set difference is
// correct. It returns nil when nothing appeared and nothing disappeared.
func stashDiff(before, after *GitState) *GitStashChange {
	beforeSet := make(map[string]struct{}, len(before.StashEntries))
	for _, entry := range before.StashEntries {
		beforeSet[entry] = struct{}{}
	}
	afterSet := make(map[string]struct{}, len(after.StashEntries))
	for _, entry := range after.StashEntries {
		afterSet[entry] = struct{}{}
	}
	var added, removed []string
	for _, entry := range after.StashEntries {
		if _, seen := beforeSet[entry]; !seen {
			added = append(added, entry)
		}
	}
	for _, entry := range before.StashEntries {
		if _, seen := afterSet[entry]; !seen {
			removed = append(removed, entry)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	return &GitStashChange{Added: added, Removed: removed}
}
