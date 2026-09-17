<p align="center">
  <img src="https://raw.githubusercontent.com/arasovic/pi-worker/main/assets/brand/github-social-preview.png" alt="Pi Worker">
</p>

<p align="center">
  <a href="https://github.com/arasovic/pi-worker/actions/workflows/ci.yml"><img src="https://github.com/arasovic/pi-worker/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://www.npmjs.com/package/pi-worker"><img src="https://img.shields.io/npm/v/pi-worker.svg" alt="npm version"></a>
  <img src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&amp;logoColor=white" alt="Go 1.25+">
  <a href="./LICENSE"><img src="https://img.shields.io/badge/MIT-green.svg" alt="MIT license"></a>
</p>

<p align="center"><strong>Let your coding agent hand small, well-defined tasks to a cheaper model.</strong></p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="./docs/v0-usage.md">Detailed usage</a> ·
  <a href="./ARCHITECTURE.md">Architecture</a>
</p>

## How it works

- **You keep your agent.** Claude Code, Codex or Hermes stays the orchestrator.
- **Small tasks go to cheaper models.** Pi Worker runs up to three [Pi](https://pi.dev/) workers in parallel, each on the exact model you asked for.
- **Results come back in order.** Every worker returns its status and a final explanation, in request order.
- **Your agent already knows how.** `npm install` also installs the `pi-worker` skill into Claude Code, Codex, Hermes and 70+ other detected coding agents, so the agent calls `pi-worker` itself.

Already on Pi? This is `pi` with a queue, parallelism and a result contract.

## Quick start

Requirements: Node.js 22.20+, a [Pi](https://pi.dev/) CLI with provider authentication, macOS or Linux on arm64 or x64.

1. **Install.** This also installs the skill for every detected coding agent.

   ```sh
   npm install -g pi-worker
   ```

   `go install github.com/arasovic/pi-worker/cmd/pi-worker@latest` gives you the binary without the skill.

2. **Pick a model.** Replace `provider/model` with one exact selector printed by `pi-worker models`.

   ```sh
   pi-worker models
   pi-worker doctor
   pi-worker config set default-model provider/model
   ```

3. **Run a task**

   ```sh
   pi-worker run --thinking high --task "Review this module and explain the main risks"
   ```

4. **Or let your agent do it.** Paste this into your coding agent:

   ```text
   Use pi-worker with provider/model at high effort to complete this task.
   ```

The requested model never silently changes. Repeat `--task` for up to three independent tasks that touch disjoint files. Thinking levels, `pi-worker skill status`, source builds and other platforms are in [detailed usage](./docs/v0-usage.md).

> [!WARNING]
> **Safety:** Pi Worker is not a sandbox. Workers can edit the current workspace and run `bash` with the current user's permissions. Parallel tasks must touch disjoint files.

## Documentation

- [Detailed usage](./docs/v0-usage.md) — every command, flag, exit code and edge case.
- [JSON contracts](./docs/json-contracts.md) — the versioned `--json` output shapes.
- [Architecture](./ARCHITECTURE.md) — how a run is admitted, supervised and measured.

Also: [Contributing](./CONTRIBUTING.md) · [Security](./SECURITY.md) · [Release runbook](./docs/releasing.md) · [Pi compatibility surface](./docs/pi-cli-surface.md)

## License

See [LICENSE](./LICENSE) and [THIRD_PARTY_NOTICES](./THIRD_PARTY_NOTICES).
