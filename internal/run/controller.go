// Package run coordinates bounded parallel worker slices: up to three
// foreground workers execute accepted tasks concurrently in one shared
// workspace, and the controller aggregates their outcomes into one run
// result.
package run

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
)

// MaxTasks is the absolute cap on accepted tasks per run.
const MaxTasks = 3

// Request describes one bounded parallel run: every accepted task runs
// concurrently through the same worker with the same model and workspace.
type Request struct {
	Model         string
	ThinkingLevel pi.ThinkingLevel
	Tasks         []string
	Workspace     string
	// Verify is the command run in the workspace after a completed run
	// to check the finished work, split into argv; empty disables
	// verification.
	Verify []string
	// Writes optionally declares, per task, the workspace-relative paths
	// that task intends to write. A nil or empty entry declares nothing
	// for that task. When two tasks declare overlapping paths the run is
	// rejected before any worker starts.
	Writes [][]string
	// Debug is the shared run-level sink every worker labels with its own
	// identity; nil disables all debug logging.
	Debug *pi.DebugSink
}

// Result is the aggregate outcome of one run. Workers preserve input
// order regardless of completion order. Verification, when present, is
// the outcome of the run-level check command executed in the workspace
// after a completed run.
type Result struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Status        contracts.RunStatus `json:"status"`
	Workers       []pi.WorkerResult   `json:"workers"`
	// Verification is the outcome of the run-level check command; nil
	// when no verification ran.
	Verification *Verification `json:"verification,omitempty"`
	// Git is present only when the run moved HEAD, the branch, or the
	// stash list; nil otherwise.
	Git *GitChange `json:"git,omitempty"`
}

// Controller runs accepted tasks concurrently through one Worker and,
// when a verifier is configured, checks a completed run's workspace
// with one command; when a git inspector is configured, it records the
// workspace git state before and after the run.
type Controller struct {
	worker       pi.Worker
	verifier     Verifier
	gitInspector GitInspector
}

// Option configures a Controller.
type Option func(*Controller)

// WithVerifier configures the run-level check command executed in the
// workspace after a completed run.
func WithVerifier(v Verifier) Option {
	return func(c *Controller) { c.verifier = v }
}

// WithGitInspector configures the read-only git-state recording around
// a run.
func WithGitInspector(g GitInspector) Option {
	return func(c *Controller) { c.gitInspector = g }
}

// New returns a controller that runs accepted tasks through worker.
// Options configure the optional verifier and the optional git-state
// inspector; without them, Run behaves exactly as before.
func New(worker pi.Worker, opts ...Option) *Controller {
	c := &Controller{worker: worker}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run validates the request before starting any worker, starts every
// accepted task concurrently with the SAME parent context passed to Run,
// waits for every started worker before returning (including after
// cancellation), and aggregates the run status from the parent context
// state and the worker outcomes. When a git inspector is configured,
// Run records the workspace git state before any worker starts and
// again after every worker settles, reporting only when HEAD, the
// branch, or the stash list moved. When a verifier and a verification
// command are configured and the run completed with the parent context
// intact, Run verifies the workspace once before returning.
func (c *Controller) Run(ctx context.Context, req Request) (Result, error) {
	if err := validate(req); err != nil {
		return Result{}, err
	}
	// The before state is recorded after validation and before the first
	// worker starts. Git state reporting is diagnostic, not a gate: a
	// non-git workspace or an inspection error leaves Git nil without
	// failing the run.
	var before *GitState
	if c.gitInspector != nil {
		before, _ = c.gitInspector.Inspect(ctx, req.Workspace)
	}
	results := make([]pi.WorkerResult, len(req.Tasks))
	var wg sync.WaitGroup
	for i, task := range req.Tasks {
		wg.Add(1)
		go func(index int, task string) {
			defer wg.Done()
			results[index] = c.worker.Run(ctx, pi.WorkerRequest{
				Model:         req.Model,
				ThinkingLevel: req.ThinkingLevel,
				Prompt:        task,
				Workspace:     req.Workspace,
				WorkerID:      index + 1,
				Debug:         req.Debug,
			})
		}(i, task)
	}
	wg.Wait()
	result := Result{
		SchemaVersion: contracts.SchemaVersion,
		Status:        aggregateStatus(ctx, results),
		Workers:       results,
	}
	// The after state is the opposite of verification: it runs on every
	// terminal status, not only on a completed run, because a run that
	// timed out mid-checkout is exactly the run whose git state a caller
	// most needs. It never depends on the parent context — the workers
	// have already settled, and a deadline is exactly when the state
	// matters — so it always runs under a fresh five-second budget. An
	// error from either inspection leaves Git nil and is never returned.
	if c.gitInspector != nil && before != nil {
		afterCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		after, err := c.gitInspector.Inspect(afterCtx, req.Workspace)
		if err == nil && after != nil && gitMoved(before, after) {
			result.Git = &GitChange{Before: *before, After: *after}
		}
	}
	// Verification runs once for the whole run after every worker has
	// settled, and only on a completed run with a live context: a partial
	// or failed run leaves the workspace half-written, and a cancelled or
	// timed-out context would fail the command for an unrelated reason.
	if c.verifier != nil && len(req.Verify) > 0 && result.Status == contracts.RunCompleted && ctx.Err() == nil {
		verification, err := c.verifier.Verify(ctx, req.Workspace, req.Verify)
		if err != nil {
			return result, fmt.Errorf("verification: %w", err)
		}
		result.Verification = &verification
	}
	return result, nil
}

// validate checks the request before any worker starts: a non-empty model,
// a non-empty workspace, between 1 and MaxTasks tasks, no empty task after
// trimming whitespace, and — when Writes is present — exactly one write
// entry per task whose declared paths are normalized and checked. The
// write declaration is pure input validation: nothing is read from the
// workspace, and the declaration is never passed to workers, never echoed
// in the result, and never enforced during or after the run.
func validate(req Request) error {
	if req.Model == "" {
		return fmt.Errorf("model is required")
	}
	if req.Workspace == "" {
		return fmt.Errorf("workspace is required")
	}
	if len(req.Tasks) == 0 {
		return fmt.Errorf("at least one task is required")
	}
	if len(req.Tasks) > MaxTasks {
		return fmt.Errorf("at most %d tasks are supported, got %d", MaxTasks, len(req.Tasks))
	}
	for i, task := range req.Tasks {
		if strings.TrimSpace(task) == "" {
			return fmt.Errorf("task %d is empty", i+1)
		}
	}
	// Writes is either nil or exactly as long as Tasks; a shorter or
	// longer slice is a validation error. An individual entry may be nil
	// or empty, declaring nothing for that task.
	if req.Writes != nil && len(req.Writes) != len(req.Tasks) {
		return fmt.Errorf("writes must declare one entry per task: got %d entries for %d tasks", len(req.Writes), len(req.Tasks))
	}
	normalized := make([][]string, len(req.Tasks))
	for i, declared := range req.Writes {
		if len(declared) == 0 {
			continue
		}
		seen := make(map[string]bool, len(declared))
		normalized[i] = make([]string, 0, len(declared))
		for _, value := range declared {
			clean, err := validateWritePath(value)
			if err != nil {
				return fmt.Errorf("task %d: %v", i+1, err)
			}
			if seen[clean] {
				return fmt.Errorf("task %d declares write path %q more than once", i+1, clean)
			}
			seen[clean] = true
			normalized[i] = append(normalized[i], clean)
		}
	}
	for i := 0; i < len(normalized); i++ {
		for j := i + 1; j < len(normalized); j++ {
			for _, a := range normalized[i] {
				for _, b := range normalized[j] {
					if pathsOverlap(a, b) {
						return fmt.Errorf("task %d and task %d declare overlapping write paths %q and %q", i+1, j+1, a, b)
					}
				}
			}
		}
	}
	return nil
}

// validateWritePath normalizes one declared write path, rejecting the
// values that cannot be compared: an empty or whitespace-only path, an
// absolute path, a path that escapes the workspace, and "." declaring the
// whole workspace. The returned path is filepath.Clean'ed.
func validateWritePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("write path %q is empty or whitespace-only", value)
	}
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("write path %q is absolute", value)
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("write path %q escapes the workspace", value)
	}
	if clean == "." {
		return "", fmt.Errorf("write path %q declares the whole workspace", value)
	}
	return clean, nil
}

// pathsOverlap reports whether two cleaned workspace-relative paths
// overlap: equal, or one is a path prefix of the other on a segment
// boundary. Comparison uses segment splitting, so "src/a" and "src/ab"
// do not overlap while "src/a" and "src/a/b.go" do.
func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	aseg := strings.Split(a, string(filepath.Separator))
	bseg := strings.Split(b, string(filepath.Separator))
	if len(aseg) > len(bseg) {
		aseg, bseg = bseg, aseg
	}
	for i := range aseg {
		if aseg[i] != bseg[i] {
			return false
		}
	}
	return true
}

// gitMoved reports whether the git state moved in a way a bounded edit
// does not normally move: HEAD, the branch, or the stash list. A
// changing Dirty flag alone does not trigger the report; modified files
// are the point of a delegation.
func gitMoved(before, after *GitState) bool {
	return before.Head != after.Head ||
		before.Branch != after.Branch ||
		before.Stashes != after.Stashes
}

// aggregateStatus maps the run outcome onto the documented precedence
// order, using the parent context state after all workers have returned:
// an expired deadline is a timeout, a cancelled parent is a cancellation,
// then every-completed, at-least-one-completed, and otherwise failed.
func aggregateStatus(ctx context.Context, workers []pi.WorkerResult) contracts.RunStatus {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return contracts.RunTimedOut
	case errors.Is(ctx.Err(), context.Canceled):
		return contracts.RunCancelled
	}
	completed := 0
	for _, worker := range workers {
		if worker.Status == pi.StatusCompleted {
			completed++
		}
	}
	switch {
	case completed == len(workers):
		return contracts.RunCompleted
	case completed > 0:
		return contracts.RunPartial
	default:
		return contracts.RunFailed
	}
}
