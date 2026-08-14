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
}

// GitChange is the workspace git state before and after a run.
type GitChange struct {
	Before GitState `json:"before"`
	After  GitState `json:"after"`
}

// GitInspector reads the git state of a workspace directory.
type GitInspector interface {
	// Inspect returns nil, nil when dir is not inside a git work tree.
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
// first: when dir is not inside a git work tree, so the guard command
// fails or prints anything other than "true", Inspect returns nil, nil
// and a caller treats the workspace as a silent no-op. After the guard,
// HEAD comes from rev-parse HEAD and is left empty when that fails —
// the unborn-branch case, which is not an error. The branch is
// rev-parse --abbrev-ref HEAD trimmed, with the literal "HEAD" meaning
// detached and left empty; an unborn branch makes that command fail
// after printing the same literal "HEAD", which is the same
// unborn-HEAD case and equally not an error. Dirty is true when git
// status --porcelain prints anything, and Stashes is the number of
// non-empty git stash list lines. A failure of any command after the
// guard other than the unborn-HEAD case is returned as an error.
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
	porcelain, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	state.Dirty = porcelain != ""
	stash, err := gitOutput(ctx, dir, "stash", "list")
	if err != nil {
		return nil, fmt.Errorf("git stash list: %w", err)
	}
	state.Stashes = nonEmptyLineCount(stash)
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

// nonEmptyLineCount counts the non-empty lines of output.
func nonEmptyLineCount(output string) int {
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	return count
}
