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
  <img src="https://img.shields.io/badge/Windows-source-0078D4" alt="Windows source">
  <a href="./LICENSE"><img src="https://img.shields.io/badge/MIT-green.svg" alt="MIT license"></a>
</p>

Delegate bounded coding tasks to exact models available through your local
[Pi](https://pi.dev/) installation.

## What is it?

Pi Worker is a small CLI and coding-agent skill. Your primary agent remains the
orchestrator; Pi Worker starts one to three workers in parallel, keeps results
in request order, and returns each worker's status and final explanation.

## Why does it exist?

Using the primary agent for every subtask can consume an expensive or limited
quota. Pi may already expose a lower-cost model or a model billed through a
separate account. Pi Worker removes the manual prompt-and-result copying while
enforcing an exact provider/model choice and reporting the effective thinking
level.

## How do I use it?

You need Node.js 22.20.0 or newer and a [Pi](https://pi.dev/) CLI with
provider authentication.
Pi `0.84.4` is verified; other semantic versions run with an explicit warning.

Install the npm package:

```sh
npm install -g pi-worker
```

The npm package supports only macOS and Linux on arm64 and x64. It includes the
native binary and provider-neutral skill. npm install attempts to install the
skill for detected coding agents through pinned `skills@1.5.23`; it never
overwrites an unrecognized existing skill.

Native archives are available from
[GitHub Releases](https://github.com/arasovic/pi-worker/releases). Windows users
must build from source; Windows is compile-checked but not runtime-tested.
Source builds are documented in [detailed usage](./docs/v0-usage.md). The Go
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
> `bash` with the current user's host permissions. Pi Worker is not a sandbox
> or worktree layer. Use a trusted workspace; parallel tasks must be disjoint.

Choose an exact selector, check readiness, save a default, and run a task.
Replace `provider/model` with one exact selector printed by `pi-worker models`
before config set:

```sh
pi-worker models
pi-worker doctor
pi-worker config set default-model provider/model
pi-worker run --thinking high --task "Review this module and explain the main risks"
```

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
Parallel writes must target disjoint files because every worker shares the
current writable workspace.

## Safety

Pi Worker is not a sandbox. bash has the current user's host permissions, and
workers can edit the current workspace. Use one worker for overlapping work.

An exact requested model never silently changes. Thinking fallback stays on the
same model and reports the fallback. Installer state is recorded in a durable
receipt; the stable identity marker distinguishes recognized Pi Worker content.

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
