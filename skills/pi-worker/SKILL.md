---
name: pi-worker
description: Use when delegating bounded coding work to Pi workers, such as on cheaper or separately metered models.
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
- Use trusted workspaces. pi-worker does not restrict git: state in each
  task file which git operations are allowed.
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

## Plan

Give each worker a task file outside the workspace: the goal, the files,
and how to check the result. A run takes one to three tasks. Across
all runs the machine runs `max-model-workers` (default 3) at once; extra
workers queue and can time out unstarted.

## Run

```sh
pi-worker run --model <provider/model> --thinking <level> \
  --task-file <task.md> --writes <paths> \
  --timeout <duration> --json --debug [--verify <command>] \
  2>/tmp/pi-worker-<run>.log
```

Do not pipe stdout: when no document comes back, the exit code is the signal.

If the host may cut the call off, add `--background`, then repeat
`pi-worker runs wait <id> --timeout <slice> --json --debug` until the run
ends; there the run's result is under `result`. A wait that runs out
leaves the run going, as does a cut-off `run`; `runs list` finds it. Exit 9
with "supervisor is no longer there": the run was interrupted; stop waiting.

## Result

Read root `outcome`. `completed` (exit 0) is the only success;
`pi-worker run --help` gives each outcome its exit code and next move.
Whatever the outcome, read and report each worker's `model`, `thinkingLevel`,
`status`, `explanation` (else `partialExplanation`), `error`, `warning`,
`transcript`, plus `changes`, `writes`, `verification`, `git`,
`leftoverProcesses`, and `worktree`. A failed run's `changes` still lists
its writes; nothing is rolled back.

`completed` does not prove the deliverable: read it yourself or check it
with `--verify`. Merge or remove a `--worktree` checkout afterwards
(`pi-worker worktrees`).
