package cli

import "io"

// commandHelp holds the detailed help text for a top-level command, printed
// by `pi-worker <command> --help`. The text is the offline contract for a
// command, so it is kept verbatim; a test guards that every flag in main.go
// appears here.
var commandHelp = map[string]string{
	"run": `pi-worker run - hand tasks to Pi workers and report what they did

Usage:
  pi-worker run [--task <prompt> | --task-file <path>]... [flags]

Each --task or --task-file is one worker; one to three per run, and the two
cannot be mixed. With neither, one task is read from stdin. --model,
--thinking, --writes and --data placed after a task belong to that task.
--model and --thinking placed before every task are the run default;
--writes and --data may precede the task only when there is one task.

Flags:
  --task <prompt>        the task text, inline
  --task-file <path>     the task text, read from a file
  --model <provider/id>  exact selector from ` + "`" + `pi-worker models` + "`" + `; never
                         substituted. Without it the configured default
                         applies (pi-worker config set default-model);
                         with neither, the run is refused (exit 2)
  --thinking <level>     off, minimal, low, medium, high, xhigh or max;
                         omitted: Pi's default for the model
  --writes <paths>       comma-separated workspace-relative paths the task
                         may write, checked after the run. Matches whole
                         path segments: internal/run covers
                         internal/run/x.go, not internal/runner.go. Every
                         task declares or none does; --writes "" declares
                         no writes; declarations must not overlap
  --data <paths>         comma-separated files appended to the task as
                         material to work on, not instructions. Advisory,
                         not containment: pass nothing the worker should
                         not act on
  --timeout <duration>   execution budget per task, default 30m; time
                         waiting in the queue (up to 15m) is not counted
  --verify <command>     one check run in the workspace after the workers
                         finish, with its own budget of the same size.
                         Split on spaces, no shell: | & ; < > $ ` + "`" + ` quotes
                         and backslashes are refused; put more in a script
  --worktree <name>      work in a new checkout at
                         <repo>/.pi-worker/worktrees/<name> on branch
                         run/<name>, made from HEAD. A separate directory,
                         not a sandbox
  --background           return at once with the run id; follow it with
                         pi-worker runs wait <id> --timeout <slice> --json
  --json                 print one JSON document on stdout; do not pipe it
  --debug                lifecycle lines on stderr; send them to a file
                         outside the workspace

Result (--json): read root outcome first. Report each worker's model,
thinkingLevel, status, explanation and error, and root changes, writes,
verification and leftoverProcesses. A failed run's changes still lists what
was written; nothing is rolled back.

Outcome and exit code:
  0  completed            the only success; still read the deliverable
  2  (refused)            fix the arguments and run again
  3  workers-unavailable  Pi or the model was not ready: a setup problem
  4  undeclared-writes    a file outside --writes changed
  5  task-failed, partial a worker failed; a provider refusal such as 403
                          in its error is an access problem, not the task
  6  verification-failed  --verify failed; read verification
  7  timeout              without a document, report the interruption
  8  cancelled            and stop
  9  internal-error

Workers run bash with your permissions in this workspace; this is not a
sandbox. Say in the task which git operations are allowed.

Full contract: https://github.com/arasovic/pi-worker/blob/main/docs/v0-usage.md
`,
	"runs": `pi-worker runs - list, follow and clean up runs

Usage:
  pi-worker runs list [--json]
  pi-worker runs status <id> [--json]
  pi-worker runs wait <id> [--timeout <duration>] [--json]
  pi-worker runs cancel <id> [--json]
  pi-worker runs prune --keep <n> [--yes] [--json]

list    every run on this machine, newest first, foreground and background,
        with its outcome: the run's own, running, interrupted (its process
        is gone) or unknown (its record cannot be read)
status  one background run's latest state, at once; waits for nothing
wait    reads a background run until it finishes, then prints it; it never
        cancels the run. --timeout bounds the wait, default 30m. When it
        runs out, the latest state is printed, the exit is 7 and the run
        keeps going: wait again or ask status later
cancel  asks a background run to stop and returns at once; follow it with
        wait to see it end as cancelled
prune   deletes foreground run records, keeping the newest <n>. A running
        run is never deleted, and neither is an unreadable record changed
        in the last hour. Background runs are never touched. It asks
        first; --yes skips the question and is required with --json or
        without a terminal

A finished background run exits with the code the same run would have in the
foreground: see pi-worker run --help. status of a run still going exits 0.
Otherwise: 0 done or declined; 2 bad arguments, an unknown run id, or prune
refused; 9 the records or the background store cannot be read, a delete
failed, or cancel found no live supervisor.
`,
	"worktrees": `pi-worker worktrees - list and remove the checkouts run --worktree made

Usage:
  pi-worker worktrees list [--json]
  pi-worker worktrees remove <name> [--yes] [--json]

A managed worktree is <repo>/.pi-worker/worktrees/<name> on branch
run/<name>; nothing else is managed.

list    name, path, branch, dirty (it has uncommitted changes) and merged
        (its branch is in your current HEAD); changes nothing
remove  deletes one clean, merged worktree, then its branch. There is no
        force: merge or discard the work first. It asks first; --yes skips
        only the question and is required with --json or without a
        terminal

Exit: 0 done or declined; 2 bad arguments, not found, dirty or not merged;
9 git failed, or the worktree changed after you confirmed: retry.
`,
	"models": `pi-worker models - list the model selectors Pi reports

Usage:
  pi-worker models [--timeout <duration>] [--json] [--debug]

Asks Pi once for its model catalog; no model is started and no prompt is
sent. Pass one printed provider/id selector to run --model, exactly.

A listed model is not proof of access. A model the account has lost still
appears here, and doctor does not catch it either. The refusal shows up only
when a run sends its prompt: the worker fails with the provider's error (for
example 403) and the run ends task-failed, exit 5. That is a setup problem,
not a task problem.

Flags:
  --timeout <duration>  budget for the query, default 30s
  --json                one document: models, each with provider, id and
                        selector
  --debug               lifecycle lines on stderr

Exit: 0 listed; 2 bad arguments; 3 Pi is missing or unavailable, or the
catalog is empty; 7 timeout; 8 cancelled; 9 Pi answered with malformed data.
`,
	"doctor": `pi-worker doctor - check that this machine is ready to run workers

Usage:
  pi-worker doctor [--timeout <duration>] [--json] [--debug]

Inspects only: it never repairs, logs in, switches models or sends a prompt.
Checks, in order: pi-executable, pi-version, config, model-catalog,
default-model, workspace. A warning leaves the machine ready; a failed check
makes it not ready. The workspace check only warns: outside a git work tree
a run still works, but cannot report what it changed. doctor does not prove
access to a model; see pi-worker models --help.

Flags:
  --timeout <duration>  budget for the inspection, default 30s
  --json                one result document; nothing on stdout when the
                        inspection times out, is cancelled or aborts
  --debug               lifecycle lines on stderr

Exit: 0 ready, warnings allowed; 2 bad arguments; 3 not ready; 7 timeout;
8 cancelled; 9 internal failure.
`,
	"config": `pi-worker config - show or set the personal defaults

Usage:
  pi-worker config show [--json]
  pi-worker config set default-model <provider/model> [--debug]
      [--timeout <duration>]
  pi-worker config set max-model-workers <n>

show               reads the local file only and never starts Pi; a
                   missing file is the empty default
default-model      the model a run uses when neither the task nor the run
                   names one. Pi's catalog is asked once, and the selector
                   is saved only when it is listed exactly; nothing is
                   substituted. --timeout bounds that query, default 30s
max-model-workers  how many workers may run at once across this machine,
                   default 3; saved locally, Pi is not contacted. There is
                   no per-run override

Exit: 0 done; 2 bad arguments; 3 Pi is missing or unavailable, or the
selector is not in the catalog; 7 timeout; 8 cancelled; 9 the configuration
cannot be read or written, or config.json is a symbolic link.
`,
	"skill": `pi-worker skill - report on the installed agent skill

Usage:
  pi-worker skill status [--json]
  pi-worker skill receipt-path [--json]

Read-only: it never installs, repairs or removes a skill.

status        checks the installer's receipt against the files on disk and
              names a recovery command when they disagree. stale means the
              files are intact but an older release installed them;
              reinstall to update
receipt-path  prints where that receipt is stored

Exit: 0 verified; 2 bad arguments; 3 missing, drifted, stale or incomplete;
9 the receipt cannot be read.
`,
	"version": `pi-worker version - print which build this is

Usage:
  pi-worker version [--json]
  pi-worker --version [--json]

--json prints one document with version, commit and buildDate. A source
build reports dev, unknown and unknown.
`,
}

// printCommandHelp writes the detailed help for command to w. An unknown
// command writes nothing; the caller decides how to report it.
func printCommandHelp(w io.Writer, command string) {
	text, ok := commandHelp[command]
	if !ok {
		return
	}
	io.WriteString(w, text)
}

// wantsHelp reports whether the first argument is -h or --help. Only the
// first argument counts, so a prompt such as `--task --help` is never taken
// for a help request.
func wantsHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help")
}

// subcommandArgs returns the arguments after a command's subcommand, so a
// help request placed after it (config set --help) is found. It is nil when
// there is no subcommand, so the caller's wantsHelp on the command arguments
// still sees a leading help flag.
func subcommandArgs(args []string) []string {
	if len(args) > 2 {
		return args[2:]
	}
	return nil
}
