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

// runsReportStartRunes is how many runes of a worker's wrap-up report the
// human block shows before it cuts the line short and points at --json for
// the rest. It is a rune count, not a byte count: a cut never splits a rune.
const runsReportStartRunes = 160

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
	snap, readErr, code := runsReadStatus(opts.runID, stderr)
	if readErr != nil {
		var unavailable *background.SupervisorUnavailableError
		if errors.As(readErr, &unavailable) {
			if code := renderRunsSnapshot(stdout, stderr, opts.json, snap, fmt.Sprintf(
				"pi-worker: runs status %s: supervisor is no longer there; the run was interrupted and will not finish",
				opts.runID)); code != 0 {
				return code
			}
			return 9
		}
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
	stopDebug := func() {}
	if opts.debug {
		stopDebug = followDebugLog(manager.DebugLogPath(opts.runID), stderr)
	}
	snap, err := manager.Wait(parent, opts.runID, opts.timeout)
	stopDebug()
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
		var unavailable *background.SupervisorUnavailableError
		if errors.As(err, &unavailable) {
			if code := renderRunsSnapshot(stdout, stderr, opts.json, snap, fmt.Sprintf(
				"pi-worker: runs wait %s: supervisor is no longer there; the run was interrupted and will not finish",
				opts.runID)); code != 0 {
				return code
			}
			return 9
		}
		if code := runsReadFailure(opts.runID, err, stderr); code != 0 {
			return code
		}
		return 9
	}
}

// runsCancelCommand requests cancellation without waiting for the supervisor
// to finish. Manager.Cancel performs the single durable read, protects the
// recorded PID with a process creation-time check, and sends only SIGTERM
// after that identity matches. No path here writes a snapshot.
func runsCancelCommand(parent context.Context, opts runsOptions, stdout, stderr io.Writer) int {
	if refused := runsRefuseUnsupportedPlatform(stderr); refused != 0 {
		return refused
	}
	if _, err := runlog.ParseRunID(opts.runID); err != nil {
		return runsUnknownRun(opts.runID, stderr)
	}
	manager, code := runsManager(stderr)
	if code != 0 {
		return code
	}
	snap, err := manager.Cancel(opts.runID)
	if err != nil {
		var unavailable *background.SupervisorUnavailableError
		if errors.As(err, &unavailable) {
			fmt.Fprintf(stderr, "pi-worker: runs cancel %s: supervisor is no longer there; the record cannot be finished from here\n", opts.runID)
			return 9
		}
		var signalFailure *background.SupervisorSignalError
		if errors.As(err, &signalFailure) {
			fmt.Fprintf(stderr, "pi-worker: runs cancel %s: could not request stop: %v\n", opts.runID, signalFailure)
			return 9
		}
		if code := runsReadFailure(opts.runID, err, stderr); code != 0 {
			return code
		}
		return 9
	}
	if snap.Terminal {
		if code := renderRunsSnapshot(stdout, stderr, opts.json, snap, ""); code != 0 {
			return code
		}
		return runsFinishedExitCode(snap)
	}
	if code := renderRunsSnapshot(stdout, stderr, opts.json, snap, fmt.Sprintf(
		"pi-worker: runs cancel %s stop requested; the run reports cancelled once it has finished stopping — runs wait follows it",
		opts.runID)); code != 0 {
		return code
	}
	return 0
}

// runsRenderWaited prints what a wait came back with and carries the exit
// code of the state that was printed: a finished run's own code, and the
// timeout code 7 for the live state a wait that ran out reports.
func runsRenderWaited(stdout, stderr io.Writer, opts runsOptions, snap background.Snapshot, note string) int {
	// A finished run with its result document prints what the same run
	// prints in the foreground; a wait that ran out, and a terminal state
	// with no result document, keep the table.
	if !opts.json && note == "" && snap.Terminal && snap.Result != nil {
		printRunResult(*snap.Result, stdout, stderr)
		return runsFinishedExitCode(snap)
	}
	if code := renderRunsSnapshot(stdout, stderr, opts.json, snap, note); code != 0 {
		return code
	}
	if note != "" {
		return contracts.ExitCode(contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout})
	}
	return runsFinishedExitCode(snap)
}

// runsReadStatus resolves the Manager and reads one run's latest durable
// snapshot, reporting a failure with its exit code already chosen. A dead
// supervisor is not a read failure: the snapshot comes back with the
// SupervisorUnavailableError so the caller can print the state it is.
func runsReadStatus(runID string, stderr io.Writer) (background.Snapshot, error, int) {
	manager, code := runsManager(stderr)
	if code != 0 {
		return background.Snapshot{}, nil, code
	}
	snap, err := manager.Status(runID)
	if err != nil {
		var unavailable *background.SupervisorUnavailableError
		if errors.As(err, &unavailable) {
			return snap, err, 9
		}
		if code := runsReadFailure(runID, err, stderr); code != 0 {
			return background.Snapshot{}, err, code
		}
		return background.Snapshot{}, err, 9
	}
	return snap, nil, 0
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
// the same result produces in the foreground. The stored outcome is the
// supervisor's one decision about what the run earned, including a
// controller error the result's fields alone do not show, so it is not
// recomputed from the result here: the printed outcome word and the exit
// code cannot disagree.
func runsFinishedExitCode(snap background.Snapshot) int {
	if snap.Outcome != nil {
		return contracts.OutcomeExitCode(*snap.Outcome)
	}
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
// verbatim with --json, and otherwise the short block `runs status`, `runs
// cancel` and an unfinished `runs wait` share — the run's identity and state,
// each worker's state, and for a finished run what each worker answered and
// the run's check lines. note is the one line a caller adds
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

	if extra := runsSnapshotExtraLines(snap); len(extra) > 0 {
		// One empty line separates the block from the table, and the block
		// exists only when a worker has a result the ANSWER column could
		// not carry. When there is nothing extra, the output is exactly
		// the table and the outcome line.
		fmt.Fprintln(stdout)
		for _, line := range extra {
			fmt.Fprintln(stdout, line)
		}
	}

	if snap.Terminal {
		// A finished run's check lines follow the table exactly as they
		// follow the worker lines in the foreground.
		if snap.Result != nil {
			printRunChecks(*snap.Result, stdout, stderr)
		}
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

// runsSnapshotExtraLines builds the lines that follow the human table for
// the workers whose result the ANSWER column cannot fully carry: each
// worker's warning, and, for a worker whose answer column shows its error
// rather than its wrap-up report, the start of that report. When any report
// was cut short, one final line points at the --json document that carries
// the whole text. The lines are in worker order; no lines are returned when
// there is nothing extra to show.
func runsSnapshotExtraLines(snap background.Snapshot) []string {
	var lines []string
	reportCut := false
	for _, worker := range snap.Workers {
		result := worker.Result
		if result == nil {
			continue
		}
		if result.Warning != "" {
			lines = append(lines, fmt.Sprintf("worker %d warning: %s", worker.WorkerID, result.Warning))
		}
		// The report is worth repeating only when the ANSWER column did not
		// already show it: runsWorkerAnswer falls to the report only when the
		// explanation is empty and the error is not.
		if result.PartialExplanation != "" && result.Explanation == "" && result.Error != "" {
			start, cut := runsReportStart(result.PartialExplanation)
			lines = append(lines, fmt.Sprintf("worker %d report: %s", worker.WorkerID, start))
			if cut {
				reportCut = true
			}
		}
	}
	if reportCut {
		lines = append(lines, fmt.Sprintf("full text: pi-worker runs status %s --json", snap.RunID))
	}
	return lines
}

// runsReportStart folds a report to one line the same way runsWorkerAnswer
// folds an answer, then cuts it to its first runsReportStartRunes runes. A
// cut never splits a rune, and the shown text is marked with an ellipsis. It
// reports whether the text was cut.
func runsReportStart(text string) (string, bool) {
	folded := strings.Join(strings.Fields(text), " ")
	runes := []rune(folded)
	if len(runes) <= runsReportStartRunes {
		return folded, false
	}
	return string(runes[:runsReportStartRunes]) + "\u2026", true
}

// runsWorkerAnswer says what one worker has to show for itself, for the
// human table: the answer a completed worker gave, the reason a worker that
// did not complete gave, and the text a worker that stopped early produced.
// A worker with no result yet is still running or queued; a running worker
// that has reported activity says so, in a fixed one-line form naming the
// latest event time, the tool-call count, and the tool running now when one
// is — a caller can then tell progress from a stall. A queued worker with no
// activity carries nothing here. Newlines are folded to spaces: the block is
// one line per worker, and `--json` carries the text verbatim.
func runsWorkerAnswer(worker background.WorkerSnapshot) string {
	if worker.Result == nil {
		if worker.Activity == nil {
			return ""
		}
		answer := "active " + worker.Activity.LastEventAt.UTC().Format(time.RFC3339) +
			fmt.Sprintf(", %d tool calls", worker.Activity.ToolCalls)
		if worker.Activity.LastTool != "" {
			answer += ", last " + worker.Activity.LastTool
		}
		return answer
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
