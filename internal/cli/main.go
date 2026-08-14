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
	default:
		printUsage(stderr)
		return 2
	}
}

// resolveRunInput parses the run flags and resolves the task list. It runs
// before any signal interception: reading the task from stdin must keep the
// default interrupt behavior.
func resolveRunInput(args []string, stdin io.Reader) (runOptions, []string, error) {
	opts, err := parseRunArgs(args)
	if err != nil {
		return opts, nil, err
	}
	if !opts.modelSpecified {
		opts.model, err = configuredRunModel()
		if err != nil {
			return opts, nil, err
		}
	}
	tasks, err := resolveTasks(opts, stdin)
	if err != nil {
		return opts, nil, err
	}
	return opts, tasks, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: pi-worker version [--json]")
	fmt.Fprintln(w, "       pi-worker models [--timeout <duration>] [--json] [--debug]")
	fmt.Fprintln(w, "       pi-worker doctor [--timeout <duration>] [--json] [--debug]")
	fmt.Fprintln(w, "       pi-worker config show [--json]")
	fmt.Fprintln(w, "       pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]")
	fmt.Fprintln(w, "       pi-worker skill status [--json]")
	fmt.Fprintln(w, "       pi-worker skill receipt-path [--json]")
	fmt.Fprintln(w, "       pi-worker run [--model <provider/model>] [--thinking <level>] [--task <prompt> | --task-file <path>]... [--timeout <duration>] [--verify <command>] [--json] [--debug]")
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
	model          string
	modelSpecified bool
	thinking       pi.ThinkingLevel
	tasks          []string
	taskFiles      []string
	timeout        time.Duration
	verify         []string
	json           bool
	debug          bool
}

// runCommand runs one to three parallel workers with an already-resolved
// task list. The run timeout is a single shared deadline on the caller's
// context covering every worker: Ctrl-C (or any parent cancellation)
// cancels the run immediately, and the timeout bounds it when no signal
// arrives. With --verify, a completed run's workspace is checked once
// before returning.
func runCommand(parent context.Context, opts runOptions, tasks []string, stdout, stderr io.Writer) int {
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

	if len(tasks) > 1 {
		fmt.Fprintf(stderr, "pi-worker: warning: %d workers share the writable current workspace; tasks must use disjoint files\n", len(tasks))
	}

	var controller *run.Controller
	if len(opts.verify) > 0 {
		controller = run.New(newWorker(), run.NewDefaultVerifier())
	} else {
		controller = run.New(newWorker())
	}
	result, err := controller.Run(ctx, run.Request{
		Model:         opts.model,
		ThinkingLevel: opts.thinking,
		Tasks:         tasks,
		Workspace:     workspace,
		Verify:        opts.verify,
		Debug:         debug,
	})
	if err != nil {
		// Defensive: the CLI validates the input surface first, so a
		// controller validation error here is an internal failure. A
		// verification context that expired mid-check is not a check
		// failure: the run ran out of time and exits like a timed-out
		// run.
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

	code := runExitCode(result)
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
	return code
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
		case "--model", "--thinking", "--timeout", "--verify":
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
			if name == "--model" {
				opts.model = value
				opts.modelSpecified = true
			} else if name == "--thinking" {
				level, ok := pi.ParseThinkingLevel(value)
				if !ok {
					return opts, fmt.Errorf("invalid thinking level %q: expected off, minimal, low, medium, high, xhigh, or max", value)
				}
				opts.thinking = level
			} else if name == "--verify" {
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

// runExitCode maps the aggregate run result onto the contract exit codes.
// A no-success run exits 3 when every worker was unavailable and 9 when
// any worker reported an internal error; partial runs stay 5. A
// verification that ran and reported a non-zero exit code exits 6; the
// run status field still describes worker outcomes only.
func runExitCode(result run.Result) int {
	if result.Verification != nil && result.Verification.ExitCode != 0 {
		return contracts.ExitCode(contracts.RunCompleted, &contracts.RunError{Kind: contracts.ErrorVerification})
	}
	switch result.Status {
	case contracts.RunCompleted:
		return 0
	case contracts.RunTimedOut:
		return contracts.ExitCode(contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout})
	case contracts.RunCancelled:
		return contracts.ExitCode(contracts.RunCancelled, &contracts.RunError{Kind: contracts.ErrorCancellation})
	}
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
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorReadiness})
	case !hasSuccess && hasError:
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal})
	default:
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorTask})
	}
}
