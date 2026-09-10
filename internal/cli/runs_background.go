package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// backgroundSupportsRuns is the seam both read commands ask whether this
// platform can host a background run at all. Tests replace it to observe the
// refusal on a platform that can; production always answers with the
// platform's own capability, which is the same answer Start reaches by.
var backgroundSupportsRuns = background.SupportsBackgroundRuns

// defaultRunsWaitTimeout bounds a `runs wait` that was given no --timeout of
// its own. It is the same length as a foreground run's default execution
// bound, because that is the wait this command stands in for: a caller who
// would have typed `pi-worker run ...` and sat there types this and waits
// here, for as long as a run of that default length could take. It is a wait
// budget only: expiring it never cancels, kills, or otherwise touches the
// run, and the run it was waiting for keeps going.
const defaultRunsWaitTimeout = 30 * time.Minute

// runsStatusCommand reports where one background run stands and returns. It
// waits for nothing: one read of the run's latest durable snapshot is the
// whole command, so the state it prints of a run in flight is the state that
// was durable at the moment it read, and asking again prints whatever has
// become durable since.
//
// A run that has finished exits with the code the same result produces in
// the foreground — the snapshot carries the run's own outcome, and
// internal/contracts maps it — and a run still going exits 0, because
// nothing about it failed: it is merely unfinished.
func runsStatusCommand(parent context.Context, opts runsOptions, stdout, stderr io.Writer) int {
	if refused := runsRefuseUnsupportedPlatform(stderr); refused != 0 {
		return refused
	}
	if _, err := runlog.ParseRunID(opts.runID); err != nil {
		return runsUnknownRun(opts.runID, stderr)
	}
	snap, code := runsReadStatus(opts.runID, stderr)
	if code != 0 {
		return code
	}
	if code := renderRunsSnapshot(stdout, stderr, opts.json, snap, ""); code != 0 {
		return code
	}
	if !snap.Terminal {
		// A run still going is a report, not a failure: the status
		// command asked one question and answered it.
		return 0
	}
	return runsFinishedExitCode(snap)
}

// runsWaitCommand waits for one background run to finish and prints its
// result. Waiting is reading and nothing else: the command never cancels,
// never kills, and never attaches to the run it waits for.
//
// When its own bound arrives first it prints the latest state it read, says
// on stderr that the wait ran out, and leaves the run alone — the run keeps
// going and finishes on its own, and its exit is the timeout code the run
// contract already reserves for a wait that expired (7). A run that finished
// within the bound exits with the code that same result produces in the
// foreground.
func runsWaitCommand(parent context.Context, opts runsOptions, stdout, stderr io.Writer) int {
	if refused := runsRefuseUnsupportedPlatform(stderr); refused != 0 {
		return refused
	}
	manager, code := runsManager(stderr)
	if code != 0 {
		return code
	}
	if _, err := runlog.ParseRunID(opts.runID); err != nil {
		return runsUnknownRun(opts.runID, stderr)
	}
	snap, err := manager.Wait(parent, opts.runID, opts.timeout)
	switch {
	case err == nil:
		return runsRenderWaited(stdout, stderr, opts, snap, "")
	case errors.Is(err, context.DeadlineExceeded):
		// The wait ran out. Whatever the latest read said is printed as
		// the state it is — a live run's state, never a run that failed —
		// and only the exit code and the stderr line say the wait is over.
		return runsRenderWaited(stdout, stderr, opts, snap, fmt.Sprintf(
			"pi-worker: runs wait %s ran out after %s; the run is still going and was not cancelled or touched",
			opts.runID, opts.timeout))
	case errors.Is(err, context.Canceled):
		fmt.Fprintf(stderr, "pi-worker: runs wait %s cancelled\n", opts.runID)
		return contracts.ExitCode(contracts.RunCancelled, &contracts.RunError{Kind: contracts.ErrorCancellation})
	default:
		if code := runsReadFailure(opts.runID, err, stderr); code != 0 {
			return code
		}
		return 9
	}
}

// runsRenderWaited prints what a wait came back with and carries the exit
// code of the state that was printed: a finished run's own code, and 0 for
// the live state a wait that ran out reports.
func runsRenderWaited(stdout, stderr io.Writer, opts runsOptions, snap background.Snapshot, note string) int {
	if code := renderRunsSnapshot(stdout, stderr, opts.json, snap, note); code != 0 {
		return code
	}
	if note != "" {
		return contracts.ExitCode(contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout})
	}
	return runsFinishedExitCode(snap)
}

// runsReadStatus resolves the Manager and reads one run's latest durable
// snapshot exactly once, reporting a failure with its exit code already
// chosen.
func runsReadStatus(runID string, stderr io.Writer) (background.Snapshot, int) {
	manager, code := runsManager(stderr)
	if code != 0 {
		return background.Snapshot{}, code
	}
	snap, err := manager.Status(runID)
	if err != nil {
		if code := runsReadFailure(runID, err, stderr); code != 0 {
			return background.Snapshot{}, code
		}
		return background.Snapshot{}, 9
	}
	return snap, 0
}

// runsManager resolves the settings a background run would have used and
// hands them to the same Manager seam `run --background` constructs through,
// so a status or a wait reads the state at the roots the run wrote it to. The
// settings are resolved, not guessed: a run's state lives beside the
// configuration the run itself was started from.
func runsManager(stderr io.Writer) (*background.Manager, int) {
	settings, err := configuredRunSettings()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return nil, 9
	}
	manager, err := newBackgroundManager(settings.admissionRoot, settings.maxModelWorkers)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return nil, contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
	}
	return manager, 0
}

// runsRefuseUnsupportedPlatform refuses both commands where no background run
// can exist: the role processes that would have executed a run cannot start
// here, so no run's state can be read, and the commands say so instead of
// answering about a run that never had one. The refusal carries exit 9 — the
// code `run --background` already exits with on such a platform, because it
// is the same reason, reported the same way. A non-zero code on a path that
// reads nothing and writes no document is what keeps it a refusal and not a
// report of some state.
func runsRefuseUnsupportedPlatform(stderr io.Writer) int {
	if backgroundSupportsRuns() {
		return 0
	}
	fmt.Fprintln(stderr, "pi-worker: background runs are not supported on this platform")
	return 9
}

// runsUnknownRun reports a run identity no run is known by as the usage error
// it is, naming the identity the caller passed. An identity that cannot be a
// run identity at all and an identity that names no stored run are the same
// mistake from the caller's side — a name to fix — so they get the same
// message, and neither is reported as a state of anything.
func runsUnknownRun(runID string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "pi-worker: unknown run %q: no background run is recorded under that identity\n", runID)
	return 2
}

// runsReadFailure chooses the exit code of a read that came back with an
// error, and reports it. An unreadable run is a usage error when it is a
// missing run: the identity the caller gave names nothing, and the directory
// that does not exist is not the run's state being broken. Everything else —
// a snapshot that cannot be decoded, a root that cannot be inspected — is an
// internal failure, because a run whose state nobody can read is not a run
// that is merely still going.
func runsReadFailure(runID string, err error, stderr io.Writer) int {
	if errors.Is(err, fs.ErrNotExist) {
		return runsUnknownRun(runID, stderr)
	}
	fmt.Fprintf(stderr, "pi-worker: read background run %s: %v\n", runID, err)
	return 9
}

// runsFinishedExitCode maps a terminal snapshot's own outcome onto the code
// the same result produces in the foreground. The snapshot carries the run
// result the supervisor wrote, so no second decision about what the run
// earned is made here: the same mapping a foreground run exits through is
// applied to that document.
func runsFinishedExitCode(snap background.Snapshot) int {
	if snap.Result != nil {
		_, code := runOutcome(*snap.Result)
		return code
	}
	// A terminal snapshot without a result document still names its run
	// status and its outcome, and the status alone is what the contract
	// maps a run-level code from.
	if snap.Status != nil {
		return contracts.ExitCode(*snap.Status, nil)
	}
	return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal})
}

// renderRunsSnapshot prints one snapshot: the documented background document
// verbatim with --json, and otherwise the short block `runs status` and `runs
// wait` share — the run's identity and state, each worker's state, and for a
// finished run what each worker answered. note is the one line a caller adds
// for itself, and it goes to stderr: a wait that ran out says so beside the
// state it printed, where a machine reading stdout is unaffected.
func renderRunsSnapshot(stdout, stderr io.Writer, jsonOutput bool, snap background.Snapshot, note string) int {
	if jsonOutput {
		data, err := json.Marshal(snap)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode run %s: %v\n", snap.RunID, err)
			return 9
		}
		fmt.Fprintln(stdout, string(data))
		if note != "" {
			fmt.Fprintln(stderr, note)
		}
		return 0
	}

	tab := tabwriter.NewWriter(stdout, 0, 4, 4, ' ', 0)
	fmt.Fprintf(tab, "RUN ID\tSTARTED\tUPDATED\tSTATE\tWORKERS\tWORKSPACE\n")
	fmt.Fprintf(tab, "%s\t%s\t%s\t%s\t%d\t%s\n",
		snap.RunID,
		snap.AcceptedAt.Format(time.RFC3339),
		snap.UpdatedAt.Format(time.RFC3339),
		snap.State,
		len(snap.Workers),
		snap.Workspace)
	fmt.Fprintf(tab, "WORKER\tSTATE\tMODEL\tANSWER\n")
	for _, worker := range snap.Workers {
		fmt.Fprintf(tab, "%d\t%s\t%s\t%s\n", worker.WorkerID, worker.State, worker.Task.Model, runsWorkerAnswer(worker))
	}
	tab.Flush()

	if snap.Terminal {
		// The outcome line is the same word the foreground run prints, and
		// it is the word the exit code the command returns was taken from.
		outcome := ""
		if snap.Outcome != nil {
			outcome = string(*snap.Outcome)
		} else if snap.Result != nil {
			outcome = string(snap.Result.Outcome)
		}
		if outcome != "" {
			fmt.Fprintf(stdout, "outcome=%s\n", outcome)
		}
	}
	if note != "" {
		fmt.Fprintln(stderr, note)
	}
	return 0
}

// runsWorkerAnswer says what one worker has to show for itself, for the
// human table: the answer a completed worker gave, the reason a worker that
// did not complete gave, and the text a worker that stopped early produced.
// An answer that is still streaming is not an answer yet, so a worker with no
// result carries nothing here — its state column already says `queued` or
// `running`. Newlines are folded to spaces: the block is one line per worker,
// and `--json` carries the text verbatim.
func runsWorkerAnswer(worker background.WorkerSnapshot) string {
	if worker.Result == nil {
		return ""
	}
	answer := worker.Result.Explanation
	if answer == "" {
		answer = worker.Result.Error
	}
	if answer == "" {
		answer = worker.Result.PartialExplanation
	}
	return strings.Join(strings.Fields(answer), " ")
}
