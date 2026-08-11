package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"pi-worker/internal/buildinfo"
	"pi-worker/internal/contracts"
	"pi-worker/internal/pi"
	"pi-worker/internal/run"
)

// defaultRunTimeout bounds one foreground worker run.
const defaultRunTimeout = 30 * time.Minute

// newWorker is a private dependency-injection seam. Tests replace it with a
// scripted fake so CLI tests never launch the user's real Pi profile.
var newWorker = func() pi.Worker { return pi.New("pi") }

// newCatalog is the private dependency-injection seam for the read-only
// model catalog command.
var newCatalog = func() pi.ModelCatalog { return pi.NewCatalog("pi") }

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
		fmt.Fprintf(stdout, "pi-worker %s\n", buildinfo.Version)
		return 0
	case "models":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return modelsCommand(ctx, args[1:], stdout, stderr)
	case "config":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return configCommand(ctx, args[1:], stdout, stderr)
	case "run":
		opts, tasks, err := resolveRunInput(args[1:], stdin)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: %v\n", err)
			printUsage(stderr)
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return runCommand(ctx, opts, tasks, stdout, stderr)
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
		fmt.Fprintf(stdout, "pi-worker %s\n", buildinfo.Version)
		return 0
	case "models":
		return modelsCommand(ctx, args[1:], stdout, stderr)
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
	if opts.model == "" {
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
	fmt.Fprintln(w, "usage: pi-worker version")
	fmt.Fprintln(w, "       pi-worker models [--timeout <duration>] [--json] [--debug]")
	fmt.Fprintln(w, "       pi-worker config show [--json]")
	fmt.Fprintln(w, "       pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]")
	fmt.Fprintln(w, "       pi-worker run [--model <provider/model>] [--task <prompt> | --task-file <path>]... [--timeout <duration>] [--json] [--debug]")
}

// runOptions holds the parsed run command surface.
type runOptions struct {
	model     string
	tasks     []string
	taskFiles []string
	timeout   time.Duration
	json      bool
	debug     bool
}

// runCommand runs one to three parallel workers with an already-resolved
// task list. The run timeout is a single shared deadline on the caller's
// context covering every worker: Ctrl-C (or any parent cancellation)
// cancels the run immediately, and the timeout bounds it when no signal
// arrives.
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

	if len(tasks) > 1 {
		fmt.Fprintf(stderr, "pi-worker: warning: %d workers share the writable current workspace; tasks must use disjoint files\n", len(tasks))
	}

	result, err := run.New(newWorker()).Run(ctx, run.Request{
		Model:     opts.model,
		Tasks:     tasks,
		Workspace: workspace,
		Debug:     debug,
	})
	if err != nil {
		// Defensive: the CLI validates the input surface first, so a
		// controller validation error here is an internal failure.
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
	}

	code := runExitCode(result)

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
		if worker.Status == pi.StatusCompleted {
			fmt.Fprintf(stdout, "worker %d: %s\n", i+1, worker.Explanation)
			continue
		}
		message := worker.Error
		if message == "" {
			message = worker.Explanation
		}
		if message == "" {
			message = fmt.Sprintf("status %q", worker.Status)
		}
		fmt.Fprintf(stderr, "pi-worker: worker %d: %s\n", i+1, message)
	}
	return code
}

func parseRunArgs(args []string) (runOptions, error) {
	opts := runOptions{timeout: defaultRunTimeout}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--model", "--timeout":
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
	if opts.model != "" {
		if err := validateModel(opts.model); err != nil {
			return opts, err
		}
	}
	if len(opts.tasks) > 0 && len(opts.taskFiles) > 0 {
		return opts, fmt.Errorf("specify exactly one input source: --task or --task-file, not both")
	}
	if count := len(opts.tasks) + len(opts.taskFiles); count > 3 {
		return opts, fmt.Errorf("at most 3 tasks per run, got %d", count)
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
// any worker reported an internal error; partial runs stay 5.
func runExitCode(result run.Result) int {
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
