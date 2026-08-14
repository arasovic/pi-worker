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
3. Preserve every explicit model. If unavailable or unauthenticated, report the setup action and stop. Never substitute a model or provider. If omitted, let the configured default apply.
4. Treat thinking as a separate axis from the model: `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`. An informal name ending in a level — "Luna Max", "Sonnet high" — is one model plus one level, never a model named `luna-max`. Resolve the model through step 2 and pass both flags: `--model <exact selector from the catalog> --thinking max`. Never guess a provider prefix. Omit thinking when unspecified.
5. Write one private task file per worker. Use one to three workers; parallelize only disjoint responsibilities and writes. Declaring the paths with `--writes` turns that rule into a checked one — a task that will write nothing declares `--writes ""`, which is what lets a read-only delegation be checked — and an overlapping declaration fails the run before any worker starts, and the run is also checked afterwards against the paths it changed, exiting `4` only when every task declared and the manifest was measured — with either missing, the check skips and the write exits `0`.
6. Run with a bounded timeout, JSON result, and debug lifecycle output:

```sh
pi-worker run --model <provider/model> --thinking <level> \
  --task-file <task-a.txt> --writes <paths-a> \
  --task-file <task-b.txt> --writes <paths-b> \
  --timeout <duration> --json --debug [--verify <command>]
```

Add `--verify <command>` when the finished workspace must be proven green
(e.g. `go test ./...`). The check runs once after the workers settle and
is split on whitespace into argv: no shell is involved, so shell syntax
is rejected up front, not executed.

Parse the single JSON document. Report each worker's model, effective
`thinkingLevel`, status, explanation, and failure. When
`thinkingFallback` is true, surface its warning: the selected model continued
with Pi's confirmed default effort. When the result carries `verification`,
a non-zero `exitCode` means the workspace check failed: report the excerpt
(`output`) and the full log (`logFile`) and do not treat the run as done;
fix the workspace and re-run. The run `status` stays `completed`; only the
process exit code and the verification object carry the failure.

## Boundaries

- Workers modify the current writable workspace and may run `bash` with the current user's host permissions. This is not a sandbox or worktree layer. A task can lead a worker to commit, stash, checkout, or reset; pi-worker does not restrict this, so the task file must state what git operations are allowed.
- When a run moves HEAD, the branch, or the stash list, the result carries a `git` object with the before and after state. Its presence means something moved that a bounded edit does not normally move: read it as a notification, not a prohibition — a caller may legitimately want a worker to commit.
- Use trusted workspaces. Parallel writes must be disjoint.
- Each run's Pi process and its descendants are terminated when the run ends, times out, or is cancelled.
- That guarantee covers pi-worker's own children only. A delegation runs for minutes, so a background job the parent agent starts beside one is exposed to a harness timeout that can SIGKILL the shell. Make that job self-terminating: it must end on its own without any cleanup step running. Where `timeout` exists, use `timeout 60 <command>`; where it does not, use a loop with a fixed iteration count that cannot run forever.
- Keep a `trap 'kill 0' EXIT INT TERM` as a secondary layer only. It does not run on SIGKILL, which a harness timeout can deliver, so it cannot substitute for the bounded command.
- Debug is bounded stderr lifecycle data, not the result. A heartbeat proves only that the managed Pi process is alive; it does not prove model progress.
- Do not repeat raw debug frames, prompts, credentials, or assistant output unnecessarily.
