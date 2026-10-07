# Pi CLI and RPC Surface

## Scope and evidence

Observed binary: a local `pi` executable resolved from `PATH`.

Observed version: `1.0.4`.

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
The surface was re-probed on 2026-09-04 against 0.85.0 from `pi --version`
and `pi --help` (`npm view @earendil-works/pi-coding-agent version` and the
local global package both report `0.85.0`); `pi --help` still exposes every
required pi-worker launch flag (`--mode rpc`, `--model`, `--session-dir`,
`--name`, `--no-context-files`, `--no-extensions`, `--no-skills`,
`--no-prompt-templates`, `--no-themes`, `--no-approve`, `--tools`), and
`pi auth --help` still lists `print-api-key`, `print-bearer-token`, and
`check`; no credential-printing/check command was run. A promptless direct
RPC session confirmed success shapes for `get_state`,
`get_available_models`, `get_available_thinking_levels`, `set_model`,
`set_thinking_level`, and `get_last_assistant_text` (empty final text
remained `data:{}`) with exact model and medium thinking confirmation for
`opencodex/command-code/meta-muse-spark-1.2-contributor`; `pi-worker models --json --debug --timeout 30s` succeeded; one real pi-worker Muse medium
implementation task created an isolated worktree, read and edited Go files,
ran tests, returned `outcome:completed` with exact model/thinking
confirmation, usage, declared-writes success, verification success, and
worktree metadata; and one bounded read-only direct RPC prompt using Muse
medium called `read` and replied `OK`, then emitted `agent_settled` with
`get_last_assistant_text` returning `data:{text:"OK"}`. The installed
`rpc-types.d.ts` and `rpc-mode.js` retain the response containers, command
names, `agent_settled`, `message_update`, `assistantMessageEvent`, and
failure response used by pi-worker; direct RPC `bash` still exists upstream
and remains outside pi-worker's outbound allowlist. Pi 0.85.0 changelog
changes relevant to pi-worker are compatible — provider stream
event-sequence fixes, built-in tools now honoring `ctx.cwd`, restored client
compatibility entry point, and RPC abort fix — and require no pi-worker
production adaptation beyond updating compatibility evidence and locking the
repeated-usage-frame observation.
The usage frame vocabulary was re-measured on 2026-09-04 against 0.85.0
with a direct read-only RPC prompt using Muse medium; the
`assistantMessageEvent.type` subtypes directly observed on that prompt were
`toolcall_start`, `toolcall_delta`, `toolcall_end`, `text_start`,
`text_delta`, and `text_end`, with no `done` or `error` subtype forwarded by
the RPC transport and no thinking frames emitted on that run; installed source
retains the broader event vocabulary (`thinking_start`, `thinking_delta`,
`thinking_end`, `text_start`, `text_delta`, `text_end`, `toolcall_start`,
`toolcall_delta`, `toolcall_end`). That direct tool run
repeated the same cumulative usage on `toolcall_start`, `toolcall_delta`,
and `toolcall_end` (input 1283, output 152, cacheRead 0, cacheWrite 0,
reasoning 0, totalTokens 1435), and the following text message emitted zero
usage on `text_start` and `text_delta`, then the final figure on `text_end`
(input 158, output 142, cacheRead 1265, cacheWrite 0, totalTokens 1565)
for a correct run total of input 1441, output 294, cacheRead 1265,
totalTokens 3000; repeated frames within one message must replace, not sum,
and pi-worker's latest-frame-per-message rule remains correct.
A later promptless/no-inference direct RPC probe against the same 0.85.0
binary confirmed idle `steer` success (response returned immediately with no
model inference), `queue_update` and `clear_queue` (which returned the probe
message), `abort` success, and clean exit. `clear_queue` remains outside
Pi Worker's outbound allowlist.

The surface was re-probed on 2026-09-06 against 0.85.1 from `pi --version`
and `pi --help`; the official release (v0.85.1, published 2026-09-05,
13 commits from v0.85.0) adds GPT-6 Astra to the model catalog, fixes
selector/hover/Alt-scroll TUI behavior, uses `prompt_cache_options.ttl: "30m"` for GPT-5.6+ long cache requests, and removes accidental experimental
client/plugin distribution from 0.85.0 — none of which affects supported
stdio RPC source; the official compare confirms no file under
`packages/coding-agent/src/modes/rpc` changed. Installed package exports
remain stable for root and `./rpc-entry`; `./client` and
`./experimental/plugin` are source-only. Both local global installations
(including PATH-selected `/opt/homebrew/bin/pi` and the NVM pi path) report
exactly 0.85.1. `pi --help` retains `--mode rpc`, `--model`, `--session-dir`,
`--name`, `--no-context-files`, `--no-extensions`, `--no-skills`,
`--no-prompt-templates`, `--no-themes`, `--no-approve`, and `--tools`;
built-in names remain `read`, `bash`, `powershell`, `edit`, `write`,
`grep`, `find`, `ls`. `pi auth --help` retains `print-api-key`,
`print-bearer-token`, and `check`; none was run. A promptless/no-inference
direct RPC session confirmed success for `get_state`,
`get_available_models`, `get_available_thinking_levels` (returned
`high`,`max` for Command Code DeepSeek), `set_model` (exact provider/id),
`set_thinking_level high` followed by `get_state` confirmation, and
`get_last_assistant_text` returning `data:{}` for empty history. Idle
`steer` accepted `pi-worker-0.85.1-probe` and emitted `queue_update`;
`clear_queue` returned that exact steering text and emptied the queue;
`abort` returned success; RPC process exited 0. Pi Worker candidate built
with VerifiedVersion 0.85.1 reports `doctor ready:true` and
`Pi version 0.85.1 is supported`; model catalog succeeds. A real Pi Worker
dogfood task on local Pi 0.85.1 used paid
`opencodex/command-code/xiaomi-mimo-v2.5`, thinking off, updated the five
allowed pin surfaces, passed declared writes and post-run piversion
verification, and completed normally. No Pi Worker production adaptation
was needed; GPT-6 Astra appearing in the catalog does not add a Pi Worker
special case.

The surface was re-probed on 2026-09-22 against 0.87.0; 0.86.0 (2026-09-19),
0.86.1 (2026-09-20), and 0.87.0 (2026-09-21) are covered together, and 0.86.x
was never pinned. Both local global installations (including the PATH-selected
`/opt/homebrew/bin/pi` and the NVM `pi` path) report exactly 0.87.0.
`dist/modes/rpc/` is byte-identical between the installed 0.86.0 and 0.87.0
packages. `pi --help` differs from 0.86.0 only by the new `META_API_KEY` line
and retains every flag Pi Worker passes: `--mode rpc`, `--model`,
`--session-dir`, `--name`, `--no-context-files`, `--no-extensions`,
`--no-skills`, `--no-prompt-templates`, `--no-themes`, `--no-approve`, and
`--tools`. Built-in tool names are unchanged. Package exports remain `.` and
`./rpc-entry`; `./client` and `./experimental/plugin` remain source-only.
`pi auth --help` retains `print-api-key`, `print-bearer-token`, and `check`;
none was run. 0.86.0 made the built-in `read`, `bash`, `edit`, and `write`
tools request strict JSON-schema tool calls; 0.87.0 turned that off for
OpenAI-compatible endpoints that do not set `compat.supportsStrictMode`.
Measured on the wire by pointing a `models.json` provider at a local capture
server: 0.86.0 sent `strict: true` for `read`, `edit`, `write`, and `bash`;
0.87.0 sent no `strict` field on any tool. A `bash` tool call killed by a
signal now reports exit code `128 + signal` instead of success (since 0.86.0).
Prompt-cache warming (since 0.86.0) also runs in RPC mode; it is a global Pi
setting with no per-run flag, and its requests are not in Pi Worker's run
usage; tracked as issue #314. The 0.87.0 breaking changes
(`shouldStopAfterTurn` removal, `ContextEditEntry`, `TurnEndEvent` and
`AgentBeforeSettleEvent`, deferred runs from `agent_settled` handlers) are SDK
and extension surfaces; Pi Worker runs `--no-extensions` and ignores
`turn_end`. 0.86.0 also routes direct RPC `steer` and `follow_up` through
extension `input` handlers, a no-op under `--no-extensions`. The 0.86.1
changes (Meta provider login, `/bug`, clipboard, z.ai overflow detection, and
Cerebras strict schemas) are interactive-mode or provider-specific and do not
reach Pi Worker. A promptless/no-inference direct RPC session confirmed
success for `get_state`, `get_available_models`,
`get_available_thinking_levels` (returned `high`,`max` for Command Code
DeepSeek), `set_model` (exact provider/id), `set_thinking_level high` followed
by `get_state` confirmation, and `get_last_assistant_text` returning `data:{}`
for empty history. Idle `steer` accepted `pi-worker-0.87.0-probe` and emitted
`queue_update`; `clear_queue` returned that exact steering text and emptied
the queue; `abort` returned success; the RPC process exited 0. A real
tool-calling Pi Worker task on `local/ornith-1.5-9b` used `read`, `write`,
`bash`, and `edit`, passed its `--verify` check and declared writes, and
completed. This pin change was made by a Pi Worker dogfood task on local Pi
0.87.0 using paid `opencodex/command-code/deepseek-deepseek-v4-flash`,
thinking `high`. A Pi Worker candidate built with VerifiedVersion 0.87.0
reports `Pi version 0.87.0 is supported` and `ready: yes` from `doctor`,
`pi-worker models` lists the catalog, and the same bounded
`local/ornith-1.5-9b` task completed with its `--verify` check passing and no
undeclared writes. No Pi Worker production adaptation was needed.

The surface was re-probed on 2026-09-30 against 0.99.1; 0.87.1 (2026-09-22),
0.99.0 (2026-09-29), and 0.99.1 (2026-09-29) are covered together, and 0.87.1
and 0.99.0 were never pinned. Both local global installations
(`/opt/homebrew/bin/pi` and the NVM `pi` path) report exactly 0.99.1.
`pi --help` differs from 0.87.0 only by a new `pi mcp <command>` line, `mcp` in
the list of commands with their own help, `--extension` also accepting
`builtin:<name>`, and `--no-extensions` now described as disabling "extension
discovery and built-in extensions". It retains every flag Pi Worker passes:
`--mode rpc`, `--model`, `--session-dir`, `--name`, `--no-context-files`,
`--no-extensions`, `--no-skills`, `--no-prompt-templates`, `--no-themes`,
`--no-approve`, and `--tools`. Built-in tool names are unchanged. Package
exports remain `.` and `./rpc-entry`; `./client` and `./experimental/plugin`
remain source-only. In `dist/modes/rpc/rpc-types.d.ts` the only change is that
successful `prompt`, `steer`, and `follow_up` responses now carry
`data:{disposition}`: `started`, `queued`, or `handled` for `prompt`, and
`queued` or `handled` for `steer` and `follow_up`. Pi Worker sends `prompt` and
`steer` but never `follow_up`, and reads only `success` from those two
responses, so the added data is ignored. The `handled` prompt path (extension
commands and extension `input` handlers) already existed in 0.87.0 and is not
reachable under `--no-extensions`. 0.99.0 ships `mcp`, `codemode`,
`tool-search`, and `llama.cpp` as built-in extensions, and `--no-extensions`
now disables them too. Measured with a temporary `PI_CODING_AGENT_DIR` whose
`mcp.json` defines a stdio server that creates a marker file: an RPC session
without `--no-extensions` started the server (marker created), and a session
with Pi Worker's exact launch flags did not (marker absent).
`pi-worker models --json` returned the identical selector set on 0.87.0 and
0.99.1 (134 selectors each); the `local` provider comes from `models.json`, not
from the built-in llama.cpp extension, and is unaffected. A
promptless/no-inference direct RPC session with Pi Worker's launch flags
confirmed success for `get_state`, `set_model` (exact provider/id),
`get_available_thinking_levels`, `set_thinking_level high` followed by
`get_state` confirmation, and `get_last_assistant_text` returning `data:{}` for
empty history. For Command Code DeepSeek the levels were `low`, `medium`,
`high`, `xhigh`, `max`; 0.87.0 returned the same set on the same day, so the
difference from the earlier `high`,`max` record comes from the model catalog,
not from Pi. Idle `steer` accepted `pi-worker-0.99.1-probe`, answered
`data:{disposition:"queued"}` and emitted `queue_update`; `clear_queue`
returned that exact steering text and emptied the queue; `abort` returned
success; the RPC process exited 0. `pi-worker doctor` on 0.99.1 before the pin
moved reported the version as an unverified warning with `ready: yes`. File
hashes of the Pi agent directory (sessions and package caches excluded) were
unchanged by the doctor, catalog, and probe runs. The remaining 0.99.x changes do not reach Pi
Worker: codemode's structured `bash` results apply only to codemode scripts;
the `RpcClient` listener fix is in the TypeScript client, which Pi Worker does
not use; `builtin:<name>` naming appears in RPC source info, which Pi Worker
does not read; session files are now created at the first user message, and Pi
Worker does not read session files. This pin change was made by a Pi Worker
dogfood task on local Pi 0.99.1 using
`opencodex/command-code/deepseek-deepseek-v4-flash`, thinking `high`. No Pi
Worker production adaptation was needed.

The surface was re-probed on 2026-10-02 against 1.0.0; 0.99.2 (2026-09-30) and
1.0.0 (2026-10-01) are covered together, and 0.99.2 was never pinned. Both
local global installations (`/opt/homebrew/bin/pi` and the NVM `pi` path)
report exactly 1.0.0. `pi --help` differs from 0.99.1 only in two descriptions:
`--provider` now reads "Provider to search for --model (requires --model)"
(1.0.0 fails `--provider` without `--model` instead of ignoring it), and
`--tui-mode` now defaults to `fullscreen`. Pi Worker passes neither flag. Every
launch flag Pi Worker passes is retained. Built-in tool names are unchanged.
Package exports are identical. `dist/modes/rpc/` is byte-identical between
0.99.1 and 1.0.0, and so is the bundled
`@earendil-works/pi-ai/dist/types.d.ts`. Dependencies changed only from
`^0.99.1` to `^1.0.0`. The bundled `RETRYABLE_PROVIDER_ERROR_PATTERN` list is
byte-identical to 0.99.1, so the issue #439 reading still holds on 1.0.0.
`pi-worker models --json` returned the identical selector set on 0.99.1 and
1.0.0 on the same day (99 selectors each). The drop from the 134 recorded for
0.99.1 comes from the provider catalog, not from Pi. A promptless/no-inference
direct RPC session with Pi Worker's launch flags confirmed success for
`get_state`, `set_model` (exact provider/id, switching from
`opencodex/opencode-go/deepseek-v4.1-flash` to
`opencodex/opencode-go/muse-spark-1.2-contributor`, confirmed by `get_state`),
`get_available_thinking_levels` (`low`, `medium`, `high`, `xhigh` for that
model), `set_thinking_level high` followed by `get_state` confirmation, and
`get_last_assistant_text` returning `data:{}` for empty history. Idle `steer`
accepted `pi-worker-1.0.0-probe`, answered `data:{disposition:"queued"}` and
emitted `queue_update`; `clear_queue` returned that exact steering text and
emptied the queue; `abort` returned success; the RPC process exited 0. The same
probe on 0.99.1 the same day gave identical responses. MCP isolation was
re-measured with a temporary `PI_CODING_AGENT_DIR` whose `mcp.json` defines a
stdio server that creates a marker file. Since 0.99.2 servers without `direct`
exposure connect in the background instead of before the first prompt, so the
server used `"exposure": "direct"`. An RPC session without `--no-extensions`
started the server (marker created), and a session with Pi Worker's exact
launch flags did not (marker absent). `pi-worker doctor` on 1.0.0 before the
pin moved reported the version as an unverified warning with `ready: yes`. The
remaining 0.99.2 and 1.0.0 changes do not reach Pi Worker: fullscreen TUI,
codemode prompt and error changes, codemode image generation, Radius and
Anthropic login methods, MCP OAuth changes, MCP lazy connection and tool
renaming, and `quietStartup` are interactive, codemode, or MCP surfaces, and Pi
Worker runs `--no-extensions`. 0.99.2 also changed provider retries after an
unparseable `Retry-After` date from immediate to exponential backoff.
`@earendil-works/pi-durable`, published alongside 1.0.0, is not a dependency of
`@earendil-works/pi-coding-agent` and does not change `--mode rpc`. This pin
change was made by a Pi Worker dogfood task on local Pi 1.0.0 using
`opencodex/opencode-go/deepseek-v4.1-flash`, thinking `high`. No Pi Worker
production adaptation was needed.

The surface was re-probed on 2026-10-07 against 1.0.4; 1.0.1 (2026-10-03),
1.0.2 (2026-10-04), 1.0.3 (2026-10-05), and 1.0.4 (2026-10-05) are covered
together, and 1.0.1 through 1.0.3 were never pinned. Both local global
installations (`/opt/homebrew/bin/pi` and the NVM `pi` path) report exactly
1.0.4. They were installed with `npm i -g
@earendil-works/pi-coding-agent@1.0.4`, not `pi update`, which since 1.0.1
recommends the pi.dev managed installation for global npm installations. Since
1.0.1 the published package ships no `npm-shrinkwrap.json`, so npm resolves the
`^1.0.4` `@earendil-works/*` dependencies at install time. Both installations
resolved `chord`, `pi-agent-core`, `pi-ai`, `pi-codemode`, `pi-mcp`,
`pi-telemetry`, and `pi-tui` to 1.0.4. The `pi` executable is
`dist/bundle/cli.js`, which carries its own copy of the `pi-agent-core` and
`pi-ai` code; the installed `@earendil-works/*` dependency copies load only for
extensions, which Pi Worker disables with `--no-extensions`. On 2026-10-08 a
copy of the 1.0.4 installation with every dependency removed gave the same
promptless RPC probe output and passed `npm run check:livepiprobe` 2/2, so
`pi --version` identifies the Pi code that Pi Worker runs. This depends on how
Pi packages its build, so the removal check is repeated at each pin.
`pi --help` differs from 1.0.0 only in `--tools` and
`--exclude-tools`, which now accept `*` patterns, and a new `--no-mcp`. The
`--tools` description now reads "Keeps MCP tools unless an entry starts with
mcp__". Every launch flag Pi Worker passes is retained, and built-in tool names
are unchanged. A `--tools` entry without `*` still matches by exact name
(`createToolNameMatcher` in `dist/core/mcp-servers.js`). Pi Worker's allowlists
contain no `*`, no `mcp__` entry, and no `tool_search`, so 1.0.4 would not
declare an unmatched MCP tool to the model even if MCP loaded. Package exports
and `bin` are identical, `dist/modes/rpc/` is byte-identical to 1.0.0, and the
installed `@earendil-works/pi-agent-core` `dist/` is identical apart from
source maps. These file comparisons read the installed, unbundled copies; the
probes below run the bundle. The installed
`@earendil-works/pi-ai/dist/types.d.ts` changed only in the
`KnownProvider` rename of `azure-openai-responses` to `azure`, a
`SamplingParams` alias, a new optional `samplingParamsByThinkingLevel`, and one
doc comment; the `Model` `provider` and `id` fields Pi Worker projects are
unchanged. The new dependency `@earendil-works/pi-telemetry` has no `fetch`
call or `https://` URL in its `dist/`. The
`RETRYABLE_PROVIDER_ERROR_PATTERN` list, present in both the installed `pi-ai`
and the `pi` bundle, gained `model is at capacity` and
`pending stream has been canceled`. `isRetryableAssistantError` still returns
false for `Upstream incomplete: missing_terminal_event` and `upstream stream
ended early (missing_terminal_event)` on both 1.0.0 and 1.0.4, so the issue
#439 reading still holds. `pi-worker models --json` returned the identical
selector set on 1.0.0 and 1.0.4 on the same day (100 selectors each). The
promptless/no-inference RPC probe described for 1.0.0, now switching to
`opencodex/opencode-go/muse-spark-1.3-contributor` and steering
`pi-worker-1.0.4-probe`, gave responses identical to the same probe on 1.0.0
the same day, and the RPC process exited 0. MCP isolation was re-measured with
the 1.0.0 recipe on both installations: the server started without
`--no-extensions` and did not start with Pi Worker's exact launch flags. MCP is
the built-in extension `builtin:mcp`; built-in extensions come from settings
resolution, and `--no-extensions` keeps only CLI extension paths. `pi-worker
doctor` on 1.0.4 before the pin moved reported the version as an unverified
warning with `ready: yes`. The remaining 1.0.1 through 1.0.4 changes do not
reach Pi Worker: MCP project overrides, MCP OAuth and client registration, tool
renderers, Cloudflare classifiers, the Nix flake, codemode image handling, the
Azure provider rename and Foundry deployments, `samplingParamsByThinkingLevel`,
owner-only output-file permissions, and editor key changes are interactive,
codemode, MCP, or provider-configuration surfaces. This pin change was made by
a Pi Worker dogfood task on local Pi 1.0.4 using
`opencodex/opencode-go/deepseek-v4.1-flash`, thinking `high`. No Pi Worker
production adaptation was needed.

## Compatibility gate

**Gate result: pass for Pi 1.0.4.** The expected `--mode rpc` surface and all
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

**Bundle check.** Every pin repeats this check, because the gate relies on
`pi --version` identifying the Pi code that Pi Worker runs. Install the
candidate release into an empty scratch directory with `npm i --ignore-scripts
@earendil-works/pi-coding-agent@<version>`. Delete every package in that
directory's `node_modules` except `@earendil-works/pi-coding-agent`. Confirm
that no parent directory has a `node_modules` directory and that `NODE_PATH`
is unset. Put a `pi` link to that copy's `dist/bundle/cli.js` first on
`PATH`. Then run the promptless RPC probe and `npm run check:livepiprobe`.
The probe output must match the unmodified installation, and the live
probe must pass. If either fails, Pi loads its installed dependencies at
run time. Then `pi --version` no longer identifies the running code, and
the pin must not move until Pi Worker also verifies those dependency
versions.

## Process invocations

Use this harmless probe launch for an isolated, read-only process. It
intentionally does not send a prompt and does not persist a session:

```sh
pi --mode rpc --offline --no-session --no-context-files --no-extensions --no-skills --no-prompt-templates --no-themes --no-approve --tools read,grep,find,ls
```

Use this writable worker launch with the current working directory as the
workspace. Pi-worker creates a fresh private session directory per worker
before launch — the run's `worker-<n>/` transcript directory, or with
`--no-transcript` a new OS temporary directory
(`os.MkdirTemp("", "pi-worker-v0-*")`) — and currently runs every worker with `--no-approve` and this tool allowlist:

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
each target, which narrows (but does not close) the window in which a reused
pid could be signalled: a target that exits and whose pid is reused between
the check and the kill can still receive the signal. This is lifecycle
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

The `steer` and `abort` shapes above are upstream observations now used only by
Pi Worker's typed internal control methods during a supervised model turn;
caller-supplied raw request shapes are never forwarded.

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
an error: <errorMessage>`, carrying the message's `errorMessage` verbatim;
when it is absent the worker reports `upstream/model turn ended with an error`
alone. Any text emitted by that failed turn is partial evidence, not a final
explanation. A settled assistant message with another stop reason and empty
text retains the generic empty-answer wording.

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
`@earendil-works/pi-coding-agent@1.0.4/node_modules/@earendil-works/pi-ai/dist/types.d.ts`;
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

Pi 1.0.4 observes the `get_available_thinking_levels` success container as
`data:{levels: ThinkingLevel[]}`, where the levels are the active model's
supported subset of the recognized seven levels (`off`, `minimal`, `low`,
`medium`, `high`, `xhigh`, `max`), not always all seven. V0 requires a non-null
array of unique, recognized strings (a non-null unique subset of recognized
levels). A well-formed `set_thinking_level success:false` is the
only setter rejection that worker policy may recover from; transport and
malformed responses remain failures.

Pi 1.0.4 observes the `get_state` success container exactly as
`{type:"response", command:"get_state", success:true, data:RpcSessionState}`.
V0 projects only `model.provider`, `model.id`, and `thinkingLevel`. All are
required after model activation; the model must equal the selected catalog
entry and thinking must be one recognized value. The full version-pinned
upstream declaration is in
`@earendil-works/pi-coding-agent@1.0.4/dist/modes/rpc/rpc-types.d.ts`; V0
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
| `steer` | `type`, `message: string` (non-empty) |
| `abort` | `type` |
| `get_last_assistant_text` | `type` |

Pi-worker must reject every other RPC type. In particular,
it must reject direct RPC `bash`: Pi 1.0.4 dispatches that command directly,
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
the delta semantics in the other direction: Pi 0.85.0 can repeat the same
cumulative usage on `toolcall_start`, `toolcall_delta`, and `toolcall_end`,
and the measured text message emitted zero usage on `text_start` and
`text_delta`, then the final figure on `text_end`; each frame that carries
usage carries the message's cumulative usage so far, so a message may report
more than one such frame with the same figure. No `message_update` frame
type terminates the measurement; pi-worker reads the latest usage a message
reported, bounded by `message_start` and `message_end`, and that
latest-frame-per-message rule remains correct — repeated frames within one
message must replace, not sum. Assistant `stopReason` can be
`stop`, `length`, `toolUse`, `error`, or `aborted`; it is not the session
terminal condition. A latest assistant `message_end` with
`stopReason: "error"` makes the worker fail with `upstream/model turn ended
with an error: <errorMessage>`, appending the assistant message's
`errorMessage` verbatim when it is present; an error stop with no
`errorMessage` yields the base sentence alone. This wording does not claim
that no text existed and does not attribute the error to a particular
provider mechanism. Text emitted by that failed turn is exposed only as
`partialExplanation`, never as `explanation`.
Partial assistant text retained for this field is capped at `MaxFrameBytes`
(8 MiB) UTF-8 bytes across the in-flight and most-recent message buffers;
when the cap is reached, older text is evicted without splitting a UTF-8 rune.
A newer valid assistant message supersedes the prior classification; user and
tool-result messages do not. A missing or malformed stopReason on the latest
assistant message does not inherit an earlier error. The assistant's
`errorMessage` is projected verbatim only in the worker `error` field, never
into the fixed continuation prompt, the warning, or the debug stream.

## Tool semantics

Built-in tool names reported by `pi --help` as of Pi 1.0.4 are `read`,
`bash`, `edit`, `write`, `grep`, `find`, `ls`, and `powershell`. Pi-worker
intentionally continues enabling only its established seven
(`read,grep,find,ls,edit,write,bash`) and does not enable `powershell` in
this update. In v0, `bash` is always enabled and can run arbitrary shell
commands with the user's host permissions. `--tools` is an allowlist of
capabilities, not a sandbox.

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
