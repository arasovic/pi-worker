<p align="center">
  <img src="https://raw.githubusercontent.com/arasovic/pi-worker/main/assets/brand/github-social-preview.png" alt="Pi Worker">
</p>

<p align="center">
  <a href="https://github.com/arasovic/pi-worker/actions/workflows/ci.yml"><img src="https://github.com/arasovic/pi-worker/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://www.npmjs.com/package/pi-worker"><img src="https://img.shields.io/npm/v/pi-worker.svg" alt="npm version"></a>
  <img src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&amp;logoColor=white" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/Node.js-22.20%2B-339933?logo=nodedotjs&amp;logoColor=white" alt="Node.js 22.20+">
  <img src="https://img.shields.io/badge/macOS-000000?logo=apple&amp;logoColor=white" alt="macOS">
  <img src="https://img.shields.io/badge/Linux-FCC624?logo=linux&amp;logoColor=black" alt="Linux">
  <img src="https://img.shields.io/badge/Windows-compile%20only-0078D4" alt="Windows compile only">
  <a href="./LICENSE"><img src="https://img.shields.io/badge/MIT-green.svg" alt="MIT license"></a>
</p>

Delegate bounded coding tasks to exact models available through your local
[Pi](https://pi.dev/) installation.

## What is it?

Pi Worker is a small CLI and coding-agent skill. Your primary agent remains the
orchestrator; Pi Worker starts one to three workers in parallel, keeps results
in request order, and returns each worker's status and final explanation.

Foreground tasks are admitted through a file-backed machine-wide FIFO that
enforces `maxModelWorkers` (effective default 3) across all Pi Worker processes
on the same host. Admitted tasks may run concurrently up to `maxModelWorkers`;
queue time is bounded at 15 minutes and does not consume the `--timeout`
budget.

## Why does it exist?

Using the primary agent for every subtask can consume an expensive or limited
quota. Pi may already expose a lower-cost model or a model billed through a
separate account. Pi Worker removes the manual prompt-and-result copying while
enforcing an exact provider/model choice and reporting the effective thinking
level.

## How do I use it?

You need Node.js 22.20.0 or newer and a [Pi](https://pi.dev/) CLI with
provider authentication.
Pi `0.85.0` is verified; other semantic versions run with an explicit warning.

Install the npm package:

```sh
npm install -g pi-worker
```

The npm package supports only macOS and Linux on arm64 and x64. It includes the
native binary and provider-neutral skill. npm install attempts to install the
skill for detected coding agents through pinned `skills@1.5.23`; it never
overwrites an unrecognized existing skill.

Native archives are available from
[GitHub Releases](https://github.com/arasovic/pi-worker/releases). macOS and
Linux are the distributed runtime targets; the npm package supports only those
systems. Windows, FreeBSD, OpenBSD, NetBSD, Solaris, and Plan 9 are compile
gates only: CI cross-compiles the Windows binary and its test packages and
compile-checks the other targets' binaries, but none of these platforms is
runtime-tested or a released platform, so Windows users must build
from source. Source builds are documented in [detailed usage](./docs/v0-usage.md). The Go
binary can also be installed directly without the bundled skill:

```sh
go install github.com/arasovic/pi-worker/cmd/pi-worker@latest
```

The binary lands in `go env GOBIN`, or in `$(go env GOPATH)/bin` when GOBIN
is unset. That directory must be on PATH:

```sh
command -v pi-worker
```

> **Safety:** Workers can modify the current writable workspace and execute
> `bash` with the current user's host permissions. Pi Worker is not a sandbox.
> `--worktree` gives one run a separate working directory, not containment: a
> worker can still reach outside it. Use a trusted workspace; parallel tasks
> must be disjoint.

Choose an exact selector, check readiness, save a default, and run a task.
Replace `provider/model` with one exact selector printed by `pi-worker models`
before config set:

```sh
pi-worker models
pi-worker doctor
pi-worker config set default-model provider/model
pi-worker run --thinking high --task "Review this module and explain the main risks"
```

The configuration file is schema-versioned. Schema 2 carries `schemaVersion`,
`defaultModel`, and `maxModelWorkers` (positive integer, effective default 3).
Schema-1 files containing only their historical fields (`schemaVersion` and
`defaultModel`) are accepted and migrated in memory to schema 2 with
`maxModelWorkers: 3` without being rewritten on disk. The stored machine
setting can be changed with `pi-worker config set max-model-workers <n>`.

Doctor is read-only. Its six checks, in order, are `pi-executable`,
`pi-version`, `config`, `model-catalog`, `default-model`, and `workspace`.
A `workspace` warning is advisory and never blocks a run: it means the
current directory is not inside a confirmed git work tree, so a run there
cannot prove what it changed — its change manifest states an omission and
its declared-writes check is skipped.

Thinking is separate from model identity. Accepted values are `off`, `minimal`,
`low`, `medium`, `high`, `xhigh`, and `max`. An unsupported explicit effort
keeps the same model, uses Pi's confirmed default, and reports the fallback.
The requested model/provider never silently changes.

From a coding agent, a request can be as short as:

```text
Use pi-worker with provider/model at high effort to complete this task.
```

Run up to three independent tasks by repeating `--task` or `--task-file`.
Parallel writes must target disjoint files because every worker shares
the current writable workspace. For all-declared disjoint multi-task
runs with Git measurement, the monitor compares two identities per task
— immediately after that worker returns and after all workers settle
(not continuous tracing) — and proven interference (final identity
differs from settled identity) is reported as undeclared and exits 4;
pi-worker never restores files; a foreign write fully made/reverted
before the owner's settlement snapshot is invisible, as is an interim
post-settlement write restored to the exact settled identity before the
final snapshot (in that latter case the owner's final output is intact);
see [detailed usage](./docs/v0-usage.md).

Manage checkouts created with `run --worktree <name>` — only the exact
Git-registered pair at `<repo-root>/.pi-worker/worktrees/<valid-name>` on
branch `run/<same-name>` is managed:

```sh
pi-worker worktrees list
pi-worker worktrees list --json
pi-worker worktrees remove my-feature --yes
```

`list` is read-only, sorted by name, and reports
`name`/`path`/`branch`/`dirty`/`merged` (`merged` against the caller’s
current `HEAD`, not a branch named `main`). `remove` deletes only a clean
checkout whose branch is merged into the caller’s current `HEAD`; there is no
force option. Human mode shows the selected row and asks `[y/N]` — only
`y`/`yes` proceeds and `--yes` skips only the question; JSON and nonterminal
use require `--yes`. See [detailed usage](./docs/v0-usage.md) for refusal and
retry rules and [JSON contracts](./docs/json-contracts.md) for JSON shapes.

## Safety

Pi Worker is not a sandbox. bash has the current user's host permissions, and
workers can edit the current workspace. Use one worker for overlapping work.

An exact requested model never silently changes. Thinking fallback stays on the
same model and reports the fallback. Each worker gets at most three startup/
handshake attempts before the prompt, each attempt uses a fresh process, and
the task prompt itself is not retried. A later successful attempt carries a
warning naming the retry. Installer state is recorded in a durable receipt;
the stable identity marker distinguishes recognized Pi Worker content.

## Troubleshooting

Show foreground installer diagnostics and inspect skill state:

```sh
npm install -g --foreground-scripts pi-worker
pi-worker skill status
pi-worker skill status --json
pi-worker skill receipt-path
npx --yes skills@1.5.23 list -g
```

A separately installed recognized skill is externally managed and may be stale.
Markerless, foreign, or mixed content is never overwritten automatically. After
backing up and verifying every affected path as Pi Worker content, recovery may
require:

```sh
npx --yes skills@1.5.23 remove pi-worker -g -y
npm install -g --foreground-scripts pi-worker
```

Do not use the global remove command for unrecognized content.

## Documentation

- [Detailed usage](./docs/v0-usage.md)
- [Versioned JSON contracts](./docs/json-contracts.md)
- [Architecture](./ARCHITECTURE.md)
- [Pi compatibility surface](./docs/pi-cli-surface.md)
- [Release snapshot runbook](./docs/releasing.md)
- [Contributing](./CONTRIBUTING.md)
- [Security](./SECURITY.md)

## License

See [LICENSE](./LICENSE) and [THIRD_PARTY_NOTICES](./THIRD_PARTY_NOTICES).
