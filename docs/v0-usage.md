# Pi Worker v0 Usage

This documents the current public v0 behavior.

## Agent skill

The canonical provider-neutral agent skill is
[`skills/pi-worker`](../skills/pi-worker). Use it when delegating through Pi,
selecting cheaper or separately metered models, or assigning one to three Pi
workers. It resolves informal model names from the catalog, uses an exact
explicit selector without fallback, and otherwise lets the configured default
apply. It keeps model and explicit reasoning effort separate, reports any
thinking fallback to Pi's confirmed default, uses external task files, keeps
parallel work disjoint, and returns the parsed final JSON result without debug
or raw protocol output.

## Prerequisites

- Node.js 22.20.0 or newer and npm.
- Installed Pi CLI. Version **0.84.1** is the verified compatibility surface;
  other semantic versions remain usable with an explicit warning.
- Provider authentication is configured in Pi itself. Do not pass credentials/secrets via `pi-worker` argv.
  - Open Pi interactively and use Pi's own authentication flow.

The npm package supports macOS and Linux on arm64 and x64. Windows requires a
source build and is compile-checked, but it is not runtime-tested in the current
release gates.

Install the public package normally, or keep installer diagnostics visible:

```sh
npm install -g pi-worker
npm install -g --foreground-scripts pi-worker
```

The binary alone can be installed from the public Go module:

```sh
go install github.com/arasovic/pi-worker/cmd/pi-worker@v0.1.1
```

The binary lands in `go env GOBIN`, or in `$(go env GOPATH)/bin` when GOBIN
is unset. That directory must be on PATH for the `pi-worker` command to
resolve. Confirm the location with:

```sh
gobin=$(go env GOBIN)
ls "${gobin:-$(go env GOPATH)/bin}/pi-worker"
command -v pi-worker
```

## Source build

For the current checkout, the repository declares Go 1.25 language compatibility
and a Go 1.26.1 toolchain:

```sh
go build -o ./bin/pi-worker ./cmd/pi-worker
```

After a source build, use `./bin/pi-worker` in place of `pi-worker` for the
commands in this document.

> Source-build metadata defaults to `version=dev`, `commit=unknown`, and
> `build date=unknown`; the human version output is `pi-worker dev`. Release
> artifacts inject all three values and print the full 40-hex commit.

## npm postinstall

During `npm install`, npm attempts to install the bundled provider-neutral
`pi-worker` skill for detected agent targets via pinned `skills@1.5.22`. It
records an `installed`, `blocked`, `skipped`, or `failed` outcome in the durable
receipt. Existing conflicts may block, skip, or fail without overwriting them.
On an unsupported npm platform or architecture, setup can skip before the
native CLI creates a receipt; `pi-worker skill status` is unavailable when the
launcher itself is unsupported.

Interactive terminals show a compact status block with the package version,
skill outcome, installed target count, and next command. The outcome word is
the only colored element. CI, `NO_COLOR`, and non-interactive installs retain
one plain diagnostic line.

## Supported commands

- `pi-worker version [--json]`
- `pi-worker models ...`
- `pi-worker doctor [--timeout <duration>] [--json] [--debug]`
- `pi-worker config show [--json]`
- `pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]`
- `pi-worker skill status [--json]`
- `pi-worker skill receipt-path [--json]`
- `pi-worker run ...`

## Version

```text
pi-worker version [--json]
```

- Human output identifies the build as `pi-worker <version>` and includes the
  release commit and build date when injected.
- `--json` emits one complete `schemaVersion: 1` document with `version`,
  `commit`, and `buildDate`. Source builds report `dev`, `unknown`, and
  `unknown` explicitly.

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
  `config`, `model-catalog`, and `default-model`.
- Pi `0.84.1` is verified and reports `ok`. A different valid semantic version
  reports `warning` and leaves the environment ready. Unreadable or malformed
  version output reports `failed`. A missing configuration is a warning;
  warnings leave the environment ready. A failed check makes it not ready.
- Skill installation status is reported separately by `pi-worker skill status`.
- `model-catalog` sends only `get_available_models`; it does not activate a
  model or send a prompt. An empty catalog is failed.
- The default timeout is `30s`. Human output has one line per ordered check and
  an overall readiness line. For a completed inspection, `--json` writes exactly
  one `doctor.Result` document to stdout. A timed-out, cancelled, or internally
  aborted inspection writes no result to stdout; `--debug` and diagnostics write
  only to stderr.
- Exit codes are `0` for ready or warning-only results, `3` for readiness
  failures, `7` for timeout, `8` for cancellation, and `9` for protocol or
  internal failures. Invalid flags return `2` before an inspection starts.

## Skill installation status

```text
pi-worker skill receipt-path [--json]
pi-worker skill status [--json]
```

- `skill receipt-path` reports the absolute path where the installer receipt is
  stored.
- `skill status` reads and verifies that receipt, including managed/affected
  target status and recovery instructions. The npm launcher also probes live
  global skill targets without adopting or changing them.
- The command is read-only: it never installs, repairs, removes, or adopts a
  skill.
- `--json` emits one complete `schemaVersion: 1` document with `status`,
  `receiptPath`, verified and receipt-tracked targets, affected targets,
  recovery instructions, and `externalInspection`.
- `externalInspection.state` is always `performed` or `unavailable`, and
  `targets` is always an array. Standalone native binaries report
  `unavailable`; the npm launcher reports `performed` only after every global
  target was resolved and inspected. Any partial failure discards all external
  findings. Missing paths are successful absence.
- External findings are informational and never change the receipt-derived
  exit code. Known identity markers are externally managed and may be stale;
  unknown or absent markers require manual inspection.
- Exit code `0` means all managed targets verified. Completion results for blocked,
  missing target, drifted target, skipped, and failed outcomes exit `3`.
- A missing receipt file reports `status: "missing"` and exits `3` with a complete JSON document.
- Malformed or unreadable receipts return code `9`.
- Usage, cancellation, and internal failures emit no `stdout` document.

## Exact run command

```text
pi-worker run [--model <provider/model>] [--thinking <level>] [--task <prompt> | --task-file <path>]... [--timeout <duration>] [--verify <command>] [--json] [--debug]
```

## Personal default model

`pi-worker` stores a two-field, versioned JSON configuration document in the
operating system's user configuration directory. It contains only
`schemaVersion` and `defaultModel`; the empty default is provider-neutral.

```text
pi-worker config show [--json]
pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]
```

Replace the provider/model placeholder with one exact selector printed by
`pi-worker models` before running `config set`.

- `config show` reads the local document only. It never launches Pi. A missing
  configuration is an empty default and exits `0`; an invalid configuration
  exits `9`.
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

- Before starting workers, `run` probes `pi --version` once for the whole run.
  An unverified, unreadable, timed-out, or otherwise failed probe writes one
  bounded warning to stderr and execution continues. It never changes JSON
  stdout or creates a new exit path; the RPC lifecycle still validates the
  effective model and thinking state fail-closed.

- `--model` must be an exact `provider/model` string.
- An explicit `--model` always wins over the configured default and does not
  read or rewrite the configuration document.
- `run` resolves the model by:
  1. `get_available_models`
  2. exact catalog match
  3. `set_model`
  4. confirmation check that `set_model` returns the exact same `provider` and `id`
- If no exact model match exists, or confirmation differs/missing, execution stops with an error. There is **no** pattern matching, fallback, or switching.

### Thinking level

- `--thinking` accepts exactly `off`, `minimal`, `low`, `medium`, `high`,
  `xhigh`, or `max` and applies to every worker in the run.
- Model and effort are separate: `provider/model:max` remains invalid.
- After exact model activation, every worker calls `get_state` to confirm the
  active model and capture Pi's default `thinkingLevel`.
- With explicit `--thinking`, the worker queries
  `get_available_thinking_levels`, applies the exact level when supported, and
  confirms the effective value with a second `get_state`.
- If the explicit level is unsupported or `set_thinking_level` returns a
  well-formed rejection, the worker keeps the captured Pi default, emits a
  warning, and continues. A successful task still exits `0`.
- Malformed RPC data, transport failure, active-model mismatch, or a successful
  set that is not confirmed are hard failures; they never fall back.
- When the flag is omitted, Pi's confirmed default is used and reported. The
  configuration file does not persist a thinking default.
- `models` and `doctor` do not enumerate thinking support because doing so
  requires activating a model; both commands remain inspection-only.

### Workspace verification

- `--verify <command>` may be given at most once and runs one check command
  in the workspace after the workers settle, once per run.
- No shell is involved: the value is split on whitespace into argv, so
  shell syntax cannot work. A value containing `|`, `&`, `;`, `<`, `>`,
  `$`, a backtick, a newline, a quote, or a backslash is rejected as a
  usage error naming the offending character, and an empty or
  whitespace-only value is rejected too. A quoted or escaped argument is
  refused rather than silently mis-split into stray fragments, so a
  command that needs shell quoting or escaping must be put in a script
  the check invokes instead. `--verify "go build ./... && go test
  ./..."` fails up front with the `&` named instead of executing
  something surprising.
- Verification runs only when the run completed with the context intact: a
  partial or failed run leaves the workspace half-written, and a timed-out
  or cancelled context skips the check.
- A passing check exits `0`. In human mode it prints one short
  `verification: ok` line after the worker summaries and carries no further
  output on purpose; in `--json` mode the result carries `verification`
  with `argv` and `exitCode: 0` only.
- A failing check (non-zero exit) exits the process `6` without changing
  the run `status` field or any worker status: those describe worker
  outcomes, and only the process exit code and the reported error carry
  the verification failure. Human mode prints the exit code and the
  captured excerpt (the first 2 KiB and the last 6 KiB when long, with the
  elided middle marked), plus the full `pi-worker-verify-*.log` path in
  the system temp directory when one was written; `--json` mode carries
  `output`, `truncated`, and `logFile` in the `verification` object.
- A context that expires while the check runs is not a verification
  failure: the run ran out of time and exits the way a timed-out run
  exits (`7`).

### Workspace git state

- When the current directory is inside a git work tree, `run` records
  the git state once before any worker starts and once after every
  worker settles: HEAD, the current branch, the dirty flag, and the
  stash count. There is no flag for this; it happens on every run.
- When the run moved HEAD, the branch, or the stash list, human mode
  prints one `pi-worker: warning: the run changed git state: ...` line
  on stderr naming what moved, and `--json` mode carries a `git` object
  with `before` and `after` states. A modified working tree alone does
  not trigger it: leaving modified files behind is the point of a run.
- The after state is collected on every terminal status, including a
  timed-out or cancelled run, under a fresh five-second budget when the
  parent context is already done.
- A workspace outside a git work tree, or a failed inspection, is a
  silent no-op: the result carries no `git` object and no warning is
  printed, and the run status and exit code are unchanged.

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

- Human output with confirmed state labels every result as
  `worker N [model=provider/model thinking=level]:`.
  - Completed worker output goes to stdout.
  - Failed/errored worker output goes to stderr.
- `--json` emits **exactly one** JSON object (single document) only after argument/input validation succeeds and a run starts, with:
  - `schemaVersion` = `1`
  - `status`
  - `workers` in input order (the same order as task inputs, not completion order)
  - each confirmed worker's effective `thinkingLevel`; explicit requests also
    include `requestedThinkingLevel`
  - fallback workers include `thinkingFallback: true` and a fixed `warning`
  - `verification`, when `--verify` ran on a completed run: `argv` and
    `exitCode` always; `output` (the captured excerpt), `truncated`, and
    `logFile` only for a failing check
  - `git`, when the run moved HEAD, the branch, or the stash list:
    `before` and `after` states, each with `head`, `branch`, `dirty`,
    and `stashes`
- Pre-run usage/input validation errors are written to stderr and may produce no JSON output.

Example:

```json
{"schemaVersion":1,"status":"completed","workers":[{"model":"provider/model-id","requestedThinkingLevel":"max","thinkingLevel":"high","thinkingFallback":true,"warning":"requested thinking=max unavailable; continuing with Pi default thinking=high","status":"completed","explanation":"Worker one done"}]}
```

### Exit codes

- `0` completed
- `2` usage
- `3` all workers unavailable / readiness path
- `5` task failure or partial completion; top-level `--json` `status` is
  `failed` for task failure and `partial` for partial completion.
- `6` verification failed; the run `status` stays `completed` and only the
  process exit code and the reported error carry the failure
- `7` timeout
- `8` cancellation
- `9` protocol/internal; for runs, no worker succeeded and any worker reported an
  internal error
- `4` (policy) is reserved and **not emitted by this v0 slice**.

### `--debug` debug stream

`--debug` writes sanitized lifecycle progress only to stderr. It includes:
- worker identity and start line
- RPC request status/duration (`get_available_models`, `set_model`,
  `get_state`, `get_available_thinking_levels`, `set_thinking_level`, `prompt`,
  `get_last_assistant_text`)
- requested and confirmed effective thinking; fallback is a fixed boolean field
- fixed model-phase transitions (`phase=model-thinking`, `phase=model-output`,
  `phase=model-tool-call`, or `phase=model-activity`); transitions are immediate
  and repeated same-phase events do not create a second heartbeat clock
- one lifecycle heartbeat from successful Pi child start until the terminal
  worker result: after 30 seconds without an emitted debug line it reports
  `phase=waiting-for-pi last-phase=<fixed-phase> silence=30s process=alive`;
  any emitted debug line resets the visible-line silence interval
- `last-phase` is pi-worker's latest fixed phase projection. `process=alive`
  means the managed Pi root has started and has not been reaped; it does not
  claim model or provider progress. The heartbeat reports observed silence and
  may also cover setup RPC waits.
- tool start/end status and duration; failed tools include a fixed `cause`, and
  bash nonzero exits also include `exit-code`
- settlement line
- worker completion and total duration

The debug stream is bounded to 512 lines per run: 315 regular lifecycle/tool/RPC
lines, 180 heartbeat lines, 16 reserved terminal lines, and one fixed budget
notice. The heartbeat is disabled, including its timer and goroutine, when
`--debug` is not enabled.

It does **not** print:
- prompts
- assistant text
- tool args/results
- raw frames
- environment values or credentials
- child stderr

### Ctrl-C / timeout cleanup and lifecycle boundary

- Ctrl-C and timeout cancel the shared run context.
- macOS/Linux: each child runs in its own process group, but cleanup avoids signalling that reusable numeric group; it kills Pi through Go's process handle and performs a best-effort, creation-time-verified descendant sweep.
- Windows: children are placed in a Job Object with kill-on-close.
- This is recovery, not a sandbox. Deliberately daemonized/reparented processes, processes spawned during the post-snapshot window, and the short Windows pre-assignment window can escape.
- If Pi exits and is reaped before cleanup can snapshot its lineage, surviving descendants may also escape; v0 does not continuously track descendants.

## Examples

### One direct task (human)

```sh
pi-worker run --model provider/model-id --thinking max --task "Implement the requested fix"
```

### Two/three tasks via files (parallel workers)

```sh
pi-worker run --model provider/model-id --thinking high --task-file ./task-a.txt --task-file ./task-b.txt
```

```sh
pi-worker run --model provider/model-id --thinking high --task-file ./task-a.txt --task-file ./task-b.txt --task-file ./task-c.txt
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

## Compatibility note

v0 is verified against Pi **0.84.1**. Other semantic versions are explicitly
reported as unverified while the runtime continues through its fail-closed RPC
validation. Re-probe before expanding the verified compatibility surface:

- [docs/pi-cli-surface.md](./pi-cli-surface.md)
