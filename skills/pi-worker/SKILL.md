---
name: pi-worker
description: Use when an agent delegates work through Pi, needs cheaper or separately metered models, or assigns one to three Pi workers.
---

# Pi Worker

Delegate bounded execution only. Keep product, architecture, scope, and
integration decisions in the parent agent. Never ask a worker to delegate.

## Run

1. Confirm `pi-worker` is on `PATH`.
2. For an informal model name, query `pi-worker models --json --debug --timeout 30s`. Select one unambiguous exact `provider/model`; report ambiguity and stop.
3. Preserve every explicit model. If unavailable or unauthenticated, report the setup action and stop. Never substitute a model or provider. If omitted, let the configured default apply. Model and thinking bind positionally like `--writes`: after a `--task` or `--task-file` they are that task's own, before every task they are the run default every task without its own inherits. Two models no longer need two runs — give each task its own `--model` and `--thinking` in one run. `--writes` keeps a stricter placement: in a multi-task run, one before every task is rejected as ambiguous, while `--model` in the same position is the run default. Two concurrent runs that both declare writes still cannot overlap in one workspace: a run that wrote nothing can be the one reported as undeclared.
4. Treat thinking as a separate axis from the model: `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`. An informal name ending in a level — "Luna Max", "Sonnet high" — is one model plus one level, never a model named `luna-max`. Resolve the model through step 2 and pass both flags: `--model <exact selector from the catalog> --thinking max`. Never guess a provider prefix. Omit thinking when unspecified.
5. Write one private task file per worker. Use one to three workers, and parallelize only disjoint responsibilities and writes. Declaring the paths with `--writes` asks for the write check; whether it actually ran is something the result reports. `--writes` paths are workspace-relative: an absolute path fails the run before any worker starts. The declaration is all-or-none: a run where some tasks declare and others do not is rejected before any worker starts. A task that will write nothing declares `--writes ""`. An overlapping declaration fails the run before any worker starts. In a one-task run, `--writes` may appear anywhere in the argument list, before the `--task` or `--task-file` included, and a prompt on stdin declares it the same way. With more than one task, place each `--writes` directly after the `--task` or `--task-file` it declares; one that precedes all of them is rejected as ambiguous.
   To hand a worker content to work ON — an issue body, a log, a spec — pass its path with `--data <paths>` (comma-separated, positional per task like `--writes`): pi-worker frames each file as a delimited MATERIAL section below the task's text and declares in the prompt that the material is content to work on, not instructions to follow — advisory: honouring it is the model's behavior, not a property pi-worker enforces. Pass nothing you would not have the worker act on as instructions: `--data` is not a containment mechanism for untrusted text. The worker result reports each file's `path`, `byteCount`, and `sha256`, never its content.
6. Run with a bounded timeout, JSON result, and debug lifecycle output:

```sh
pi-worker run --model <provider/model> --thinking <level> \
  --task-file <task-a.txt> --writes <paths-a> \
  --task-file <task-b.txt> --writes <paths-b> \
  --timeout <duration> --json --debug [--verify <command>] \
  2>/tmp/pi-worker-debug.log
```

stdout carries only the JSON document; `--debug` stderr goes to a file outside
the workspace (a file inside would read as an undeclared change). Do not pipe
the command to another tool: the exit code is the signal when no document
comes back, and a pipe hands it to the downstream tool instead.

Add `--verify <command>` when the finished workspace must be proven green
(e.g. `go test ./...`). The check runs once after the workers settle and
is split on whitespace into argv: no shell is involved, so shell syntax
is rejected up front, not executed. The result's `git`, `changes`, and
`writes` describe the workers only: they are captured before the check
runs, so keep the check read-only or inspect its artifacts separately
when you need a clean evidence report.

Parse the single JSON document. A run can end without producing a document;
then the exit code is the signal. An exit of 2 always means the command was
rejected — fix your argv and re-run; an exit of 9 is an internal failure; an
exit of 7 or 8 means it was cut short: without a document, report interruption
and stop; with a document, read and report each worker's `model`, effective
`thinkingLevel`, `status`, `explanation`, `partialExplanation` when present, and
`error`, plus root `changes`, `writes` when present, and `verification` when
present. The rejection message is on stderr, not stdout — the documented
invocation sends its debug output there too — so read stderr when no document
appears.
When `thinkingFallback` is true, surface its warning: the selected model
continued with Pi's confirmed default effort. Read root `outcome`:
`completed` is the only done state — a `writes.skipped` value means a check
could not run, unproven, not clean. When `writes.skipped` is `change manifest
unavailable`, the manifest was not measured: read `changes.omitted`, which is
always present on a real run — the CLI always configures the git inspector, so
`changes` never vanishes from output. A listed file carrying
`noFinalNewline: true` ends without a final newline; that is descriptive, not
a verdict — `added` means the run produced it that way, `modified` means it
may always have been so. The reason decides the caller's next move: `unborn
head`, `context already done`, `measurement failed`, or `work
tree not confirmed` — the last meaning the workspace is not a git work tree,
git is missing, or the guard failed transiently, which the reason does not
claim to distinguish. Which reasons a retry can clear differs: `measurement
failed` — a git command failure or a budget that expired — and the transient
guard failure behind `work tree not confirmed` can clear on retry; the reason
cannot tell that cause from a genuinely unconfirmed work tree, so one retry is
a fair test and repeating it is not. `context already done` means the run's
own context was already dead when it would have inspected, so re-run with a
live context. `unborn head` means the repository has no commits, which no
retry can change. `verification-failed` means the `verification` object is
there; report it, fix the workspace, and re-run. Any other word means report
it with its object when one exists (`writes`, `verification`, or the worker's
`error`) and stop.

## Boundaries

- Workers modify the current writable workspace and may run `bash` with the current user's host permissions. This is not a sandbox. The run flag `--worktree <name>` opts one run into a checkout of its own: a separate working directory, not containment — a worker can still reach outside it. Without the flag, behavior is unchanged and the worker works in the current directory. A task can lead a worker to commit, stash, checkout, or reset; pi-worker does not restrict this, so the task file must state what git operations are allowed.
- When a run moves HEAD, the branch, or the stash list, the result carries a `git` object with the before and after state. Its presence means something moved that a bounded edit does not normally move: read it as a notification, not a prohibition — a caller may legitimately want a worker to commit.
- Use trusted workspaces. Parallel writes must be disjoint, and whenever `--writes` is used, one run at a time per workspace: if one run writes a stray path, a concurrent run that declared it writes nothing is the one reported as undeclared — a run that wrote nothing gets accused.
- Cleanup is best-effort lifecycle recovery, not a sandbox or a no-escape guarantee. Deliberately daemonized or reparented Unix descendants, processes spawned during teardown, and the Windows pre-assignment window can escape.
- Parent-started side jobs must self-terminate.
- Keep a `trap 'kill 0' EXIT INT TERM` as a secondary layer only. It does not run on SIGKILL, which a harness timeout can deliver, so it cannot substitute for the bounded command.
- Debug is bounded stderr lifecycle data, not the result. A heartbeat proves only that the managed Pi process is alive; it does not prove model progress.
- Do not repeat raw debug frames, prompts, credentials, or assistant output unnecessarily.
