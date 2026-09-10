package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// backgroundPiExecutable is the Pi program a background worker drives. It is
// the same program a foreground worker drives, named here because a background
// worker runs in a process that resolves nothing for itself. It is a seam only
// so a test can point one run at a stand-in Pi.
var backgroundPiExecutable = "pi"

// backgroundRoleExecutable names the program a supervisor is spawned from.
// Empty — always, in production — means this program. A test binary, which
// dispatches no role token, names the built one here.
var backgroundRoleExecutable = ""

// newBackgroundManager is the seam the background run command constructs its
// Manager through. Tests replace it to point one run's state at a directory of
// their own; production always resolves the default location.
var newBackgroundManager = func(admissionRoot string, maxModelWorkers int) (*background.Manager, error) {
	return background.NewManager("", admissionRoot, maxModelWorkers)
}

// backgroundRunCommand starts one run in the background and returns as soon as
// it is accepted, while the run keeps going in a process of its own. Every
// option a foreground run takes keeps its meaning here: --background changes
// who waits, not what runs.
//
// The accepted run's identity is what the caller needs afterwards — it is the
// argument every later question about the run takes — so it is printed on
// stdout, and with --json the accepted snapshot is printed there instead,
// exactly as it is stored.
func backgroundRunCommand(ctx context.Context, opts runOptions, tasks []run.Task, stdout, stderr io.Writer) int {
	workspace, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine workspace: %v\n", err)
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
	}

	manager, err := newBackgroundManager(opts.admissionRoot, opts.maxModelWorkers)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
	}

	started, err := manager.Start(ctx, background.StartOptions{
		Tasks:            tasks,
		Workspace:        workspace,
		Verify:           opts.verify,
		ExecutionTimeout: opts.timeout,
		PiExecutable:     backgroundPiExecutable,
		WorktreeName:     opts.worktree,
		RoleExecutable:   backgroundRoleExecutable,
		Debug:            opts.debug,
	})
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return backgroundStartExitCode(err)
	}

	if worktreeOf := started.Snapshot.Worktree; worktreeOf != nil {
		fmt.Fprintf(stderr, "pi-worker: worktree %s on branch %s\n", worktreeOf.Path, worktreeOf.Branch)
	}

	if opts.json {
		data, err := json.Marshal(started.Snapshot)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode accepted run: %v\n", err)
			return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}

	fmt.Fprintf(stdout, "run %s accepted with %d worker(s); it continues in the background\n",
		started.RunID, len(started.Snapshot.Workers))
	return 0
}

// backgroundStartExitCode maps a start failure onto the code the same failure
// would produce in the foreground: a refusal the caller can correct is a usage
// error, an expired or cancelled wait keeps its own code, and anything else is
// internal.
func backgroundStartExitCode(err error) int {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return contracts.ExitCode(contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout})
	case errors.Is(err, context.Canceled):
		return contracts.ExitCode(contracts.RunCancelled, &contracts.RunError{Kind: contracts.ErrorCancellation})
	case worktree.IsRefusal(err):
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorUsage, Message: err.Error()})
	default:
		return contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal, Message: err.Error()})
	}
}
