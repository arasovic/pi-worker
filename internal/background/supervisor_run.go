package background

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/shirou/gopsutil/v4/process"
)

// supervisorLaunch is what the run observer learned about one worker while
// its process was still alive: the identity of the launched process, sampled
// from the process table at that moment, and the moment the start was
// observed. A launch whose identity the process table could not answer
// carries no process at all rather than an invented one.
type supervisorLaunch struct {
	process   *ProcessIdentity
	startedAt time.Time
}

// supervisorLaunches records the launches of one run. The controller calls
// the observer from worker goroutines, so every read and write is
// serialized on mu.
type supervisorLaunches struct {
	mu      sync.Mutex
	workers map[int]supervisorLaunch
}

func newSupervisorLaunches() *supervisorLaunches {
	return &supervisorLaunches{workers: make(map[int]supervisorLaunch)}
}

// observer returns the pi.ProcessObserver handed to every worker. The pid it
// is given names a process that is alive right now, which is the only moment
// its creation time can be sampled truthfully.
func (l *supervisorLaunches) observer() pi.ProcessObserver {
	return func(workerID, pid int) {
		launch := supervisorLaunch{startedAt: time.Now().UTC()}
		if created, err := supervisorPidCreateTime(pid); err == nil {
			launch.process = &ProcessIdentity{PID: pid, CreateTime: created}
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.workers[workerID] = launch
	}
}

// get returns the recorded launch for a worker ID, and whether one exists.
func (l *supervisorLaunches) get(workerID int) (supervisorLaunch, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	launch, ok := l.workers[workerID]
	return launch, ok
}

// supervisorPidCreateTime returns the process-table creation time of pid in
// milliseconds. A launch can never be recorded with an invented time: the
// process table must answer, and answer positively, for the identity to exist.
func supervisorPidCreateTime(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d", pid)
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, fmt.Errorf("observe process %d: %w", pid, err)
	}
	created, err := p.CreateTime()
	if err != nil {
		return 0, fmt.Errorf("observe process %d creation time: %w", pid, err)
	}
	if created <= 0 {
		return 0, fmt.Errorf("observe process %d: creation time %d is not positive", pid, created)
	}
	return created, nil
}

// runAcceptedRun drives an accepted supervisor start result to its terminal
// snapshot through the production private worker host. executable names the
// same-binary role executable that spawns worker-host children.
func runAcceptedRun(ctx context.Context, executable string, result supervisorStartResult) error {
	return runAcceptedRunWith(ctx, newWorkerHostAdapter(executable, result.request.piExecutable), result)
}

// runAcceptedRunWith drives an accepted supervisor start result through
// worker in exactly three steps: promote the persisted accepted Snapshot to
// running, run the controller over the tickets this run's acceptance already
// prepared, then persist the terminal Snapshot. One Replace per phase.
//
// The caller must hand over an accepted result: scheduling a preparation
// nobody accepted would release leases it never owned. The prepared tickets
// belong to the controller from step 2 on — this function never cancels
// them.
//
// The terminal snapshot is written on every path of an accepted result —
// including the ones where the running snapshot could not be persisted and
// where controller.Run returned an error — because a supervisor that exits
// without one leaves a run nobody can ever finish. Only what failed is
// reported: a nil return means the terminal snapshot is durable.
func runAcceptedRunWith(ctx context.Context, worker pi.Worker, result supervisorStartResult) error {
	if !result.accepted || result.preparation == nil {
		return fmt.Errorf("run accepted run (%s): start result is not accepted", result.request.runID)
	}
	req, prep, store := result.request, result.preparation, result.preparation.store
	var errs []error

	// Step 1 — the accepted Snapshot becomes the running Snapshot. A
	// running phase that cannot be persisted leaves the accepted Snapshot
	// durable and the tickets still prepared, so the run is still started
	// and still settled: an accepted snapshot needs no promotion to carry
	// a terminal result.
	base := prep.snapshot
	running, err := markAcceptedRunRunning(store, prep.snapshot)
	if err != nil {
		errs = append(errs, fmt.Errorf("run accepted run (%s): %w", req.runID, err))
	} else {
		base = running
	}

	// Step 2 — the controller runs the accepted tasks exactly the way a
	// foreground run does, except that admission uses the prepared tickets
	// instead of enqueuing a second set, and each launch is observed while
	// the process is alive. req.workspace already names the private checkout
	// when one was prepared. req.debug is not a debug sink, so Debug stays
	// nil.
	launches := newSupervisorLaunches()
	options := []run.Option{run.WithGitInspector(run.NewDefaultGitInspector())}
	if len(req.verify) > 0 {
		options = append(options, run.WithVerifier(run.NewDefaultVerifier()))
	}
	options = append(options, run.WithPreparedAdmission(req.runID, req.acceptedAt, req.executionTimeout, prep.tickets))
	runResult, runErr := run.New(worker, options...).Run(ctx, run.Request{
		Tasks:          req.tasks,
		Workspace:      req.workspace,
		Verify:         req.verify,
		OnProcessStart: launches.observer(),
	})
	if runErr != nil {
		errs = append(errs, fmt.Errorf("run accepted run (%s): controller: %w", req.runID, runErr))
	}

	// Step 3 — the terminal Snapshot. The phase before it is already
	// durable, so it is the base even when the controller returned no
	// result at all.
	terminal, terminalErr := buildTerminalRunSnapshot(base, runResult, launches, runErr)
	if terminalErr != nil {
		err := fmt.Errorf("run accepted run (%s): build terminal snapshot: %w", req.runID, terminalErr)
		errs = append(errs, err)
		cause := joinSupervisorStartErrors(runErr, err)
		if writeErr := writeFailedTerminalSnapshot(store, base, cause); writeErr != nil {
			errs = append(errs, writeErr)
		}
		return joinSupervisorStartErrors(errs...)
	}
	if err := store.Replace(terminal); err != nil {
		errs = append(errs, fmt.Errorf("run accepted run (%s): replace terminal snapshot: %w", req.runID, err))
	}
	return joinSupervisorStartErrors(errs...)
}

// markAcceptedRunRunning copies the accepted Snapshot, promotes the run and
// every worker to the running state, and persists it through store. It
// returns the promoted Snapshot only once it is durable.
//
// Validate requires a running worker to carry a startedAt and a process
// identity, and no worker process exists yet at this point: admission has not
// even granted the first lease. The identity this snapshot supervises the run
// with is the accepted snapshot's supervisor — a real, live process that owns
// every launch to come — and step 3 records what the observer measured in its
// place.
func markAcceptedRunRunning(store *Store, accepted Snapshot) (Snapshot, error) {
	running := accepted
	running.State = RunRunning
	running.Terminal = false
	running.UpdatedAt = time.Now().UTC()

	identity := accepted.Supervisor
	if identity.PID <= 0 || identity.CreateTime <= 0 {
		// An accepted snapshot always carries a positive supervisor
		// identity; when it somehow does not, this process is observed
		// instead, and a process table that cannot answer fails the phase
		// rather than persisting an invented identity.
		created, err := supervisorPidCreateTime(os.Getpid())
		if err != nil {
			return Snapshot{}, fmt.Errorf("mark run running: %w", err)
		}
		identity = ProcessIdentity{PID: os.Getpid(), CreateTime: created}
	}
	for i := range running.Workers {
		worker := &running.Workers[i]
		started := running.UpdatedAt
		worker.State = WorkerRunning
		worker.StartedAt = &started
		worker.FinishedAt = nil
		worker.Result = nil
		provisional := identity
		worker.Process = &provisional
	}

	if err := store.Replace(running); err != nil {
		return Snapshot{}, fmt.Errorf("mark run running: %w", err)
	}
	return running, nil
}

// buildTerminalRunSnapshot derives the terminal Snapshot from what the
// controller returned: the run status, outcome and full result on top, and
// each worker's result, terminal state and observed launch underneath.
//
// A non-nil controller error decides the run status on its own, the way the
// CLI maps the same error to its exit code, because a run whose controller
// failed is recorded by how it failed. The controller fills no outcome word —
// the CLI assigns that after Run returns — so an outcome the controller did
// not name is derived here through the same contracts.RunOutcome call, and
// whichever value is used is carried into the stored result copy, which keeps
// the snapshot's three views of the run consistent.
func buildTerminalRunSnapshot(running Snapshot, runResult run.Result, launches *supervisorLaunches, runErr error) (Snapshot, error) {
	status := runResult.Status
	if runErr != nil {
		status = failedRunStatus(runErr)
	}
	state, err := terminalRunState(status)
	if err != nil {
		return Snapshot{}, err
	}
	if len(runResult.Workers) != len(running.Workers) {
		return Snapshot{}, fmt.Errorf("controller returned %d worker results for %d workers", len(runResult.Workers), len(running.Workers))
	}
	outcome := runResult.Outcome
	if !outcomeSet(outcome) {
		outcome = contracts.RunOutcome(status, supervisorRunError(status, runErr))
	}

	now := time.Now().UTC()
	terminal := running
	terminal.State = state
	terminal.Terminal = true
	terminal.UpdatedAt = now
	result := runResult
	result.Status = status
	result.Outcome = outcome
	terminal.Status = &status
	terminal.Outcome = &outcome
	terminal.Result = &result

	for i := range terminal.Workers {
		worker := &terminal.Workers[i]
		workerState, err := terminalWorkerState(runResult.Workers[i].Status)
		if err != nil {
			return Snapshot{}, fmt.Errorf("worker %d: %w", i+1, err)
		}
		worker.State = workerState
		worker.Result = &runResult.Workers[i]
		// A terminal snapshot records only what the observer witnessed: the
		// running phase's provisional identity is dropped here, because a
		// worker nobody saw launch has no process to name and no start to
		// report, and the moment the controller returned stands in for the
		// missing observation.
		started := now
		var observed *ProcessIdentity
		if launch, ok := launches.get(worker.WorkerID); ok {
			started = launch.startedAt
			observed = launch.process
		}
		worker.Process = observed
		worker.StartedAt = &started
		worker.FinishedAt = &now
	}
	return terminal, nil
}

// writeFailedTerminalSnapshot persists a terminal Snapshot for a run whose
// result could not be carried into one — the controller returned nothing
// usable, or what it returned cannot name a snapshot. Every worker is
// reported as an error carrying the cause, under the status that cause
// derives.
func writeFailedTerminalSnapshot(store *Store, running Snapshot, cause error) error {
	status := failedRunStatus(cause)
	message := "run ended without a controller result"
	if cause != nil {
		message = cause.Error()
	}
	workers := make([]pi.WorkerResult, len(running.Workers))
	for i := range workers {
		workers[i] = pi.WorkerResult{
			Model:  running.Workers[i].Task.Model,
			Status: string(WorkerError),
			Error:  message,
		}
	}
	terminal, err := buildTerminalRunSnapshot(running, run.Result{
		SchemaVersion: contracts.SchemaVersion,
		Status:        status,
		Workers:       workers,
	}, newSupervisorLaunches(), cause)
	if err != nil {
		return fmt.Errorf("run accepted run (%s): build failed terminal snapshot: %w", running.RunID, err)
	}
	if err := store.Replace(terminal); err != nil {
		return fmt.Errorf("run accepted run (%s): replace failed terminal snapshot: %w", running.RunID, err)
	}
	return nil
}

// failedRunStatus maps a controller failure onto the run status the CLI
// would report for the same error: an expired deadline is a timed-out run, a
// cancelled one is a cancelled run, and anything else failed.
func failedRunStatus(runErr error) contracts.RunStatus {
	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		return contracts.RunTimedOut
	case errors.Is(runErr, context.Canceled):
		return contracts.RunCancelled
	default:
		return contracts.RunFailed
	}
}

// supervisorRunError pairs a derived status with the error kind the CLI
// would report for it, so the persisted outcome names the same reason the
// returned error carries.
func supervisorRunError(status contracts.RunStatus, runErr error) *contracts.RunError {
	if runErr == nil {
		return nil
	}
	kind := contracts.ErrorInternal
	switch status {
	case contracts.RunTimedOut:
		kind = contracts.ErrorTimeout
	case contracts.RunCancelled:
		kind = contracts.ErrorCancellation
	}
	return &contracts.RunError{Kind: kind, Message: runErr.Error()}
}

// terminalRunState maps a run status onto the terminal RunState. The two
// share their string values, but an unmapped value would persist a snapshot
// no reader can validate, so it is refused here instead.
func terminalRunState(status contracts.RunStatus) (RunState, error) {
	switch state := RunState(string(status)); state {
	case RunCompleted, RunPartial, RunFailed, RunTimedOut, RunCancelled:
		return state, nil
	default:
		return "", fmt.Errorf("unexpected run status %q", string(status))
	}
}

// terminalWorkerState maps one worker result status onto the terminal
// WorkerState, refusing anything a snapshot cannot carry.
func terminalWorkerState(status string) (WorkerState, error) {
	switch state := WorkerState(status); state {
	case WorkerCompleted, WorkerFailed, WorkerTimedOut, WorkerCancelled, WorkerUnavailable, WorkerError:
		return state, nil
	default:
		return "", fmt.Errorf("unexpected worker status %q", status)
	}
}
