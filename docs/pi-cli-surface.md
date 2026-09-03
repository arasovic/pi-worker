# Pi CLI and RPC Surface

## Scope and evidence

Observed binary: a local `pi` executable resolved from `PATH`.

Observed version: `0.84.4`.

Evidence was collected on 2026-08-10 from `pi --version`, `pi --help`, and
`pi auth --help`. The installed package source and bundled RPC documentation
were also inspected.
No authentication command that reads credentials was run. No model catalog,
prompt, or other billable operation was run.
The surface was re-probed on 2026-08-22 against 0.84.2 from `pi --version`
and `pi --help`, with `--mode rpc` and every required flag still present; no
authentication command, model catalog call, prompt, or other billable
operation was run in the re-probe.
The surface was re-probed on 2026-08-29 against 0.84.3 from `pi --version`
and `pi --help`, with the `--help` flag surface including `--mode rpc` and
every required flag still present; the RPC command names, thinking levels,
and response container shapes were re-checked in the installed package and
all held; no authentication command, model catalog call, prompt, or other
billable operation was run in the re-probe.
The surface was re-probed on 2026-08-29 against 0.84.4 from `pi --version`
and `pi --help`, with the `--help` flag surface including `--mode rpc` and
every required flag still present; the RPC command names, thinking levels,
and response container shapes were re-checked in the installed package and
all held; no authentication command, model catalog call, prompt, or other
billable operation was run in the re-probe.
The usage frame vocabulary was re-measured on 2026-08-29 against 0.84.4
with an instrumented pi-worker build running real prompt workloads on two
providers; the `assistantMessageEvent.type` subtypes observed on the wire
were `thinking_start`, `thinking_delta`, `thinking_end`, `text_start`,
`text_delta`, `text_end`, `toolcall_start`, `toolcall_delta`, and
`toolcall_end` — the vocabulary as observed, not a closed set — and no
`done` or `error` subtype was ever forwarded by the RPC transport. Numbers
appear on the end frame of each content block (`thinking_end`,
`toolcall_end`, `text_end`) and each carries the message's cumulative
usage so far, so a message may report more than one such frame with the
same figure. The delta frames report all-zero usage, and one provider
reported all-zero usage on every frame of a tool-using run.

## Compatibility gate

**Gate result: pass for Pi 0.84.4.** The expected `--mode rpc` surface and all
required flags are present. Pin or re-probe this exact surface before allowing
an unpinned Pi upgrade, because RPC command names and event shapes are not
guaranteed stable by this document.

| Required surface | Observed support |
| --- | --- |
| `--mode rpc` | Yes; `rpc` is one of `text`, `json`, and `rpc`. |
| `--model <pattern>` | Yes; accepts a model pattern or ID, including `provider/id` and optional `:thinking`. |
| `--session-dir <dir>` | Yes. |
| `--name <name>` | Yes. |
| `--no-extensions` | Yes; explicit `--extension` paths still load. |
| `--no-skills` | Yes. |
| `--no-prompt-templates` | Yes. |
| `--no-themes` | Yes. |
| `--no-approve` | Yes; ignores project-local files for the run. |
| `--tools <tools>` | Yes; comma-separated allowlist across built-in, extension, and custom tools. |

## Process invocations

Use this harmless probe launch for an isolated, read-only process. It
intentionally does not send a prompt and does not persist a session:

```sh
pi --mode rpc --offline --no-session --no-context-files --no-extensions --no-skills --no-prompt-templates --no-themes --no-approve --tools read,grep,find,ls
```

Use this writable worker launch with the current working directory as the
workspace. Pi-worker creates a fresh private session directory per worker in a
new OS temporary directory (`os.MkdirTemp("", "pi-worker-v0-*")`) before
launch, and currently runs every worker with `--no-approve` and this tool allowlist:

```sh
pi --mode rpc --session-dir <session-dir> --name <worker-id> --no-context-files --no-extensions --no-skills --no-prompt-templates --no-themes --no-approve --tools read,grep,find,ls,edit,write,bash
```

In v0, `bash` is always enabled. It executes arbitrary shell commands with the
current user's host permissions; `--tools` is a capability allowlist, not a
sandbox.

Use `--model provider/model-id` only after selecting an entry returned by
`get_available_models`, or use `set_model` after startup. The writable command
uses a dedicated session directory and name; concurrent workers must each use
their own session directory and name. Pi-worker must not reuse an existing
worker session directory.

`--no-extensions` does not block an explicitly supplied `--extension` path,
so the worker must never add that flag. `--tools` is an allowlist, not a
sandbox; it limits agent tools but does not replace process isolation. It also
does not prevent parallel workers from colliding on the same workspace files,
session directory, or other mutable resources.

Pi-worker's process cleanup terminates Pi and its descendants. On Windows it
uses an assigned Job Object; on macOS/Linux it kills Pi through Go's
reaped-aware process handle and best-effort sweeps its descendant lineage,
including ordinary descendants that moved to another process group. The sweep
records Pi's creation-time identity at startup and identity-checks the root and
each target to protect against pid reuse. This is lifecycle
recovery, not a sandbox or a no-escape guarantee: a deliberately daemonized
or reparented Unix process, a descendant spawned during the teardown sweep
itself, a surviving descendant after Pi exits and is reaped before cleanup can
take a lineage snapshot, and the short Windows pre-assignment window are
outside the v0 contract. V0 does not continuously track descendants.

## Authentication surface

The supported authentication help form is:

```sh
pi auth --help
```

The help lists `print-api-key`, `print-bearer-token`, and `check`. Do not run
the two print commands in a worker. `check` requires a provider or model and
can refresh OAuth credentials unless `--no-refresh` is used; it was not run.

## JSONL protocol

RPC consumes one JSON object per stdin line and emits one JSON object per
stdout line. LF is the protocol delimiter; clients may strip a trailing CR
from input. Requests may include an optional string `id`, echoed by their
response. Events generally do not include `id`; `bash_execution_update` does
when its direct `bash` request has one.

### Compact wire examples

The JSON objects below intentionally omit fields that are not relevant to the
example. The v0 consumer projection follows this section.

```json
{"id":"catalog-1","type":"get_available_models"}
{"id":"catalog-1","type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"...","id":"..."}]}}

{"id":"model-1","type":"set_model","provider":"...","modelId":"..."}
{"id":"model-1","type":"response","command":"set_model","success":true,"data":{"provider":"...","id":"..."}}

{"id":"state-1","type":"get_state"}
{"id":"state-1","type":"response","command":"get_state","success":true,"data":{"model":{"provider":"...","id":"..."},"thinkingLevel":"medium","isStreaming":false,"sessionId":"...","messageCount":0,"pendingMessageCount":0}}

{"id":"levels-1","type":"get_available_thinking_levels"}
{"id":"levels-1","type":"response","command":"get_available_thinking_levels","success":true,"data":{"levels":["off","minimal","low","medium","high","xhigh","max"]}}

{"id":"thinking-1","type":"set_thinking_level","level":"max"}
{"id":"thinking-1","type":"response","command":"set_thinking_level","success":true}

{"id":"prompt-1","type":"prompt","message":"..."}
{"id":"prompt-1","type":"response","command":"prompt","success":true}

{"id":"final-1","type":"get_last_assistant_text"}
{"id":"final-1","type":"response","command":"get_last_assistant_text","success":true,"data":{"text":"..."}}

{"id":"abort-1","type":"abort"}
{"id":"abort-1","type":"response","command":"abort","success":true}

{"id":"bad-1","type":"response","command":"set_model","success":false,"error":"..."}
```

The `abort` frames above are observed upstream commands but are not emitted by
v0. Pi-worker constructs `get_state` only for exact model/thinking
confirmation; it never forwards caller-supplied request shapes.

`set_model` requires an exact `provider` plus `modelId` that exists in the
current available-model snapshot; otherwise it returns `success: false`.
`get_last_assistant_text` omits the `text` key entirely (data:{}) when no
assistant message exists or its text is empty: the server serializes the
undefined "no text" value as `{}`, not as `{"text":null}` as its client
signature suggests. Empty or missing text means no usable answer, never a
protocol failure. A successful `prompt` response only means preflight
accepted, queued, or handled the prompt; later failures are emitted in the
event/message stream, including assistant messages with `stopReason:
"error"`. Pi-worker reports the stable error as `upstream/model turn ended with
an error`, without copying the message's `errorMessage`; any text emitted by
that failed turn is partial evidence, not a final explanation. A settled
assistant message with another stop reason and empty text retains the generic
empty-answer wording.

### V0 consumer projection

The installed `RpcResponse` failure variant is exactly:

```ts
{ id?: string; type: "response"; command: string; success: false; error: string }
```

The `get_available_models` success container is exactly
`{type:"response", command:"get_available_models", success:true,
data:{models: Model[]}}`. The `set_model` success container is exactly
`{type:"response", command:"set_model", success:true, data:Model}`. The
full version-pinned upstream `Model` declaration is in
`@earendil-works/pi-coding-agent@0.84.1/node_modules/@earendil-works/pi-ai/dist/types.d.ts`;
it is not duplicated here because v0 must not validate or reconstruct it.

V0 decodes each catalog entry as this projection only:

```ts
type ModelProjection = {
  provider: string;
  id: string;
};
```

`provider` and `id` must both be present and strings. Every other catalog field
is ignored by Go decoding and is never reconstructed or re-serialized. V0
sends only the exact `provider` and `id` returned by one catalog response in
its later `set_model` request.

V0 requires `set_model` success to carry `data:Model` and treats a missing,
null, mistyped, or mismatched confirmation as a protocol violation: the
response `provider` and `id` strings must exactly equal the requested catalog
pair. Success without that confirmation is never accepted.

Pi 0.84.1 observes the `get_available_thinking_levels` success container as
`data:{levels: ThinkingLevel[]}`, with exact levels `off`, `minimal`, `low`,
`medium`, `high`, `xhigh`, and `max`. V0 requires a non-null array of unique,
recognized strings. A well-formed `set_thinking_level success:false` is the
only setter rejection that worker policy may recover from; transport and
malformed responses remain failures.

Pi 0.84.1 observes the `get_state` success container exactly as
`{type:"response", command:"get_state", success:true, data:RpcSessionState}`.
V0 projects only `model.provider`, `model.id`, and `thinkingLevel`. All are
required after model activation; the model must equal the selected catalog
entry and thinking must be one recognized value. The full version-pinned
upstream declaration is in
`@earendil-works/pi-coding-agent@0.84.1/dist/modes/rpc/rpc-types.d.ts`; V0
does not reconstruct or re-serialize the remaining state.

### V0 outbound RPC allowlist

Pi-worker constructs every outbound JSON object itself and never forwards
caller-supplied JSON. Apart from an internally generated optional `id` for
response correlation, it emits only these request shapes:

| Type | Required fields |
| --- | --- |
| `get_available_models` | `type` |
| `set_model` | `type`, `provider: string`, `modelId: string` |
| `get_state` | `type` |
| `get_available_thinking_levels` | `type` |
| `set_thinking_level` | `type`, `level: ThinkingLevel` |
| `prompt` | `type`, `message: string` |
| `get_last_assistant_text` | `type` |

The observed upstream RPC `abort` command is not on this allowlist. Pi-worker
must reject and must not emit it.

Pi-worker must reject and must not emit every other RPC type. In particular,
it must reject direct RPC `bash`: Pi 0.84.1 dispatches that command directly,
so it bypasses the CLI `--tools` allowlist.

### Debug observability

The v0 debug projection uses fixed model phases: `model-thinking`,
`model-output`, `model-tool-call`, and `model-activity`. A `message_update`
frame is classified only from `assistantMessageEvent.type`; transitions are
reported immediately and repeated same-phase events do not create a second
heartbeat clock. One lifecycle heartbeat starts after the managed Pi child
starts successfully and continues through setup and model activity until the
terminal worker result. After 30 seconds without an emitted debug line it
reports the fixed projection
`phase=waiting-for-pi last-phase=<fixed-phase> silence=30s process=alive`.
Any emitted debug line resets this visible-line silence interval. `last-phase`
is pi-worker's fixed phase projection; `process=alive` means the managed Pi
root has started and has not been reaped, not that model or provider progress
is occurring. This heartbeat reports observed silence and may cover a slow
setup RPC.

For failed tools, only an exact `bash` tool name is eligible for a cause
projection from the final `result.content` text entry: nonzero exits report
`cause=nonzero-exit exit-code=N`, timeouts report `cause=timeout`, and aborts
report `cause=cancelled`. Other and malformed forms, and all non-bash failures,
report `cause=unknown`. Debug output never includes command, result, argument,
identifier, path, or credential data. The run-level debug stream is bounded to
512 lines: 315 regular lifecycle/tool/RPC lines, 180 heartbeat lines, 16
reserved terminal lines, and one fixed budget notice. The lanes are
independent. The single `debug budget exhausted` notice reports the first lane
to fill and suppresses only that lane, so heartbeat and terminal lines can
still follow it.

### Completion and final text

Treat `{"type":"agent_settled"}` as the terminal condition for a submitted
prompt. It means no automatic retry, compaction retry, or queued continuation
remains. Do not treat `agent_end` as terminal: it describes one low-level run
and can be followed by retry, compaction, or queued work. At settlement, use
`get_last_assistant_text` for the final assistant text; use
`message_end.message` as the authoritative complete message if reconstructing
the event stream.

Relevant events include:

```json
{"type":"agent_start"}
{"type":"message_update","usage":{"input":1200,"output":340,"cacheRead":0,"cacheWrite":0,"totalTokens":1540,"cost":{"input":0.0012,"output":0.0017,"cacheRead":0,"cacheWrite":0,"total":0.0029}},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"..."}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"..."}]}}
{"type":"turn_end","message":{},"toolResults":[]}
{"type":"agent_end","messages":[],"willRetry":false}
{"type":"agent_settled"}
```

`message_update` is delta-only for message content: reconstruct live text
by `contentIndex` from `message_start` plus update events, and do not
expect a cumulative `message` field. Its `usage` field is the exception to
the delta semantics in the other direction: numbers appear on the end
frame of each content block (`thinking_end`, `toolcall_end`, `text_end`)
and carry the message's cumulative usage so far, so a message may report
more than one such frame with the same figure; the delta frames carried
all-zero usage objects in the observed runs — never a running total and
never a delta. No `message_update` frame type terminates the
measurement; pi-worker reads the latest usage a message reported, bounded
by `message_start` and `message_end`. Assistant `stopReason` can be
`stop`, `length`, `toolUse`, `error`, or `aborted`; it is not the session
terminal condition. A latest assistant `message_end` with
`stopReason: "error"` makes the worker fail with `upstream/model turn ended
with an error`; this wording does not claim that no text existed and does not
attribute the error to a particular provider mechanism. Text emitted by that
failed turn is exposed only as `partialExplanation`, never as `explanation`.
Partial assistant text retained for this field is capped at `MaxFrameBytes`
(8 MiB) UTF-8 bytes across the in-flight and most-recent message buffers;
when the cap is reached, older text is evicted without splitting a UTF-8 rune.
A newer valid assistant message supersedes the prior classification; user and
tool-result messages do not. A missing or malformed stopReason on the latest
assistant message does not inherit an earlier error. The assistant's
`errorMessage` is not a pi-worker result projection.

## Tool semantics

Built-in tool names reported by `pi --help` are `read`, `bash`, `edit`,
`write`, `grep`, `find`, and `ls`. In v0, Pi-worker currently enables all seven
and always includes `bash`, which can run arbitrary shell commands with the
user's host permissions. `--tools` is an allowlist of capabilities, not a
sandbox.

`find` accepts `{ "pattern": string, "path"?: string, "limit"?: number }`.
Its installed implementation searches files by glob pattern, returns paths
relative to the search directory, respects `.gitignore`, and truncates at a
default of 1,000 results or 50 KiB. Its default implementation resolves an
`fd` executable, then spawns that subprocess with fixed glob-search arguments;
the tool interface exposes no arbitrary command string, deletion, edit, or
write operation.

Pi resolves the executable from its tools directory first, then from `PATH`
as `fd` or `fdfind`. If neither is available, `ensureTool("fd", true)` can
download and install `fd`, which writes to Pi's tools directory. `--offline`
blocks that acquisition and makes `find` fail if no resolved `fd` exists.
Pi-worker or its operator must trust or validate the resolved executable
source and `PATH`. `find` is not a general filesystem or process-execution
safety boundary.

## Compatibility risks

- `--model` accepts patterns at process startup, while RPC `set_model` accepts
  exact `provider` and `modelId`; a worker must not assume one syntax works in
  the other location.
- Available models are a runtime snapshot. Choose from
  `get_available_models` rather than hard-coding an unverified catalog entry.
- The package source exposes additional RPC commands, including direct `bash`.
  Do not forward arbitrary RPC input; admit only the worker command subset.
- Extensions, skills, prompt templates, themes, and context files can modify
  behavior. Preserve every disabling flag in the safe invocation.
- `--offline` disables startup network operations. It does not prove that a
  later prompt cannot require provider connectivity, so the worker must keep
  prompt execution under its own authorization and billing controls.

## Source locations inspected

- `docs/rpc.md`: RPC framing, command examples, event semantics, terminal
  condition, and message types.
- `dist/modes/rpc/rpc-types.d.ts`: installed command and response unions.
- `dist/modes/rpc/rpc-mode.js`: command dispatch, exact-model lookup, and
  settlement subscription behavior.
- `dist/core/tools/find.js`: `find` schema and read-only glob implementation.
