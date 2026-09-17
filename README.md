<p align="center">
  <img src="https://raw.githubusercontent.com/arasovic/pi-worker/main/assets/brand/github-social-preview.png" alt="Pi Worker">
</p>

<p align="center">
  <a href="https://github.com/arasovic/pi-worker/actions/workflows/ci.yml"><img src="https://github.com/arasovic/pi-worker/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://www.npmjs.com/package/pi-worker"><img src="https://img.shields.io/npm/v/pi-worker.svg" alt="npm version"></a>
  <img src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&amp;logoColor=white" alt="Go 1.25+">
  <a href="./LICENSE"><img src="https://img.shields.io/badge/MIT-green.svg" alt="MIT license"></a>
</p>

Let your coding agent hand small, well-defined tasks to a cheaper model.
Pi Worker runs them through [Pi](https://pi.dev/) and brings the results back.

## What is it?

Pi Worker is a small CLI and coding-agent skill. Your primary agent stays the
orchestrator; Pi Worker runs one to three workers in parallel and returns each
worker's status and final explanation in request order.

Already on Pi? This is `pi` with a queue, parallelism and a result contract.

## How it works

```text
orchestrator (Claude Code / Codex / Hermes)
      │  pi-worker run --task ... (1-3 tasks)
      ▼
pi-worker ── starts one Pi process per task, waits, collects
      ▼
status + final explanation per task, in request order
```

Workers run concurrently, each on the exact model you asked for, and results
stay in request order rather than completion order.

## Install

Requirements: Node.js 22.20+, a [Pi](https://pi.dev/) CLI with provider
authentication, and macOS or Linux on arm64 or x64.

```sh
npm install -g pi-worker
```

`go install github.com/arasovic/pi-worker/cmd/pi-worker@latest` installs the
binary without the skill; other platforms and source builds are in
[detailed usage](./docs/v0-usage.md).

## First run

Run these commands, replacing `provider/model` with one exact selector printed
by `pi-worker models`.

```sh
pi-worker models
pi-worker doctor
pi-worker config set default-model provider/model
pi-worker run --thinking high --task "Review this module and explain the main risks"
```

The requested model never silently changes; thinking levels are in
[detailed usage](./docs/v0-usage.md).

## From a coding agent

```text
Use pi-worker with provider/model at high effort to complete this task.
```

Repeat `--task` for up to three independent tasks that touch disjoint files.

> **Safety:** Pi Worker is not a sandbox. Workers can edit the current workspace
> and run `bash` with the current user's permissions. Parallel tasks must touch
> disjoint files.

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
