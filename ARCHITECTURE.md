# Architecture

Pi Worker is a standalone adapter between an external coding agent and the
models configured in Pi.

```text
Codex, Claude, or Hermes
        -> pi-worker CLI
        -> Pi RPC
        -> selected model
```

The external coding agent remains the orchestrator. Pi Worker starts the child
workers, verifies their configuration, manages their lifecycle, and returns a
stable result.

## Product Boundary

Pi Worker owns a deliberately small contract:

- exact `provider/model` selection;
- no silent model substitution;
- separate requested and effective thinking levels;
- explicit same-model thinking fallback;
- one to three tasks executed concurrently, with results returned in request
  order;
- one aggregate JSON document and stable exit codes;
- bounded, redacted lifecycle diagnostics on stderr;
- direct CLI use without requiring an MCP host.

It is not intended to become a general subagent platform. Agent personas,
chains, long-lived pools, planning, nested delegation, and broad workflow
engines belong in Pi or its extension ecosystem.

## Runtime Design

The Go runtime communicates with Pi through its documented JSONL RPC mode. For
each worker it:

1. starts a dedicated Pi process;
2. reads the available model catalog;
3. requires an exact provider and model match;
4. activates the selected model;
5. reads session state to confirm the effective model and thinking level;
6. submits the task and waits for settlement;
7. collects the final explanation;
8. reaps the managed process and session resources.

Parallel tasks share the caller's writable workspace and must have disjoint
file ownership. Pi Worker provides lifecycle management, not a sandbox or
worktree isolation layer.

## Validation Boundary

Model and thinking values may originate in an LLM-generated command, so Pi
Worker validates them before treating them as effective configuration.

Catalog discovery alone is not sufficient. The runtime checks exact catalog
membership, activates the model, and confirms the resulting session state.
Unsupported thinking levels remain on the same model, use Pi's confirmed
default, and produce an explicit warning.

This validation boundary is the main reason Pi Worker uses RPC instead of only
forwarding command-line arguments to Pi.

## Evaluated Alternatives

### Direct `pi -p`

Direct Pi is the smallest way to delegate a task:

```sh
pi -p --model provider/model --thinking high --no-session "Task"
```

It works well for lightweight use, and an external orchestrator can run several
processes concurrently. The caller must supply ordering, aggregation, timeout
and cancellation behavior, cleanup, effective-state validation, diagnostics,
and error classification.

Pi Worker packages those responsibilities behind one stable CLI contract.

### `@parke.dev/pi-subagent`

The `@parke.dev/pi-subagent@0.8.0` SDK can run Pi workers from an external Node
process without a parent Pi model turn. It provides useful lifecycle,
cancellation, stall detection, retry, worktree, usage, and cost machinery.

Its current public result surface does not expose the complete effective Pi
state needed here:

- `TaskResult.thinking` echoes the requested value rather than the effective
  one;
- the reported model comes from an assistant message rather than confirmed
  session state;
- `get_state` is parsed for `sessionId`, while provider, model, and
  `thinkingLevel` are not projected to the result.

The public `ChildRunner` accepts a custom `BackendAdapter`, but the built-in Pi
backend and protocol parser are not public exports. Using a custom adapter
would therefore move Pi protocol ownership into another local implementation.

Version 0.8.0 also exports TypeScript source rather than compiled JavaScript,
so normal Node consumption requires a TypeScript loader or a bundling step.

### `pi-subagent-bridge`

`pi-subagent-bridge` is prior art for the same external-orchestrator-to-Pi
direction. Version 0.2.0 was published on 2026-07-12, before Pi Worker. The
implementation evaluated here was version 0.4.0, which provides a broad MCP
surface for model discovery, parallel runs, steering, cancellation, sessions,
worktrees, events, and persisted run state.

The evaluated version's integration contract differs from Pi Worker:

- catalog lookup is available but is not a required gate for each run;
- model and thinking arguments are forwarded without confirming effective
  session state;
- failures use MCP semantics instead of process exit codes;
- consumers take on an MCP tool/schema surface and SQLite-backed state;
- the packaged installation is oriented around a Codex plugin.

The bridge is a strong option when its richer MCP workflow is desired. Pi
Worker instead favors a small, portable CLI and mandatory state validation.

## Trade-offs

The standalone runtime keeps behavior independent of a particular
orchestrator or extension host, but it also means maintaining:

- Pi RPC compatibility;
- process and session cleanup across supported platforms;
- CLI, JSON, debug, doctor, and configuration contracts;
- Go and npm build and release verification;
- a larger test suite than a shell launcher would require.

These costs are accepted in exchange for a narrow and reproducible integration
boundary. General subagent features should continue to be delegated to the Pi
ecosystem rather than added here.

## Future Simplification

The standalone Pi RPC layer can be reconsidered if an SDK provides:

- effective provider, model, and thinking state through a stable public result;
- compiled JavaScript exports that can be consumed directly;
- a reproducible, integration-tested Pi compatibility range.

Any replacement should preserve exact model selection, confirmed thinking, no
silent fallback, stable JSON and exit semantics, bounded diagnostics,
cancellation, and direct use from different coding agents.
