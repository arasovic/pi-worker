package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arasovic/pi-worker/internal/buildinfo"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/piversion"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// defaultRunTimeout bounds one foreground worker run.
const defaultRunTimeout = 30 * time.Minute

// newWorker is a private dependency-injection seam. Tests replace it with a
// scripted fake so CLI tests never launch the user's real Pi profile.
var newWorker = func() pi.Worker { return pi.New("pi") }

const runVersionProbeTimeout = 5 * time.Second

// runVersionProbe is called once for each run, before any worker starts.
// Tests replace it with a deterministic result; the production probe never
// exposes child output or stderr to callers.
var runVersionProbe = defaultRunVersionProbe

func defaultRunVersionProbe(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, runVersionProbeTimeout)
	defer cancel()
	return piversion.Probe(ctx, "pi")
}

// newCatalog is the private dependency-injection seam for the read-only
// model catalog command.
var newCatalog = func() pi.ModelCatalog { return pi.NewCatalog("pi") }

// runlogDir, runlogStart, and runlogInterrupted are the private
// dependency-injection seams for the run record written while a run is
// in flight and for the reader that scans earlier records for
// interrupted runs before a run starts. Tests replace them with a
// temporary directory and with scripted failures; the production
// values write and read records in the user's config directory.
var runlogDir = runlog.Dir
var runlogStart = runlog.Start
var runlogInterrupted = runlog.Interrupted

// runlogList is the private dependency-injection seam for the read-only
// runs list command. Tests replace it with a scripted failure; the
// production value reads the records newest first.
var runlogList = runlog.List

// stdinIsTerminal reports whether the command's stdin is an
// interactive terminal — the one question a bare io.Reader cannot
// answer, so runs prune asks it here instead of reaching for os.Stdin
// behind the caller's back. Tests replace it with a scripted answer;
// the production value asks os.Stdin itself. A Stat that errors means
// not a terminal, so the command refuses rather than prompting into a
// stream that will never answer — the same fail-safe direction the
// interrupted-run scan uses for its own uncertain check: both resolve
// doubt toward doing nothing.
var stdinIsTerminal = defaultStdinIsTerminal

func defaultStdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// shutdownSignals are the signals that request an orderly shutdown. SIGINT
// is the interactive Ctrl-C; SIGTERM is what process supervisors, container
// runtimes, and agent harness timeouts send first. Both must reach the run
// context, because an unhandled SIGTERM terminates the process immediately,
// skipping child termination and session directory removal.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// interruptContext installs the shutdown signal interception for one command
// and returns its context with the matching stop function. Commands share it
// so a new command cannot silently observe a narrower signal set.
func interruptContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), shutdownSignals...)
}

// Main runs the pi-worker command. Signal interception is installed only
// after the run arguments and the tasks are resolved: while pi-worker
// reads the task from stdin there is no child process and no
// cleanup-requiring work, so the first Ctrl-C must terminate promptly with
// the default interrupt behavior instead of being swallowed by a
// NotifyContext that the blocking read never observes.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		return versionCommand(args[1:], stdout, stderr)
	case "models":
		ctx, stop := interruptContext()
		defer stop()
		return modelsCommand(ctx, args[1:], stdout, stderr)
	case "doctor":
		ctx, stop := interruptContext()
		defer stop()
		return doctorCommand(ctx, args[1:], stdout, stderr)
	case "config":
		ctx, stop := interruptContext()
		defer stop()
		return configCommand(ctx, args[1:], stdout, stderr)
	case "run":
		opts, tasks, err := resolveRunInput(args[1:], stdin)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: %v\n", err)
			printUsage(stderr)
			return 2
		}
		ctx, stop := interruptContext()
		defer stop()
		return runCommand(ctx, opts, tasks, stdout, stderr)
	case "skill":
		ctx, stop := interruptContext()
		defer stop()
		return skillCommand(ctx, args[1:], stdout, stderr)
	case "runs":
		ctx, stop := interruptContext()
		defer stop()
		return runsCommand(ctx, args[1:], stdin, stdout, stderr)
	default:
		printUsage(stderr)
		return 2
	}
}

// mainWithContext is the private deterministic seam around Main minus signal
// handling. Tests drive cancellation deterministically through this seam
// without sending a real signal to the test process.
func mainWithContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		return versionCommand(args[1:], stdout, stderr)
	case "models":
		return modelsCommand(ctx, args[1:], stdout, stderr)
	case "doctor":
		return doctorCommand(ctx, args[1:], stdout, stderr)
	case "config":
		return configCommand(ctx, args[1:], stdout, stderr)
	case "run":
		opts, tasks, err := resolveRunInput(args[1:], stdin)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: %v\n", err)
			printUsage(stderr)
			return 2
		}
		return runCommand(ctx, opts, tasks, stdout, stderr)
	case "skill":
		return skillCommand(ctx, args[1:], stdout, stderr)
	case "runs":
		return runsCommand(ctx, args[1:], stdin, stdout, stderr)
	default:
		printUsage(stderr)
		return 2
	}
}

// resolveRunInput parses the run flags and resolves the task list. It runs
// before any signal interception: reading the task from stdin must keep the
// default interrupt behavior.
func resolveRunInput(args []string, stdin io.Reader) (runOptions, []run.Task, error) {
	opts, err := parseRunArgs(args)
	if err != nil {
		return opts, nil, err
	}
	// The run-level model falls back to the configured default, and only
	// when some task will need it: a task carrying --model of its own
	// never consults the run-level value. The flag-introduced task list
	// is known here; a prompt on stdin can never carry a per-task --model
	// — a --model before it is the run-level value, not its own — so a
	// stdin run always needs the run-level model.
	if !opts.modelSpecified && runNeedsModel(opts) {
		opts.model, err = configuredRunModel()
		if err != nil {
			return opts, nil, err
		}
	}
	tasks, err := resolveTasks(opts, stdin)
	if err != nil {
		return opts, nil, err
	}
	// --writes entries pair with the task most recently introduced when
	// they appeared. A --writes that appeared before every task was held
	// pending by the parser; it binds here, where the final task count is
	// known. Exactly one task makes its target unambiguous — it can only
	// be that task's declaration, and a prompt read from stdin has no
	// other way to declare at all. More than one task leaves the target
	// unknowable, so the run is rejected.
	if len(opts.writesPending) > 0 {
		if len(tasks) != 1 {
			return opts, nil, fmt.Errorf("--writes must follow the --task or --task-file it declares: with more than one task, a --writes that precedes them all is ambiguous; place each --writes directly after its task")
		}
		if len(opts.writesPending) > 1 || (len(opts.writes) > 0 && opts.writes[0].Declared) {
			return opts, nil, fmt.Errorf("--writes specified more than once for task 1")
		}
		pending := opts.writesPending[0]
		pending.Declared = true
		opts.writes = []run.WriteDeclaration{pending}
	}
	// Pad what was declared to the final task count so the declaration
	// pairs with the task list, with a zero-value entry — Declared false —
	// for every task that declared nothing.
	if opts.writes != nil {
		for len(opts.writes) < len(tasks) {
			opts.writes = append(opts.writes, run.WriteDeclaration{})
		}
	}
	// --data entries pair with the task most recently introduced when
	// they appeared, exactly like --writes. A --data that appeared before
	// every task was held pending by the parser; it binds here, where the
	// final task count is known: exactly one task makes its target
	// unambiguous — it can only be that task's declaration, and a prompt
	// read from stdin has no other way to declare at all — while more
	// than one task leaves the target unknowable, so the run is rejected.
	if len(opts.dataPending) > 0 {
		if len(tasks) != 1 {
			return opts, nil, fmt.Errorf("--data must follow the --task or --task-file it declares: with more than one task, a --data that precedes them all is ambiguous; place each --data directly after its task")
		}
		if len(opts.dataPending) > 1 || (len(opts.data) > 0 && opts.data[0].Declared) {
			return opts, nil, fmt.Errorf("--data specified more than once for task 1")
		}
		pending := opts.dataPending[0]
		pending.Declared = true
		opts.data = []run.DataDeclaration{pending}
	}
	// Pad what was declared to the final task count, exactly like the
	// writes padding above: a zero-value entry — Declared false — for
	// every task that declared nothing.
	if opts.data != nil {
		for len(opts.data) < len(tasks) {
			opts.data = append(opts.data, run.DataDeclaration{})
		}
	}
	// The pairing stops here: each declaration is bound into the task
	// record it belongs to, and nothing past this point indexes a task
	// and its writes by position. Model and thinking precedence — task,
	// then run level, then the configured default — resolves in the same
	// loop: the records carry the effective values, and the controller
	// makes no decision about defaults.
	records := make([]run.Task, len(tasks))
	for i, task := range tasks {
		records[i] = run.Task{Prompt: task, Model: opts.model, ThinkingLevel: opts.thinking}
		if i < len(opts.taskModels) && opts.taskModelSpecified[i] {
			records[i].Model = opts.taskModels[i]
		}
		if i < len(opts.taskThinking) && opts.taskThinking[i] != "" {
			records[i].ThinkingLevel = opts.taskThinking[i]
		}
		if opts.writes != nil {
			records[i].Writes = opts.writes[i]
		}
		// Every declared data file is read here, once, up front, before
		// any worker starts, in the same pass that validates the rest of
		// the argv: a missing, unreadable, or otherwise failing file is a
		// usage error like every other argv mistake and exits 2. Reading
		// up front is not a protection against anything; it is
		// determinism, so two tasks given the same path are guaranteed
		// the same bytes even if a worker rewrites the file mid-run.
		if opts.data != nil {
			material, err := readDataDeclaration(opts.data[i])
			if err != nil {
				return opts, nil, fmt.Errorf("task %d: %v", i+1, err)
			}
			records[i].Data = material
		}
	}
	// The write declaration is validated here, on the resolved records,
	// where every task's declaration is knowable: the overlap rule needs
	// the whole run's declarations together, so it cannot run during
	// parse. A bad declaration is a usage error like any other argv
	// mistake and exits 2, before the controller runs.
	if err := run.ValidateWrites(records); err != nil {
		return opts, nil, err
	}
	return opts, records, nil
}

// runNeedsModel reports whether some task will fall back to the run-level
// model, which must then resolve from the configured default when no
// run-level --model specified it. A prompt read from stdin cannot carry a
// per-task --model, so it always needs the run-level value; a
// flag-introduced task needs it unless a --model bound to it directly.
func runNeedsModel(opts runOptions) bool {
	if len(opts.tasks)+len(opts.taskFiles) == 0 {
		return true
	}
	for i := 0; i < len(opts.tasks)+len(opts.taskFiles); i++ {
		if i >= len(opts.taskModelSpecified) || !opts.taskModelSpecified[i] {
			return true
		}
	}
	return false
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: pi-worker version [--json]")
	fmt.Fprintln(w, "       pi-worker models [--timeout <duration>] [--json] [--debug]")
	fmt.Fprintln(w, "       pi-worker doctor [--timeout <duration>] [--json] [--debug]")
	fmt.Fprintln(w, "       pi-worker config show [--json]")
	fmt.Fprintln(w, "       pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]")
	fmt.Fprintln(w, "       pi-worker skill status [--json]")
	fmt.Fprintln(w, "       pi-worker skill receipt-path [--json]")
	fmt.Fprintln(w, "       pi-worker runs list [--json]")
	fmt.Fprintln(w, "       pi-worker runs prune --keep <n> [--yes] [--json]")
	fmt.Fprintln(w, "       pi-worker run [--task <prompt> | --task-file <path>]... [--model <provider/model>] [--thinking <level>] [--data <paths>] [--writes <paths>] [--timeout <duration>] [--verify <command>] [--json] [--debug]")
}

type versionOutput struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	BuildDate     string `json:"buildDate"`
}

func versionCommand(args []string, stdout, stderr io.Writer) int {
	jsonOutput := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--json":
		jsonOutput = true
	default:
		fmt.Fprintln(stderr, "pi-worker: invalid version syntax")
		printUsage(stderr)
		return 2
	}

	info := buildinfo.Current()
	if !jsonOutput {
		fmt.Fprintf(stdout, "pi-worker %s\n", info)
		return 0
	}
	data, err := json.Marshal(versionOutput{
		SchemaVersion: contracts.SchemaVersion,
		Version:       info.Version,
		Commit:        info.Commit,
		BuildDate:     info.BuildDate,
	})
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: encode version: %v\n", err)
		return 9
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// runOptions holds the parsed run command surface.
type runOptions struct {
	// model is the run-level value: a --model that appeared before any
	// --task or --task-file. modelSpecified reports whether such a
	// --model appeared: an explicitly empty value stays distinguishable
	// from "no value", because only the unspecified run-level model falls
	// back to the configured default.
	model          string
	modelSpecified bool
	// thinking is the run-level value: a --thinking that appeared before
	// any --task or --task-file. The empty level means unset; off is an
	// explicit level, never conflated with it.
	thinking  pi.ThinkingLevel
	tasks     []string
	taskFiles []string
	timeout   time.Duration
	verify    []string
	json      bool
	debug     bool
	// writes is the per-task declared write set in task order: nil when
	// no --writes appeared, and a zero-value entry — Declared false — for
	// a task that declared nothing. --writes "" fills a Declared true
	// entry with no paths, the writes-nothing declaration.
	writes []run.WriteDeclaration
	// writesPending holds --writes declarations that appeared before any
	// --task or --task-file, in order. They cannot be indexed at parse
	// time: the final task list is not known until resolveRunInput, and
	// a prompt on stdin is introduced only there. A one-task run binds
	// them to that single task; a run with more than one task rejects
	// them, because no task can be named.
	writesPending []run.WriteDeclaration
	// data is the per-task declared data file paths in task order: nil
	// when no --data appeared, and a zero-value entry — Declared false —
	// for a task that declared nothing. --data "" is rejected at parse
	// time, so a declared entry always carries at least one path.
	data []run.DataDeclaration
	// dataPending holds --data declarations that appeared before any
	// --task or --task-file, in order, exactly like writesPending: a
	// one-task run binds them to that single task; a run with more than
	// one task rejects them, because no task can be named.
	dataPending []run.DataDeclaration
	// taskModels holds the per-task --model values in task order: "" for
	// a task that specified none. taskModelSpecified marks each entry
	// actually specified, so a --model "" still counts as specified for
	// the once-per-task rule. taskThinking holds the per-task --thinking
	// levels in task order, the empty level for a task that specified
	// none; --thinking "" never lands here, parse rejects it.
	taskModels         []string
	taskModelSpecified []bool
	taskThinking       []pi.ThinkingLevel
}

// runCommand runs one to three parallel workers with an already-resolved
// task list. The run timeout is a single shared deadline on the caller's
// context covering every worker: Ctrl-C (or any parent cancellation)
// cancels the run immediately, and the timeout bounds it when no signal
// arrives. With --verify, a completed run's workspace is checked once
// before returning.
func runCommand(parent context.Context, opts runOptions, tasks []run.Task, stdout, stderr io.Writer) int {
	workspace, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine workspace: %v\n", err)
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
	}

	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()

	var debug *pi.DebugSink
	if opts.debug {
		debug = pi.NewDebugSink(stderr)
	}

	preflightPiVersion(ctx, stderr)

	if len(tasks) > 1 && !allWritesDeclared(tasks) {
		fmt.Fprintf(stderr, "pi-worker: warning: %d workers share the writable current workspace; tasks must use disjoint files\n", len(tasks))
	}

	// The run record is written while the run is in flight: the start
	// line reaches disk before any worker starts, and Finish appends the
	// finish line on the way out, so a run killed without warning —
	// Ctrl-C, timeout, a closed terminal, a killed supervisor — still
	// leaves its record with no finish line, which is how a later reader
	// recognizes the interruption. A record that cannot be written must
	// never fail or block a run: the warning is printed and the run
	// continues with no recorder, on which Finish and WorkerProcess are
	// no-ops.
	startedAt := time.Now()
	var recorder *runlog.Recorder
	dir, err := runlogDir()
	if err == nil {
		// Earlier runs are scanned for interruptions before this run's
		// own record exists, so the current run cannot block its own
		// watermark. A scan failure — an unreadable records directory
		// or an unwritable marker — is one warning in the existing
		// style, the interrupted runs the scan did find are still
		// printed, and the run continues: a record problem never fails
		// a run.
		paths, scanErr := runlogInterrupted(dir)
		if scanErr != nil {
			fmt.Fprintf(stderr, "pi-worker: warning: interrupted-run check unavailable: %v\n", scanErr)
		}
		warnInterruptedRuns(paths, dir, stderr)
		recorder, err = runlogStart(dir, startedAt, workspace, tasks)
	}
	if err != nil {
		recorder = nil
		fmt.Fprintf(stderr, "pi-worker: warning: run record unavailable: %v\n", err)
	}

	// Every run records the workspace git state before and after, so a
	// task that moves HEAD, the branch, or the stash list shows up in
	// the result whether or not --verify was passed; there is no flag
	// for this feature.
	var controller *run.Controller
	if len(opts.verify) > 0 {
		controller = run.New(newWorker(), run.WithVerifier(run.NewDefaultVerifier()), run.WithGitInspector(run.NewDefaultGitInspector()))
	} else {
		controller = run.New(newWorker(), run.WithGitInspector(run.NewDefaultGitInspector()))
	}
	result, err := controller.Run(ctx, run.Request{
		Tasks:     tasks,
		Workspace: workspace,
		Verify:    opts.verify,
		Debug:     debug,
		OnProcessStart: func(workerID int, pid int) {
			// The closure is safe on a nil recorder: WorkerProcess is a
			// no-op then, exactly like Finish.
			recorder.WorkerProcess(time.Now(), workerID, pid)
		},
	})
	finishedAt := time.Now()
	if err != nil {
		// The finish line carries the run-level error text, so a failed
		// run is still fully recorded; a record write failure is the
		// same kind of warning and changes no exit code.
		if recordErr := recorder.Finish(finishedAt, nil, err); recordErr != nil {
			fmt.Fprintf(stderr, "pi-worker: warning: run record unavailable: %v\n", recordErr)
		}
		// Defensive: the CLI validates the input surface first — the
		// write declaration through run.ValidateWrites in resolveRunInput
		// like every other field — so a controller validation error here
		// is unreachable from CLI input and really is an internal
		// failure. A verification context that expired mid-check is not
		// a check failure: the run ran out of time and exits like a
		// timed-out run.
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return contracts.ExitCode(contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout})
		case errors.Is(err, context.Canceled):
			return contracts.ExitCode(contracts.RunCancelled, &contracts.RunError{Kind: contracts.ErrorCancellation})
		default:
			return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
		}
	}

	outcome, code := runOutcome(result)
	result.Outcome = outcome
	// The finish line rides the normal path after the outcome is
	// assigned, so the record carries exactly what the run reports.
	if recordErr := recorder.Finish(finishedAt, &result, nil); recordErr != nil {
		fmt.Fprintf(stderr, "pi-worker: warning: run record unavailable: %v\n", recordErr)
	}
	for i, worker := range result.Workers {
		if worker.Warning != "" {
			fmt.Fprintf(stderr, "pi-worker: worker %d: %s\n", i+1, worker.Warning)
		}
	}

	if opts.json {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode result: %v\n", err)
			return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
		}
		fmt.Fprintln(stdout, string(data))
		if code != 0 {
			for i, worker := range result.Workers {
				if worker.Status != pi.StatusCompleted && worker.Error != "" {
					fmt.Fprintf(stderr, "pi-worker: worker %d: %s\n", i+1, worker.Error)
				}
			}
		}
		return code
	}

	for i, worker := range result.Workers {
		label := workerOutputLabel(i+1, worker)
		if worker.Status == pi.StatusCompleted {
			fmt.Fprintf(stdout, "%s: %s\n", label, worker.Explanation)
			continue
		}
		message := worker.Error
		if message == "" {
			message = worker.Explanation
		}
		if message == "" {
			message = fmt.Sprintf("status %q", worker.Status)
		}
		fmt.Fprintf(stderr, "pi-worker: %s: %s\n", label, message)
	}
	if result.Verification != nil {
		printVerification(result.Verification, stdout, stderr)
	}
	if result.Git != nil {
		printGitChange(result.Git, stderr)
	}
	if result.Changes != nil {
		printChanges(result.Changes, stdout)
	}
	if result.Writes != nil {
		// The verdict and the skip reason are result lines like the
		// change manifest and belong on stdout; the violation is the
		// failure exit 4 refers to and belongs on stderr.
		w := stdout
		if result.Writes.Skipped == "" && result.Writes.UndeclaredCount > 0 {
			w = stderr
		}
		printWrites(result.Writes, w)
	}
	fmt.Fprintf(stdout, "outcome=%s\n", result.Outcome)
	return code
}

// warnInterruptedRuns prints one stderr warning line per interrupted
// run the pre-run scan found, capped at five, and one summary line for
// the remainder. The full record path is the point of each line: the
// reader's model is "there are records, I will open one if I care",
// and a warning without a path is not a signal. The cap keeps a long
// interrupted history from flooding the terminal; the summary names
// the records directory so the caller knows where to look.
func warnInterruptedRuns(paths []string, dir string, stderr io.Writer) {
	shown := min(len(paths), 5)
	for _, path := range paths[:shown] {
		fmt.Fprintf(stderr, "pi-worker: warning: an earlier run was interrupted: %s\n", path)
	}
	if len(paths) > shown {
		fmt.Fprintf(stderr, "pi-worker: warning: %d more interrupted runs in %s\n", len(paths)-shown, dir)
	}
}

// printVerification prints the run-level verification outcome after the
// worker summaries in human mode. A passing check is one short line on
// stdout; a failing check reports the exit code and the captured excerpt
// on stderr, plus the full-log path when one was written. The run status
// stays unchanged: it describes worker outcomes only.
func printVerification(verification *run.Verification, stdout, stderr io.Writer) {
	if verification.ExitCode == 0 {
		fmt.Fprintln(stdout, "verification: ok")
		return
	}
	fmt.Fprintf(stderr, "pi-worker: verification failed with exit code %d\n", verification.ExitCode)
	if verification.Output != "" {
		fmt.Fprint(stderr, verification.Output)
		if !strings.HasSuffix(verification.Output, "\n") {
			fmt.Fprintln(stderr)
		}
	}
	if verification.LogFile != "" {
		fmt.Fprintf(stderr, "pi-worker: verification log: %s\n", verification.LogFile)
	}
}

// printGitChange prints the run-level git-state change after the worker
// summaries in human mode: one stderr warning line naming what moved —
// HEAD, the branch, or the stash entries — when a run changed the git
// state in a way a bounded edit does not normally move. HEAD is shown
// at git's default seven-character abbreviation; the full values ride
// in the --json result. The run status stays unchanged: this is a
// notification, not a restriction.
func printGitChange(change *run.GitChange, stderr io.Writer) {
	var parts []string
	before, after := change.Before, change.After
	if before.Head != after.Head {
		parts = append(parts, fmt.Sprintf("HEAD %s -> %s", gitHead(before.Head), gitHead(after.Head)))
	}
	if before.Branch != after.Branch {
		parts = append(parts, fmt.Sprintf("branch %s -> %s", gitValue(before.Branch), gitValue(after.Branch)))
	}
	if change.Stash != nil {
		if removed := gitStashEntries(change.Stash.Removed); removed != "" {
			parts = append(parts, "stash removed: "+removed)
		}
		if added := gitStashEntries(change.Stash.Added); added != "" {
			parts = append(parts, "stash added: "+added)
		}
	}
	if len(parts) == 0 {
		return
	}
	fmt.Fprintf(stderr, "pi-worker: warning: the run changed git state: %s\n", strings.Join(parts, ", "))
}

// gitStashEntries renders stash entries for the human warning, each as
// the sha at git's default seven-character abbreviation followed by a
// space and the subject. At most three entries are listed; the remainder
// is summarized as "and N more".
func gitStashEntries(entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	limit := min(len(entries), 3)
	parts := make([]string, 0, limit+1)
	for _, entry := range entries[:limit] {
		sha, subject, _ := strings.Cut(entry, " ")
		parts = append(parts, gitHead(sha)+" "+subject)
	}
	if len(entries) > limit {
		parts = append(parts, fmt.Sprintf("and %d more", len(entries)-limit))
	}
	return strings.Join(parts, "; ")
}

// printChanges prints the run's measured change manifest after the
// worker summaries in human mode: one information line on stdout — the
// same class as "verification: ok", never a warning — naming the file
// count and the summed added/deleted lines, then up to five paths most
// churn first, one per line indented two spaces, and a final indented
// line naming how many more remain when the list is longer. The trailing
// count is relative to TotalFiles, not to len(Files), so a truncated
// manifest reports the paths the entry cap dropped as well as the ones
// the five-line limit dropped. When at least one listed entry was
// already dirty before the run, the line appends a parenthesised count
// of them, "N already modified before the run": their counts are
// measured against the last commit rather than against the pre-run
// content, so they include work that was already there and the summed
// +added/-deleted would otherwise read inflated by the caller's own
// uncommitted work. When at least one listed entry carries
// NoFinalNewline, a second parenthesised count, "N without a final
// newline", follows, separated by a space: it is a measurement, never
// a verdict, and it counts only the listed entries exactly as the
// dirty-before count does. The clause and the +added/-deleted sums describe
// the listed entries: they count only what the capped Files list
// carries, while TotalFiles is the true count, so above the entry cap
// the two can disagree. The clause still belongs on the header rather
// than on the path lines: the path lines show at most the five most
// churned entries, so the header is the one place a count over all the
// listed entries stays visible when the five-line limit drops rows. A
// measured run that changed nothing prints the zero line alone; an omitted
// manifest prints the reason instead, so a human never has to guess
// whether "no changes" means measured-zero or not-measured.
func printChanges(changes *run.Changes, stdout io.Writer) {
	if changes.Omitted != "" {
		fmt.Fprintf(stdout, "changes: omitted: %s\n", changes.Omitted)
		return
	}
	added, deleted, dirtyBefore, noFinalNewline := 0, 0, 0, 0
	for _, file := range changes.Files {
		added += file.Added
		deleted += file.Deleted
		if file.DirtyBefore {
			dirtyBefore++
		}
		if file.NoFinalNewline {
			noFinalNewline++
		}
	}
	filesWord := "files"
	if changes.TotalFiles == 1 {
		filesWord = "file"
	}
	fmt.Fprintf(stdout, "changes: %d %s, +%d/-%d", changes.TotalFiles, filesWord, added, deleted)
	if dirtyBefore > 0 {
		fmt.Fprintf(stdout, " (%d already modified before the run)", dirtyBefore)
	}
	if noFinalNewline > 0 {
		fmt.Fprintf(stdout, " (%d without a final newline)", noFinalNewline)
	}
	fmt.Fprintln(stdout)
	shown := min(len(changes.Files), 5)
	for _, file := range changes.Files[:shown] {
		counts := fmt.Sprintf("+%d/-%d", file.Added, file.Deleted)
		if file.Binary {
			counts = "binary"
		}
		fmt.Fprintf(stdout, "  %s  %s\n", file.Path, counts)
	}
	if len(changes.Files) > shown {
		fmt.Fprintf(stdout, "  and %d more\n", changes.TotalFiles-shown)
	}
}

// printWrites prints the run's write check after the change manifest in
// human mode, mirroring printChanges against the same rules. A clean
// verdict prints one short "writes: ok" line on stdout — the same class
// as "verification: ok", and the whole point of the field: a caller
// must be able to see that the check ran and passed, not merely that
// nothing was said. A skipped check prints the reason instead, so a
// human never has to guess whether "no writes" means checked-clean or
// not-checked. A nil check prints nothing: the caller never declared and
// there is nothing to report. The violation goes to stderr, not stdout,
// because it is a failure and it is what exit 4 refers to: one count
// line, then up to five undeclared paths, two spaces indented, one per
// line, and a final indented line naming how many more remain when more
// were undeclared than were printed. The trailing count is relative to
// UndeclaredCount, not to len(Undeclared), so a truncated check reports
// the paths the entry cap dropped as well as the ones the five-line
// limit dropped: it tells the human how many undeclared paths they have
// not seen, which is the number they need.
func printWrites(check *run.WriteCheck, w io.Writer) {
	if check == nil {
		return
	}
	if check.Skipped != "" {
		fmt.Fprintf(w, "writes: skipped: %s\n", check.Skipped)
		return
	}
	if check.UndeclaredCount == 0 {
		fmt.Fprintln(w, "writes: ok")
		return
	}
	pathsWord := "paths"
	if check.UndeclaredCount == 1 {
		pathsWord = "path"
	}
	fmt.Fprintf(w, "pi-worker: write check failed: %d undeclared %s\n", check.UndeclaredCount, pathsWord)
	shown := min(len(check.Undeclared), 5)
	for _, path := range check.Undeclared[:shown] {
		fmt.Fprintf(w, "  %s\n", path)
	}
	if len(check.Undeclared) > shown {
		fmt.Fprintf(w, "  and %d more\n", check.UndeclaredCount-shown)
	}
}

// gitHead renders a commit hash for the human warning at git's default
// seven-character abbreviation, and an empty hash — an unborn branch
// has no HEAD — as (none), the same placeholder gitValue uses for the
// branch field.
func gitHead(value string) string {
	if value == "" {
		return "(none)"
	}
	if len(value) > 7 {
		return value[:7]
	}
	return value
}

// gitValue renders an empty git identity in the human warning: an unborn
// branch has no HEAD, and a detached HEAD has no branch.
func gitValue(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

// allWritesDeclared reports whether every task carried a --writes
// declaration. Only then is the shared-workspace warning suppressed:
// the caller stated the disjoint-file contract for the whole run and
// the controller checked it before any worker started. A declared empty
// set counts as declared — the task bounded itself to nothing — while
// any task that did not declare keeps the warning.
func allWritesDeclared(tasks []run.Task) bool {
	if len(tasks) == 0 {
		return false
	}
	for _, task := range tasks {
		if !task.Writes.Declared {
			return false
		}
	}
	return true
}

func preflightPiVersion(ctx context.Context, stderr io.Writer) {
	output, err := runVersionProbe(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "pi-worker: warning: Pi version could not be verified; continuing")
		return
	}
	classification := piversion.Classify(output)
	switch classification.Status {
	case piversion.StatusVerified:
		return
	case piversion.StatusUnverified:
		fmt.Fprintf(stderr, "pi-worker: warning: Pi version %s is unverified; verified version is %s; continuing\n", classification.Version, piversion.VerifiedVersion)
	default:
		fmt.Fprintln(stderr, "pi-worker: warning: Pi version output could not be verified; continuing")
	}
}

func workerOutputLabel(index int, worker pi.WorkerResult) string {
	if worker.ThinkingLevel == "" {
		return fmt.Sprintf("worker %d", index)
	}
	return fmt.Sprintf("worker %d [model=%s thinking=%s]", index, worker.Model, worker.ThinkingLevel)
}

func parseRunArgs(args []string) (runOptions, error) {
	opts := runOptions{timeout: defaultRunTimeout}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--timeout", "--verify":
			if !hasValue {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			if name == "--verify" {
				argv, err := parseVerifyCommand(value)
				if err != nil {
					return opts, err
				}
				opts.verify = argv
			} else {
				duration, err := time.ParseDuration(value)
				if err != nil {
					return opts, fmt.Errorf("invalid timeout %q: %v", value, err)
				}
				if duration <= 0 {
					return opts, fmt.Errorf("invalid timeout %q: must be positive", value)
				}
				opts.timeout = duration
			}
		case "--model", "--thinking":
			// Positional, in exactly the way --writes is: a --model or
			// --thinking that follows a --task or --task-file binds to
			// that task, at most once per task. Unlike --writes, one that
			// precedes every task is NOT ambiguous and NOT rejected: it is
			// the run-level value, which is what it means today, at most
			// once each.
			if !hasValue {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			index := len(opts.tasks) + len(opts.taskFiles) - 1
			if index < 0 {
				// The run-level value: the current meaning of a flag that
				// precedes every task, at most once each.
				if name == "--model" {
					if opts.modelSpecified {
						return opts, fmt.Errorf("flag %s specified more than once", name)
					}
					opts.model = value
					opts.modelSpecified = true
				} else {
					if opts.thinking != "" {
						return opts, fmt.Errorf("flag %s specified more than once", name)
					}
					level, ok := pi.ParseThinkingLevel(value)
					if !ok {
						return opts, fmt.Errorf("invalid thinking level %q: expected off, minimal, low, medium, high, xhigh, or max", value)
					}
					opts.thinking = level
				}
				break
			}
			// After a task: binds to that task, at most once per task.
			if name == "--model" {
				for len(opts.taskModels) <= index {
					opts.taskModels = append(opts.taskModels, "")
					opts.taskModelSpecified = append(opts.taskModelSpecified, false)
				}
				if opts.taskModelSpecified[index] {
					return opts, fmt.Errorf("--model specified more than once for task %d", index+1)
				}
				opts.taskModels[index] = value
				opts.taskModelSpecified[index] = true
			} else {
				level, ok := pi.ParseThinkingLevel(value)
				if !ok {
					return opts, fmt.Errorf("invalid thinking level %q: expected off, minimal, low, medium, high, xhigh, or max", value)
				}
				for len(opts.taskThinking) <= index {
					opts.taskThinking = append(opts.taskThinking, "")
				}
				if opts.taskThinking[index] != "" {
					return opts, fmt.Errorf("--thinking specified more than once for task %d", index+1)
				}
				opts.taskThinking[index] = level
			}
		case "--task", "--task-file":
			// Repeatable: one occurrence per accepted task.
			if !hasValue {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			if name == "--task" {
				opts.tasks = append(opts.tasks, value)
			} else {
				opts.taskFiles = append(opts.taskFiles, value)
			}
		case "--writes":
			// Positional: applies to the task most recently introduced by
			// --task or --task-file, at most once per task. One exception:
			// a --writes that appears before any task cannot be indexed
			// here — the task list is not known at parse time, and the task
			// may even be a prompt arriving on stdin later — so it is held
			// pending and bound in resolveRunInput, where the final task
			// count decides: one task makes it that task's declaration,
			// more than one makes it ambiguous and rejected.
			if !hasValue {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			declaration, err := parseWritesDeclaration(value)
			if err != nil {
				return opts, err
			}
			index := len(opts.tasks) + len(opts.taskFiles) - 1
			if index < 0 {
				opts.writesPending = append(opts.writesPending, declaration)
				break
			}
			for len(opts.writes) <= index {
				opts.writes = append(opts.writes, run.WriteDeclaration{})
			}
			if opts.writes[index].Declared {
				return opts, fmt.Errorf("--writes specified more than once for task %d", index+1)
			}
			declaration.Declared = true
			opts.writes[index] = declaration
		case "--data":
			// Positional, exactly like --writes: applies to the task most
			// recently introduced by --task or --task-file, at most once
			// per task. One exception, again like --writes: a --data that
			// appears before any task cannot be indexed here — the task
			// list is not known at parse time, and the task may even be a
			// prompt arriving on stdin later — so it is held pending and
			// bound in resolveRunInput, where the final task count
			// decides: one task makes it that task's declaration, more
			// than one makes it ambiguous and rejected. Unlike --writes,
			// an empty value is rejected here: omitting the flag already
			// means "no material", so --data "" has no "carries nothing"
			// meaning to declare.
			if !hasValue {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			declaration, err := parseDataDeclaration(value)
			if err != nil {
				return opts, err
			}
			index := len(opts.tasks) + len(opts.taskFiles) - 1
			if index < 0 {
				opts.dataPending = append(opts.dataPending, declaration)
				break
			}
			for len(opts.data) <= index {
				opts.data = append(opts.data, run.DataDeclaration{})
			}
			if opts.data[index].Declared {
				return opts, fmt.Errorf("--data specified more than once for task %d", index+1)
			}
			declaration.Declared = true
			opts.data[index] = declaration
		case "--json", "--debug":
			if hasValue {
				return opts, fmt.Errorf("flag %s does not take a value", name)
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			if name == "--json" {
				opts.json = true
			} else {
				opts.debug = true
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown flag %q", arg)
			}
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}
	if opts.modelSpecified {
		if err := validateModel(opts.model); err != nil {
			return opts, err
		}
	}
	// validateModel runs on every model that arrives on the command line:
	// the run-level value and every per-task value alike, with the same
	// error text.
	for i, model := range opts.taskModels {
		if !opts.taskModelSpecified[i] {
			continue
		}
		if err := validateModel(model); err != nil {
			return opts, err
		}
	}
	if len(opts.tasks) > 0 && len(opts.taskFiles) > 0 {
		return opts, fmt.Errorf("specify exactly one input source: --task or --task-file, not both")
	}
	if count := len(opts.tasks) + len(opts.taskFiles); count > run.MaxTasks {
		return opts, fmt.Errorf("at most %d tasks per run, got %d", run.MaxTasks, count)
	}
	for i, task := range opts.tasks {
		if strings.TrimSpace(task) == "" {
			return opts, fmt.Errorf("task %d is empty", i+1)
		}
	}
	for i, path := range opts.taskFiles {
		if strings.TrimSpace(path) == "" {
			return opts, fmt.Errorf("task file %d path is empty", i+1)
		}
	}
	return opts, nil
}

// parseWritesDeclaration parses a --writes value into the declaration it
// carries. A trimmed-empty value is the one spelling that cannot collide
// with a real path: it is how a task declares that it writes nothing.
// Only a non-empty value is split on commas, so an empty element between
// commas keeps failing below.
func parseWritesDeclaration(value string) (run.WriteDeclaration, error) {
	var declaration run.WriteDeclaration
	if strings.TrimSpace(value) != "" {
		paths := strings.Split(value, ",")
		for i, path := range paths {
			// Trim surrounding whitespace immediately, before every other
			// check: "docs/a.md, src/x" and "docs/a.md,src/x" must reach
			// validation as the same paths. A trimmed-empty element still
			// gets the existing empty-element error below.
			paths[i] = strings.TrimSpace(path)
			if paths[i] == "" {
				return declaration, fmt.Errorf("invalid writes %q: empty element between commas", value)
			}
		}
		declaration.Paths = paths
	}
	return declaration, nil
}

// parseDataDeclaration parses a --data value into the declaration it
// carries. Unlike --writes, a trimmed-empty value is a usage error:
// omitting the flag already means "no material", so the empty spelling
// has no "carries nothing" meaning. The value is split on commas, each
// element trimmed; an empty element between commas keeps failing below.
func parseDataDeclaration(value string) (run.DataDeclaration, error) {
	var declaration run.DataDeclaration
	if strings.TrimSpace(value) == "" {
		return declaration, fmt.Errorf("invalid data %q: empty value", value)
	}
	paths := strings.Split(value, ",")
	for i, path := range paths {
		// Trim surrounding whitespace immediately, before every other
		// check: "docs/a.md, src/x" and "docs/a.md,src/x" must arrive at
		// the file read as the same paths. A trimmed-empty element still
		// gets the existing empty-element error below.
		paths[i] = strings.TrimSpace(path)
		if paths[i] == "" {
			return declaration, fmt.Errorf("invalid data %q: empty element between commas", value)
		}
	}
	declaration.Paths = paths
	return declaration, nil
}

// parseVerifyCommand validates the raw --verify value and splits it on
// whitespace into argv. pi-worker never runs a shell and never assembles
// a command string, so shell metacharacters cannot work: a value
// containing any of them is rejected with a usage error naming the
// offending character rather than silently mis-executed. An empty or
// whitespace-only value is rejected too.
func parseVerifyCommand(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("invalid verify command %q: empty or whitespace-only", value)
	}
	for _, char := range value {
		if strings.ContainsRune("|&;<>$`\n'\"\\", char) {
			return nil, fmt.Errorf("invalid verify command %q: shell character %q is not supported", value, char)
		}
	}
	return strings.Fields(value), nil
}

func validateModel(model string) error {
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("invalid model %q: expected exact provider/model", model)
	}
	if strings.ContainsAny(model, ": \t\r\n") {
		return fmt.Errorf("invalid model %q: exact provider/model required, no pattern or thinking suffix", model)
	}
	return nil
}

// resolveTasks resolves the accepted task list: task values as-is, task
// files read in order (each must be non-empty), or one task read from
// stdin when neither flag was given.
func resolveTasks(opts runOptions, stdin io.Reader) ([]string, error) {
	if len(opts.tasks) > 0 {
		return append([]string(nil), opts.tasks...), nil
	}
	if len(opts.taskFiles) > 0 {
		tasks := make([]string, 0, len(opts.taskFiles))
		for _, path := range opts.taskFiles {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read task file: %v", err)
			}
			if strings.TrimSpace(string(data)) == "" {
				return nil, fmt.Errorf("task file %q is empty", path)
			}
			tasks = append(tasks, string(data))
		}
		return tasks, nil
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read prompt from stdin: %v", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, fmt.Errorf("no prompt provided on stdin")
	}
	return []string{string(data)}, nil
}

// readDataDeclaration reads every file a --data declaration names, once,
// up front, before any worker starts. A missing or unreadable file is a
// usage error returned to resolveRunInput like every other argv mistake,
// so it exits 2. The content is attached as-is: no limit is imposed on
// size or count, because pi-worker cannot know the caller's budget,
// model, or context window. Absolute paths are allowed — --data reads a
// file rather than declaring one, and the material usually sits in a
// temp directory outside the workspace.
func readDataDeclaration(declaration run.DataDeclaration) ([]run.DataFile, error) {
	if !declaration.Declared {
		return nil, nil
	}
	files := make([]run.DataFile, 0, len(declaration.Paths))
	for _, path := range declaration.Paths {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read data file %q: %v", path, err)
		}
		files = append(files, run.DataFile{Path: path, Content: content})
	}
	return files, nil
}

// runFailure resolves which (status, error kind) pair describes the
// aggregate run result, in the documented precedence order. It is the
// single place that decision is made: the word and the exit code are
// both derived from what it returns, so they cannot describe
// different things. The codes follow the documented precedence order:
// run-outcome codes first (5, 7, 8, 9, and the readiness 3), then the
// write-check policy code 4, then the verification code 6, then
// completion's 0. A no-success run exits 3 when every worker was
// unavailable and 9 when any worker reported an internal error; partial
// runs stay 5. The run status field always describes worker outcomes
// only.
func runFailure(result run.Result) (contracts.RunStatus, *contracts.RunError) {
	switch result.Status {
	case contracts.RunTimedOut:
		return contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout}
	case contracts.RunCancelled:
		return contracts.RunCancelled, &contracts.RunError{Kind: contracts.ErrorCancellation}
	}
	// Partial and failed runs keep their run-outcome codes before any
	// check is considered: a run that did not complete is answered by
	// its outcome, not by the checks that run on every terminal status.
	if result.Status != contracts.RunCompleted {
		hasSuccess := false
		hasError := false
		allUnavailable := true
		for _, worker := range result.Workers {
			switch worker.Status {
			case pi.StatusCompleted:
				hasSuccess = true
			case pi.StatusError:
				hasError = true
				allUnavailable = false
			case pi.StatusUnavailable:
			default:
				allUnavailable = false
			}
		}
		switch {
		case !hasSuccess && allUnavailable:
			return result.Status, &contracts.RunError{Kind: contracts.ErrorReadiness}
		case !hasSuccess && hasError:
			return result.Status, &contracts.RunError{Kind: contracts.ErrorInternal}
		default:
			return result.Status, &contracts.RunError{Kind: contracts.ErrorTask}
		}
	}
	// Completed only. The write contract outranks the quality signal: a
	// run that wrote outside its declared scope has breached the
	// contract the caller relied on to bound it, and whether its tests
	// pass is secondary information the result document carries either
	// way. Contract breach outranks quality signal. Only a verdict with
	// undeclared paths exits 4: a skipped check never does — a skip
	// means the question could not be answered, and answering
	// "violation" would be a lie — and a clean verdict never does.
	if result.Writes != nil && result.Writes.Skipped == "" && result.Writes.UndeclaredCount > 0 {
		return contracts.RunCompleted, &contracts.RunError{Kind: contracts.ErrorPolicy}
	}
	if result.Verification != nil && result.Verification.ExitCode != 0 {
		return contracts.RunCompleted, &contracts.RunError{Kind: contracts.ErrorVerification}
	}
	return contracts.RunCompleted, nil
}

// runOutcome returns both the self-describing word and the contract
// exit code for the aggregate run result, both derived in one place
// from the (status, error kind) pair resolved by runFailure.
func runOutcome(result run.Result) (contracts.Outcome, int) {
	status, runError := runFailure(result)
	return contracts.RunOutcome(status, runError), contracts.ExitCode(status, runError)
}
