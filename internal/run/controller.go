// Package run coordinates bounded parallel worker slices: up to three
// foreground workers execute accepted tasks concurrently in one shared
// workspace, and the controller aggregates their outcomes into one run
// result.
package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

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
}

// Controller runs accepted tasks concurrently through one Worker and,
// when a verifier is configured, checks a completed run's workspace
// with one command.
type Controller struct {
	worker   pi.Worker
	verifier Verifier
}

// New returns a controller that runs accepted tasks through worker.
// When a verifier is supplied, the controller also verifies a completed
// run's workspace with it; without one, Run behaves exactly as before.
func New(worker pi.Worker, verifier ...Verifier) *Controller {
	c := &Controller{worker: worker}
	if len(verifier) > 0 {
		c.verifier = verifier[0]
	}
	return c
}

// Run validates the request before starting any worker, starts every
// accepted task concurrently with the SAME parent context passed to Run,
// waits for every started worker before returning (including after
// cancellation), and aggregates the run status from the parent context
// state and the worker outcomes. When a verifier and a verification
// command are configured and the run completed with the parent context
// intact, Run verifies the workspace once before returning.
func (c *Controller) Run(ctx context.Context, req Request) (Result, error) {
	if err := validate(req); err != nil {
		return Result{}, err
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
// a non-empty workspace, between 1 and MaxTasks tasks, and no empty task
// after trimming whitespace.
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
	return nil
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
