---
name: pi-worker
description: Use when an agent delegates work through Pi, needs cheaper or separately metered models, or assigns one to three Pi workers.
---

# Pi Worker

Delegate bounded execution only. Keep product, architecture, scope, and
integration decisions in the parent. Never ask a worker to delegate.

`pi-worker <command> --help` carries each command's flags, result fields, and
exit codes; read `pi-worker run --help` before the first run.

## Boundaries

- Workers modify the current writable workspace and may run `bash` with the
  current user's host permissions. This is not a sandbox. `--worktree <name>`
  gives a run a separate working directory, not containment.
- Use trusted workspaces. pi-worker does not restrict commit, stash, checkout,
  or reset: state in each task file which git operations are allowed.
- Parallel writes must be disjoint. Runs sharing a workspace are not locked
  against each other: serialize them or give each its own worktree.
- Parent-started side jobs must self-terminate.
- Do not repeat prompts, credentials, raw debug output, or transcript contents
  in reports; a `transcript` is the whole Pi session, secrets included.

## Model

Use the exact model asked for; never substitute a model or provider. Resolve an
informal name with `pi-worker models --json`; on ambiguity or an unavailable
model, report it and stop. Thinking is a separate level: "Acme Max" is one
model plus `--thinking max`.

## Run

Write one private task file per worker. Use one to three workers, each with its
own disjoint `--writes`.

```sh
pi-worker run --model <provider/model> --thinking <level> \
  --task-file <task-a.txt> --writes <paths-a> \
  --task-file <task-b.txt> --writes <paths-b> \
  --timeout <duration> --json --debug [--verify <command>] \
  2>/tmp/pi-worker-debug.log
```

Do not pipe stdout: when no document comes back, the exit code is the signal.
When the host may cut the call off, add `--background` and wait in slices with
`pi-worker runs wait <id> --timeout <slice> --json`; a wait that runs out
leaves the run going, as does a cut-off `run`, until `--timeout`; `runs list`
or its `run <id>` line finds it. Give each wait `--debug` to replay debug
lines. Exit 9 with "supervisor is no longer there" means the run was
interrupted; waiting again will not help. In that document the result sits
under `result`.

## Result

Read root `outcome`. `completed` (exit 0) is the only success;
`pi-worker run --help` gives each outcome its exit code and next move.
Whatever the outcome, read and report each worker's `model`, `thinkingLevel`,
`status`, `explanation` (else `partialExplanation`), `error`,
`warning`, `transcript`, plus `changes`, `writes`, `verification`, `git`,
`leftoverProcesses`, and `worktree`. A failed run's `changes` still lists
what workers wrote; nothing is rolled back.

`completed` does not prove the deliverable: read it yourself, or pass a
`--verify` command that inspects it.
