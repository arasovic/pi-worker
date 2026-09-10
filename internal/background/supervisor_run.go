package background

import (
	"context"
	"errors"
	"fmt"
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

// supervisorRunObserver is the run controller's process observer and the one
// writer of every non-terminal snapshot of one run. It holds the last
// snapshot this run made durable, the launches it observed, and every
// persistence failure it could not return, all under mu: the controller calls
// the observer from worker goroutines, so a launch is recorded and promoted
// in one serialized step.
//
// The in-memory snapshot is never a hypothesis about the run. It changes only
// after a Replace succeeded, so it always mirrors the durable file, and no
// snapshot it hands out shares its worker slice with a value it keeps: the
// only writes to a promoted snapshot happen on a copy made under mu before
// that copy becomes durable.
type supervisorRunObserver struct {
	mu       sync.Mutex
	store    *Store
	durable  Snapshot
	launched map[int]supervisorLaunch
	recorded []error
}

// newSupervisorRunObserver returns the observer for one accepted run. The
// durable Snapshot it starts from is the one acceptance persisted; store may
// be nil for an observer that records launches without persisting anything.
func newSupervisorRunObserver(store *Store, accepted Snapshot) *supervisorRunObserver {
	workers := make([]WorkerSnapshot, len(accepted.Workers))
	copy(workers, accepted.Workers)
	durable := accepted
	durable.Workers = workers
	return &supervisorRunObserver{store: store, durable: durable, launched: make(map[int]supervisorLaunch)}
}

// observer returns the pi.ProcessObserver handed to every worker. The pid it
// is given names a process that is alive right now, which is the only moment
// its creation time can be sampled truthfully.
func (o *supervisorRunObserver) observer() pi.ProcessObserver {
	return func(workerID, pid int) {
		// Sampled before mu is taken: the identity is only knowable while the
		// process is alive, and a lock held by another launch must never be
		// what delays the observation.
		launch := supervisorLaunch{startedAt: time.Now().UTC()}
		created, err := supervisorPidCreateTime(pid)
		if err == nil {
			launch.process = &ProcessIdentity{PID: pid, CreateTime: created}
		}

		o.mu.Lock()
		defer o.mu.Unlock()
		if launch.process == nil {
			// No identity exists, so this worker cannot be reported as
			// running: it stays queued in the durable snapshot and the reason
			// is recorded instead.
			o.noteFailure(fmt.Errorf("observe worker %d launch: %w", workerID, err))
			return
		}
		o.launched[workerID] = launch
		if o.store == nil {
			return
		}

		pending := o.snapshotLocked()
		workers := make([]WorkerSnapshot, len(pending.Workers))
		copy(workers, pending.Workers)
		pending.Workers = workers

		now := launch.startedAt
		pending.State = RunRunning
		pending.Terminal = false
		pending.UpdatedAt = now
		for i := range pending.Workers {
			if pending.Workers[i].WorkerID != workerID {
				continue
			}
			pending.Workers[i].State = WorkerRunning
			pending.Workers[i].StartedAt = &now
			pending.Workers[i].Process = launch.process
			pending.Workers[i].FinishedAt = nil
			pending.Workers[i].Result = nil
		}
		if err := o.store.Replace(pending); err != nil {
			// The worker really did launch and its identity is known, so the
			// launch stays recorded; only the promotion is not durable yet,
			// and the next launch retries it over the same base.
			o.noteFailure(fmt.Errorf("persist running snapshot for worker %d: %w", workerID, err))
			return
		}
		o.durable = pending
	}
}

// current returns the last snapshot this run made durable, or the accepted
// snapshot it started from when nothing was persisted since.
func (o *supervisorRunObserver) current() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	snap := o.snapshotLocked()
	workers := make([]WorkerSnapshot, len(snap.Workers))
	copy(workers, snap.Workers)
	snap.Workers = workers
	return snap
}

// launch returns the recorded launch for a worker ID, and whether one exists.
func (o *supervisorRunObserver) launch(workerID int) (supervisorLaunch, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	launch, ok := o.launched[workerID]
	return launch, ok
}

// failures returns every persistence failure the observer recorded, including
// the creation-time lookups that left a worker queued. The observer answers
// the controller with nothing, so this is where those failures surface.
func (o *supervisorRunObserver) failures() []error {
	o.mu.Lock()
	defer o.mu.Unlock()
	errs := make([]error, len(o.recorded))
	copy(errs, o.recorded)
	return errs
}

// noteFailure records a failure the observer cannot return. o.mu must be held.
func (o *supervisorRunObserver) noteFailure(err error) {
	o.recorded = append(o.recorded, err)
}

// snapshotLocked reads the durable snapshot. o.mu must be held, and the
// result shares its worker slice with what the observer keeps: callers that
// mutate must copy first.
func (o *supervisorRunObserver) snapshotLocked() Snapshot {
	return o.durable
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
// worker in two persisted phases: whatever the process observer writes while
// the run is in flight — one Replace per observed launch, promoting the run
// from accepted to running — and then the terminal Snapshot.
//
// The caller must hand over an accepted result: scheduling a preparation
// nobody accepted would release leases it never owned. The prepared tickets
// belong to the controller from phase one on — this function never cancels
// them.
//
// The terminal snapshot is written on every path of an accepted result —
// including the ones where no launch could be persisted and where
// controller.Run returned an error — because a supervisor that exits without
// one leaves a run nobody can ever finish. Only what failed is reported: a
// nil return means the terminal snapshot is durable.
func runAcceptedRunWith(ctx context.Context, worker pi.Worker, result supervisorStartResult) error {
	if !result.accepted || result.preparation == nil {
		return fmt.Errorf("run accepted run (%s): start result is not accepted", result.request.runID)
	}
	req, prep, store := result.request, result.preparation, result.preparation.store
	var errs []error

	// Step 1 — the controller runs the accepted tasks exactly the way a
	// foreground run does, except that admission uses the prepared tickets
	// instead of enqueuing a second set, and each launch is persisted by the
	// observer while its process is alive. The run stays accepted until a
	// worker process exists to make it running. req.workspace already names
	// the private checkout when one was prepared, and req.debug is not a
	// debug sink, so Debug stays nil.
	observer := newSupervisorRunObserver(store, prep.snapshot)
	options := []run.Option{run.WithGitInspector(run.NewDefaultGitInspector())}
	if len(req.verify) > 0 {
		options = append(options, run.WithVerifier(run.NewDefaultVerifier()))
	}
	options = append(options, run.WithPreparedAdmission(req.runID, req.acceptedAt, req.executionTimeout, prep.tickets))
	runResult, runErr := run.New(worker, options...).Run(ctx, run.Request{
		Tasks:          req.tasks,
		Workspace:      req.workspace,
		Verify:         req.verify,
		OnProcessStart: observer.observer(),
	})
	if runErr != nil {
		errs = append(errs, fmt.Errorf("run accepted run (%s): controller: %w", req.runID, runErr))
	}
	// What the observer could not persist is a failure of this run even
	// though the run itself settled: a worker whose launch never reached disk
	// is a fact the snapshot does not carry.
	errs = append(errs, observerFailures(req.runID, observer.failures())...)

	// Step 2 — the terminal Snapshot. Whatever the observer made durable is
	// its base; when the observer persisted nothing, the accepted snapshot is.
	base := observer.current()
	terminal, terminalErr := buildTerminalRunSnapshot(base, runResult, observer, runErr)
	if terminalErr != nil {
		err := fmt.Errorf("run accepted run (%s): build terminal snapshot: %w", req.runID, terminalErr)
		errs = append(errs, err)
		cause := joinSupervisorStartErrors(runErr, err)
		if writeErr := writeFailedTerminalSnapshot(observer, cause); writeErr != nil {
			errs = append(errs, writeErr)
		}
		return joinSupervisorStartErrors(errs...)
	}
	if err := store.Replace(terminal); err != nil {
		errs = append(errs, fmt.Errorf("run accepted run (%s): replace terminal snapshot: %w", req.runID, err))
	}
	return joinSupervisorStartErrors(errs...)
}

// observerFailures words the observer's recorded failures for the run that
// produced them, without losing the causes the observer already named.
func observerFailures(runID string, errs []error) []error {
	out := make([]error, 0, len(errs))
	for _, err := range errs {
		out = append(out, fmt.Errorf("run accepted run (%s): %w", runID, err))
	}
	return out
}

// buildTerminalRunSnapshot derives the terminal Snapshot from what the
// controller returned: the run status, outcome and full result on top, and
// each worker's result, terminal state and observed launch underneath.
//
// A non-nil controller error decides the run status on its own, the way the
// CLI maps the same error to its exit code, because a run whose controller
// failed is recorded by how it failed, and it decides the outcome word too:
// the controller's own outcome value is never kept, because the controller
// fills none. Without a controller error the result answers for itself
// through run.RunFailure, the function the CLI derives its word and exit code
// from, so a run whose verification failed or whose writes strayed outside
// the declared paths persists what a foreground run would have reported
// rather than its status word alone. Whichever value is used is carried into
// the stored result copy, which keeps the snapshot's three views of the run
// consistent.
func buildTerminalRunSnapshot(base Snapshot, runResult run.Result, launches *supervisorRunObserver, runErr error) (Snapshot, error) {
	status := runResult.Status
	var runError *contracts.RunError
	if runErr != nil {
		status, runError = failedRunStatus(runErr), supervisorRunError(failedRunStatus(runErr), runErr)
	} else {
		status, runError = run.RunFailure(runResult)
	}
	if len(runResult.Workers) != len(base.Workers) {
		return Snapshot{}, fmt.Errorf("controller returned %d worker results for %d workers", len(runResult.Workers), len(base.Workers))
	}
	state, err := terminalRunState(status)
	if err != nil {
		return Snapshot{}, err
	}
	outcome := contracts.RunOutcome(status, runError)

	now := time.Now().UTC()
	terminal := base
	workers := make([]WorkerSnapshot, len(base.Workers))
	copy(workers, base.Workers)
	terminal.Workers = workers
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
		// A terminal snapshot records only what the observer witnessed. A
		// worker nobody saw launch has no start to report and no process to
		// name: Validate's terminal rule requires finishedAt alone, so both
		// stay nil rather than taking the moment this function ran or the
		// supervisor's identity as substitutes.
		var started *time.Time
		var observed *ProcessIdentity
		if launch, ok := launches.launch(worker.WorkerID); ok {
			startedAt := launch.startedAt
			started = &startedAt
			observed = launch.process
		}
		worker.Process = observed
		worker.StartedAt = started
		worker.FinishedAt = &now
	}
	return terminal, nil
}

// writeFailedTerminalSnapshot persists a terminal Snapshot for a run whose
// result could not be carried into one — the controller returned nothing
// usable, or what it returned cannot name a snapshot. Every worker is
// reported as an error carrying the cause, under the status that cause
// derives. The observer whose run failed supplies the base snapshot and the
// launches already recorded, so nothing already durable is rewritten.
func writeFailedTerminalSnapshot(observer *supervisorRunObserver, cause error) error {
	running := observer.current()
	store := observer.store
	if store == nil {
		return fmt.Errorf("run accepted run (%s): replace failed terminal snapshot: no store", running.RunID)
	}
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
	}, observer, cause)
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
