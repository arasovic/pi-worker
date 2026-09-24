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
