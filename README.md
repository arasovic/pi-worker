# Pi Worker

Pi Worker lets a primary coding agent delegate work to models available
through your local Pi installation.

## What is it?

Keep your preferred coding agent as the orchestrator and send a bounded task
to an exact model configured in Pi. Pi Worker starts the worker, waits for it,
and returns its status and final explanation. It can run up to three independent
tasks in parallel.

## Why does it exist?

Using the primary agent for every subtask can consume an expensive or limited
quota. Pi may already expose a lower-cost API model or a model billed through a
separate account. Pi Worker removes the manual prompt-and-result copying from
that workflow.

## How do I use it?

Pi is a local coding-agent CLI that provides the model catalog and worker
runtime used by Pi Worker. Choose an exact `provider/model` selector from Pi,
then run a bounded task in your current workspace.

## Requirements

- Node.js 22.20.0 or newer and npm.
- Pi CLI with provider authentication configured in Pi. Version 0.84.1 is the
  verified compatibility surface; other semantic versions run with an explicit
  unverified-version warning.
- An available exact model selector from `pi-worker models`.
- Go 1.26.1 for source builds and release snapshots; the module language baseline is Go 1.25.0.

The npm package supports only macOS and Linux on arm64 and x64. Windows users
must build from source; Windows is compile-checked but not runtime-tested in
the current release gates.

## Install with npm

Pi Worker is not published to npm yet. The following commands are the intended
post-publication install path; they are not currently usable from the registry:

```sh
npm install -g pi-worker
```

For foreground installer diagnostics, use:

```sh
npm install -g --foreground-scripts pi-worker
```

For the current checkout, follow the
[source-build instructions](./docs/v0-usage.md#source-build). Commands use
`./bin/pi-worker` after a source build.

## First run

> **Safety before running a worker task:** Workers can modify the current
> writable workspace and execute `bash` with the user's host permissions; they
> are not a sandbox or worktree layer. Use a trusted workspace only; parallel
> tasks must be disjoint.

Inspect available exact selectors and local readiness, then save one exact
selector and run a task. Replace `provider/model` with one exact selector
printed by `pi-worker models` before config set:

```sh
pi-worker doctor
pi-worker models
pi-worker config set default-model provider/model
pi-worker run --thinking high --task "Review this module and explain the main risks"
```

`doctor` is inspection-only. Its five checks, in order, are
`pi-executable`, `pi-version`, `config`, `model-catalog`, and `default-model`.
Skill installation status is separate.

Thinking effort is separate from model identity. Use `off`, `minimal`, `low`,
`medium`, `high`, `xhigh`, or `max` with `--thinking`. If an explicit effort is
unsupported, Pi Worker keeps the same selected model, uses that model's
confirmed Pi default effort, and reports the fallback.

## Use from a coding agent

Give the coding agent an exact model and effort instruction:

```text
Use pi-worker with provider/model at high effort to complete this task.
```

The agent should report the selected model, effective effort, status, and final
explanation. An explicit model or the configured exact default is required;
Pi Worker does not infer a replacement.

## Run independent tasks in parallel

Run one to three independent tasks with disjoint file ownership:

```sh
pi-worker run --model provider/model \
  --thinking high \
  --task-file ./task-a.txt --task-file ./task-b.txt --task-file ./task-c.txt
```

Use one task for overlapping work. Parallel workers share the workspace, so
parallel writes must target disjoint files.

## What gets installed

- The package contains four native binaries for macOS/Linux arm64/x64. The
  launcher selects the matching binary at runtime; installation does not remove
  the others.
- It contains the canonical provider-neutral `pi-worker` skill.
- npm install attempts to install the bundled provider-neutral skill for detected
  agent targets via pinned `skills@1.5.22` and records an installed,
  blocked, skipped, or failed outcome in the durable receipt. Existing conflicts
  may block, skip, or fail without overwriting.

## Safety

Workers use the current writable workspace. Parallel writes must target
disjoint files. bash has the current user's host permissions, and Pi Worker
is not a sandbox or worktree isolation layer.

An exact requested model/provider never silently changes. An explicit effort
fallback stays on the same model and is reported.

## Install from GitHub Releases

Pi Worker is not published yet. Release links will be added when available.

## Troubleshooting

Check skill installation separately from the five doctor checks:

```sh
pi-worker skill status
pi-worker skill status --json
pi-worker skill receipt-path
npx --yes skills@1.5.22 list -g
```

`skill status --json` reports managed targets, recovery information, and live
external-skill inspection when invoked through the npm launcher;
`skill receipt-path` reports the durable receipt location. If npm installation
is otherwise unclear, rerun it in the foreground:

```sh
npm install -g --foreground-scripts pi-worker
```

For identity-verified unmanaged or drifted Pi Worker content only: inspect
every affected path and make a backup first. Only after every affected path is
verified as Pi Worker content may you run:

```sh
npx --yes skills@1.5.22 remove pi-worker -g -y
npm install -g --foreground-scripts pi-worker
```

Never use that global remove command for markerless, foreign, or mixed
conflicts. Preserve those paths, inspect them, and resolve them separately.
A direct GitHub skill installation includes the stable Pi Worker identity
marker. A later npm install recognizes it as externally managed, never adopts
or overwrites it, and reports that it may be stale. Markerless content remains
a manual conflict. An unknown marker may belong to a newer Pi Worker version;
inspect it without automatic removal or reinstall.

## Advanced documentation

- [Architecture and evaluated alternatives](./ARCHITECTURE.md)
- [Detailed v0 usage](./docs/v0-usage.md)
- [Pi CLI compatibility and RPC surface](./docs/pi-cli-surface.md)
- [Release snapshot runbook](./docs/releasing.md)
- [Contributing](./CONTRIBUTING.md)
- [Security](./SECURITY.md)

The detailed documents contain exit-code tables, RPC details, compatibility
evidence, lifecycle boundaries, and source-build information. Run `npm run verify`
for the complete local Node/package verification.

## License

See [LICENSE](./LICENSE) and [THIRD_PARTY_NOTICES](./THIRD_PARTY_NOTICES).
