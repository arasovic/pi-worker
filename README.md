# Pi Worker

Pi Worker runs one to three foreground Pi workers in the current writable
workspace. Each worker uses the exact selected model, or an exact locally
configured default. The included agent skill is provider-neutral.

v0.1 is pre-release software. It is source-build only, pinned to Pi 0.84.1,
and is not a sandbox.

## Prerequisites

- Go 1.25 or later (the repository declares its exact toolchain).
- Pi CLI 0.84.1, with provider authentication configured in Pi.

Build from a local source checkout:

```sh
git clone <repository-url> pi-worker
cd pi-worker
go build -o ./bin/pi-worker ./cmd/pi-worker
./bin/pi-worker version
```

Source builds report `dev`.

## Quick start

List the available exact selectors, optionally save one as the local default,
then inspect readiness:

```sh
./bin/pi-worker models
./bin/pi-worker config set default-model provider/model-id
./bin/pi-worker doctor
```

Run one task directly:

```sh
./bin/pi-worker run --model provider/model-id --task "Implement the requested fix"
```

Run disjoint file-based tasks in parallel:

```sh
./bin/pi-worker run --model provider/model-id \
  --task-file ./task-a.txt --task-file ./task-b.txt
```

`run --model` has exact precedence: an explicit `--model`, then the configured
default model, then a usage error. Pi Worker never infers a model, falls back,
or switches providers.

## Workspace and safety boundary

Workers share the current writable workspace. Run only one to three workers;
parallel writes must target disjoint files. Every worker has `bash` enabled and
can run commands with the current user's host permissions. Pi Worker is not a
sandbox or worktree isolation layer.

## Output and exits

Human results use labeled worker lines. `run --json` writes one JSON document
after valid invocation; diagnostics, including sanitized `--debug` output, may
be written to stderr.

| Exit | Meaning |
| --- | --- |
| 0 | Completed, or inspection completed with warnings only |
| 2 | Usage or configuration input error |
| 3 | Readiness failure, including unavailable Pi or model |
| 5 | Task failure or partial completion |
| 7 | Timeout |
| 8 | Cancellation |
| 9 | Protocol or internal failure |

`models` and `doctor` are inspection-only: neither submits a prompt nor invokes
a model. Both report a missing or unavailable Pi as readiness exit 3.
`config set default-model` also checks the live catalog without prompting and
saves only an available exact selector. `config show` reads local configuration
only.

## Agent skill

The canonical source is [`skills/pi-worker`](./skills/pi-worker). It supports
one to three disjoint Pi-worker tasks, separate model/effort selection, visible
thinking fallback, and no recursive delegation. Skill installation status is
reported by `pi-worker skill status`.

## Details

- [Detailed v0 usage](./docs/v0-usage.md)
- [Pi 0.84.1 compatibility record](./docs/pi-cli-surface.md)

## Deferred to v1

- Trust and content-provenance controls
- Sandbox or container execution
- Worktree and patch-application isolation
- Background lifecycle and durable worker management
- Public installer, package-manager distribution, and automated skill installation
