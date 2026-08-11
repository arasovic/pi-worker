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

	"pi-worker/internal/contracts"
	"pi-worker/internal/pi"
)

// maxTasks is the absolute cap on accepted tasks per run.
const maxTasks = 3

// Request describes one bounded parallel run: every accepted task runs
// concurrently through the same worker with the same model and workspace.
type Request struct {
	Model         string
	ThinkingLevel pi.ThinkingLevel
	Tasks         []string
	Workspace     string
	// Debug is the shared run-level sink every worker labels with its own
	// identity; nil disables all debug logging.
	Debug *pi.DebugSink
}

// Result is the aggregate outcome of one run. Workers preserve input
// order regardless of completion order.
type Result struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Status        contracts.RunStatus `json:"status"`
	Workers       []pi.WorkerResult   `json:"workers"`
}

// Controller runs accepted tasks concurrently through one Worker.
type Controller struct {
	worker pi.Worker
}

// New returns a controller that runs accepted tasks through worker.
func New(worker pi.Worker) *Controller {
	return &Controller{worker: worker}
}

// Run validates the request before starting any worker, starts every
// accepted task concurrently with the SAME parent context passed to Run,
// waits for every started worker before returning (including after
// cancellation), and aggregates the run status from the parent context
// state and the worker outcomes.
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
	return Result{
		SchemaVersion: contracts.SchemaVersion,
		Status:        aggregateStatus(ctx, results),
		Workers:       results,
	}, nil
}

// validate checks the request before any worker starts: a non-empty model,
// a non-empty workspace, between 1 and maxTasks tasks, and no empty task
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
	if len(req.Tasks) > maxTasks {
		return fmt.Errorf("at most %d tasks are supported, got %d", maxTasks, len(req.Tasks))
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
