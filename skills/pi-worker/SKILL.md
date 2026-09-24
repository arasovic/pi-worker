---
name: pi-worker
description: Use when an agent delegates work through Pi, needs cheaper or separately metered models, or assigns one to three Pi workers.
---

# Pi Worker

Delegate bounded execution only. Keep product, architecture, scope, and
integration decisions in the parent agent. Never ask a worker to delegate.

Every command explains itself: `pi-worker <command> --help` carries its flag
rules, result fields, and exit codes. Read `pi-worker run --help` before the
first run.

## Boundaries

- Workers modify the current writable workspace and may run `bash` with the
  current user's host permissions. This is not a sandbox. `--worktree <name>`
  gives a run a separate working directory, not containment.
- Use trusted workspaces. pi-worker does not restrict commit, stash, checkout,
  or reset: state in each task file which git operations are allowed.
- Parallel writes must be disjoint. Runs sharing a workspace are not locked
  against each other: serialize them or give each its own worktree.
- Parent-started side jobs must self-terminate.
- Do not repeat prompts, credentials, or raw debug output in reports.

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
leaves the run going. In that document the result sits under `result`.

## Result

Read root `outcome`. `completed` (exit 0) is the only success; the exit code
mirrors the outcome, and `pi-worker run --help` gives each one its next move.
Whatever the outcome, read and report each worker's `model`, `thinkingLevel`,
`status`, `explanation`, and `error`, plus `changes`, `writes`,
`verification`, and `leftoverProcesses`. A failed run's `changes` still lists
what the workers wrote; nothing is rolled back.

`completed` does not prove the deliverable: read it yourself, or pass a
`--verify` command that inspects it.
