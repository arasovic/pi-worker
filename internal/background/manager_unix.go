//go:build darwin || linux

package background

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/arasovic/pi-worker/internal/runlog"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// roleExecutable names the program a detached supervisor is spawned from:
// this program itself, because the supervisor is the same binary re-executed
// with a private role token.
func roleExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own executable: %w", err)
	}
	return executable, nil
}

// Start mints the run identity, prepares the worktree when one was asked
// for, and hands the run to a fresh detached supervisor, returning while
// the run is still going.
//
// The run is not started by this process and is not supervised by it: once
// Start returns, the detached supervisor owns the run, and every later state
// of that run — its terminal snapshot included — reaches the Store these
// roots name through that supervisor's own writes, which is why Status and
// Wait answer about a run this Manager never started.
//
// A start that is not accepted leaves nothing behind. The durable snapshot
// and the admission tickets are persisted by the supervisor before it accepts
// and rolled back by the handoff when it does not, so the only state this
// call itself creates is the worktree — and an unaccepted Start gives back
// exactly that one, never the caller's workspace, and never a checkout
// somebody has already touched, which is what the removal itself verifies.
// A worktree that Start could not give back is reported, so a leftover
// private checkout is never silent.
//
// Start waits for the acceptance handshake and no further: cancellation or
// its own bound arriving before the supervisor answers leaves the run
// unstarted, while a supervisor that already answered is never recalled and
// the run keeps going whether or not anything is still listening.
func (m *Manager) Start(ctx context.Context, opts StartOptions) (StartedRun, error) {
	executable := opts.RoleExecutable
	if executable == "" {
		resolved, err := roleExecutable()
		if err != nil {
			return StartedRun{}, fmt.Errorf("background manager start: %w", err)
		}
		executable = resolved
	}
	return m.startWithExecutable(ctx, executable, opts)
}

// startWithExecutable is Start over an explicit role executable. Production
// reaches it only through Start, which resolves this program; a test reaches
// it with the built shipped binary, because a test binary is not a program
// that dispatches role tokens.
func (m *Manager) startWithExecutable(ctx context.Context, executable string, opts StartOptions) (StartedRun, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.validateStartOptions(opts); err != nil {
		return StartedRun{}, err
	}

	// The handshake wait is bounded by what the caller asked for and by the
	// caller's own deadline, whichever is sooner. A bound that is already
	// spent is reported as the expired deadline it is, and never rounded up
	// into waiting longer than the caller said.
	budget := opts.QueueWait
	if budget <= 0 {
		budget = defaultStartWait
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < budget {
			budget = remaining
		}
	}
	if budget <= 0 {
		return StartedRun{}, fmt.Errorf("background manager start: %w", context.DeadlineExceeded)
	}

	acceptedAt := time.Now().UTC().Truncate(time.Second)
	req := supervisorStartRequest{
		runID:            runlog.RunID(acceptedAt),
		acceptedAt:       acceptedAt,
		workspace:        opts.Workspace,
		tasks:            opts.Tasks,
		verify:           opts.Verify,
		executionTimeout: opts.ExecutionTimeout,
		backgroundRoot:   m.root,
		admissionRoot:    m.admissionRoot,
		maxModelWorkers:  m.maxModelWorkers,
		piExecutable:     opts.PiExecutable,
		debug:            opts.Debug,
	}

	// The worktree is the one thing this call creates that the handoff
	// cannot roll back, so it is prepared first and given back on every
	// return that did not get the run accepted.
	remove := opts.resolver()
	giveBackWorktree := func() error { return nil }
	if opts.WorktreeName != "" {
		prepared, err := worktree.Prepare(ctx, opts.Workspace, opts.WorktreeName)
		if err != nil {
			return StartedRun{}, fmt.Errorf("background manager start: prepare worktree %q: %w", opts.WorktreeName, err)
		}
		req.worktree = &prepared
		req.workspace = prepared.Path
		giveBackWorktree = func() error {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeCleanupWait)
			defer cancel()
			if err := remove(cleanupCtx, opts.Workspace, prepared); err != nil {
				return fmt.Errorf("background manager start: remove the worktree %q prepared for a start that was not accepted: %w", prepared.Path, err)
			}
			return nil
		}
	}

	handoffCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	result, err := startSupervisorHandoff(handoffCtx, executable, req)
	if err != nil {
		// The handoff reported its own failure, unaccepted by definition:
		// its snapshot and tickets are already rolled back, so only the
		// worktree is left to give back, and its failure joins the cause.
		return StartedRun{}, joinStartErrors(err, giveBackWorktree())
	}
	if !result.accepted {
		return StartedRun{}, joinStartErrors(errSupervisorRejected, giveBackWorktree())
	}

	// Accepted: the snapshot and the tickets are durable and owned by the
	// run lifecycle, the worktree is where the run works, and the supervisor
	// is already detached, so nothing below may unwind any of it — not even
	// a cancellation arriving at this instant.
	return StartedRun{RunID: req.runID, Snapshot: result.snapshot}, nil
}
