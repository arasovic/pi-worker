# Pi Worker v0 Usage

This documents the current public v0 behavior.

## Agent skill

The canonical provider-neutral agent skill is
[`skills/pi-worker`](../skills/pi-worker). Use it when delegating through Pi,
selecting cheaper or separately metered models, or assigning one to three Pi
workers. It resolves informal model names from the catalog, uses an exact
explicit selector without fallback, and otherwise lets the configured default
apply. It uses external task files, keeps parallel work disjoint, and reports
the parsed final JSON result without debug or raw protocol output.

## Prerequisites

- Go version from `go.mod`: language level `go 1.25.0` (toolchain declared as `go1.26.1`).
- Installed Pi CLI compatible with the **Pi 0.84.1** verified surface.
- Provider authentication is configured in Pi itself. Do not pass credentials/secrets via `pi-worker` argv.
  - Open Pi interactively and use its `/login` flow for authentication.
- Build from source only:

```sh
go build -o ./bin/pi-worker ./cmd/pi-worker
```

> Source builds report version `dev`.
> Release/version injection and packaging remain deferred.
> Packaging/installers are not part of v0.

## Supported commands

- `pi-worker version`
- `pi-worker models ...`
- `pi-worker doctor [--timeout <duration>] [--json] [--debug]`
- `pi-worker config show [--json]`
- `pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]`
- `pi-worker run ...`

## Model catalog

```text
pi-worker models [--timeout <duration>] [--json] [--debug]
```

- `models` queries Pi once with `get_available_models`; it never activates a
  model or submits a prompt.
- The catalog process enables only `read,grep,find,ls`; it cannot write files
  or run shell commands through Pi.
- Human output is one sorted exact `provider/id` selector per line.
- `--json` emits one object with `schemaVersion: 1` and sorted `models`, each
  containing `provider`, `id`, and `selector`.
- The default timeout is `30s`. An empty catalog is a readiness failure (exit
  code `3`), as is a missing or unavailable Pi executable. Malformed Pi data is
  a protocol/internal failure (exit code `9`).

## Local readiness doctor

```text
pi-worker doctor [--timeout <duration>] [--json] [--debug]
```

- `doctor` is inspection-only: it never repairs configuration, logs in,
  switches providers or models, reads Pi profile/auth files, invokes a model,
  or submits a prompt.
- It performs these checks in this exact order: `pi-executable`, `pi-version`,
  `config`, `model-catalog`, `default-model`, and `global-skill`.
- The Pi version check accepts exactly `0.84.1`. A missing configuration or
  global skill is a warning; warnings leave the environment ready. A failed
  check makes it not ready.
- `model-catalog` sends only `get_available_models`; it does not activate a
  model or send a prompt. An empty catalog is failed.
- The default timeout is `30s`. Human output has one line per ordered check and
  an overall readiness line. `--json` writes exactly one `doctor.Result`
  document to stdout; `--debug` and diagnostics write only to stderr.
- Exit codes are `0` for ready or warning-only results, `3` for readiness
  failures, `7` for timeout, `8` for cancellation, and `9` for protocol or
  internal failures. Invalid flags return `2` before an inspection starts.

## Exact run command

```text
pi-worker run [--model <provider/model>] [--task <prompt> | --task-file <path>]... [--timeout <duration>] [--json] [--debug]
```

## Personal default model

`pi-worker` stores a two-field, versioned JSON configuration document in the
operating system's user configuration directory. It contains only
`schemaVersion` and `defaultModel`; the empty default is provider-neutral.

```text
pi-worker config show [--json]
pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]
```

- `config show` reads the local document only. It never launches Pi.
- `config set` requires an exact `provider/model` selector, queries Pi's model
  catalog once, and writes the default only when that exact selector is
  available. A missing or unavailable Pi executable is a readiness failure
  (exit code `3`); protocol/internal failures use exit code `9`. There is no
  fallback.
- Configuration writes use a same-directory temporary file, file sync, atomic
  replacement, and owner-only permissions where supported.
- Model precedence is explicit `run --model`, then the configured
  `defaultModel`, then a usage error (exit `2`). When no model resolves, stdin
  is not read.

## Behavior

### Model selection

- `--model` must be an exact `provider/model` string.
- An explicit `--model` always wins over the configured default and does not
  read or rewrite the configuration document.
- `run` resolves the model by:
  1. `get_available_models`
  2. exact catalog match
  3. `set_model`
  4. confirmation check that `set_model` returns the exact same `provider` and `id`
- If no exact model match exists, or confirmation differs/missing, execution stops with an error. There is **no** pattern matching, fallback, or switching.

### Exactly one input mechanism

- `--task` and `--task-file` may each repeat up to 3 total tasks.
- Only one input source is allowed:
  - `--task` repeated 1..3 times, or
  - `--task-file` repeated 1..3 times, or
  - no task flags: one task read from stdin.
- Mixing `--task` and `--task-file` is rejected.
- Default timeout is `30m` and applies to the whole foreground run (all workers share one deadline).

### Workspace and worker sharing

- The current working directory is the writable workspace.
- Each worker gets a fresh temporary session directory created with `os.MkdirTemp("", "pi-worker-v0-*")`.
- For more than one worker, all workers share that workspace and a warning is printed:

```text
pi-worker: warning: N workers share the writable current workspace; tasks must use disjoint files
```

- Assign disjoint files, or use only one worker. v0 makes no worktree/isolation claim.
- Every v0 worker always enables `read,grep,find,ls,edit,write,bash` with `--no-approve`; `bash` can execute arbitrary shell commands with the current user's host permissions, and this is not a sandbox.

### Output

- Human output labels every worker result with `worker N:`.
  - Completed worker output goes to stdout.
  - Failed/errored worker output goes to stderr.
- `--json` emits **exactly one** JSON object (single document) only after argument/input validation succeeds and a run starts, with:
  - `schemaVersion` = `1`
  - `status`
  - `workers` in input order (the same order as task inputs, not completion order)
- Pre-run usage/input validation errors are written to stderr and may produce no JSON output.

Example:

```json
{"schemaVersion":1,"status":"completed","workers":[{"model":"provider/model-id","status":"completed","explanation":"Worker one done"},{"model":"provider/model-id","status":"completed","explanation":"Worker two done"}]}
```

### Exit codes

- `0` completed
- `2` usage
- `3` all workers unavailable / readiness path
- `5` task failure or partial completion
- `7` timeout
- `8` cancellation
- `9` protocol/internal
- `4` (policy) and `6` (verification) are reserved and **not emitted by this v0 slice**.

### `--debug` debug stream

`--debug` writes sanitized lifecycle progress only to stderr. It includes:
- worker identity and start line
- RPC request status/duration (`get_available_models`, `set_model`, `prompt`, `get_last_assistant_text`)
- model streaming heartbeat (`phase=model-streaming`, first line and at most one heartbeat every 30s)
- tool start/end status and duration
- settlement line
- worker completion and total duration

It does **not** print:
- prompts
- assistant text
- tool args/results
- raw frames
- environment values or credentials
- child stderr

### Ctrl-C / timeout cleanup and lifecycle boundary

- Ctrl-C and timeout cancel the shared run context.
- macOS/Linux: each child runs in its own process group; cleanup kills that group and performs a best-effort, creation-time-verified descendant sweep.
- Windows: children are placed in a Job Object with kill-on-close.
- This is recovery, not a sandbox. Deliberately daemonized/reparented processes, processes spawned during the post-snapshot window, and the short Windows pre-assignment window can escape.
- If Pi exits and is reaped before cleanup can snapshot its lineage, surviving descendants may also escape; v0 does not continuously track descendants.

## Examples

### One direct task (human)

```sh
pi-worker run --model provider/model-id --task "Implement the requested fix"
```

### Two/three tasks via files (parallel workers)

```sh
pi-worker run --model provider/model-id --task-file ./task-a.txt --task-file ./task-b.txt
```

```sh
pi-worker run --model provider/model-id --task-file ./task-a.txt --task-file ./task-b.txt --task-file ./task-c.txt
```

### Stdin fallback

```sh
cat prompt.txt | pi-worker run --model provider/model-id
```

## v1 deferrals (not in v0)

- trust store and content provenance
- Docker/OpenShell
- worktree/patch application
- durable registry / background / status / wait / steer / cancel / resume
- public installer, package manager, and skill installation

## Compatibility note

v0 is pinned/probed against Pi **0.84.1**. Re-probe before any unpinned Pi upgrade:

- [docs/pi-cli-surface.md](./pi-cli-surface.md)
