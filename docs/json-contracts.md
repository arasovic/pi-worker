# JSON contracts

Pi Worker publishes eight versioned JSON documents. This page is the v1
contract for agents and other machine consumers.

## Shared rules

- Every complete document is one JSON object followed by one LF byte.
- `schemaVersion` is required and is the integer `1`.
- Required arrays are always arrays, including when empty; they are never
  `null` or omitted.
- An unsupported `schemaVersion` must be rejected.
- Removing a field, changing a field type or enum meaning, or adding a new
  required field requires a new schema version. Additive optional fields may
  remain in v1, so external consumers should ignore unknown fields after
  validating the supported schema version.
- Durable input documents are stricter: config and skill receipts reject
  unknown fields and trailing data. The npm launcher also strictly validates
  the native `skill status` document shipped in the same package before adding
  live external inspection.
- Usage and early setup failures may write no JSON. Diagnostics and `--debug`
  output go to stderr and never become a second stdout document.

## `version --json`

All fields are required strings except `schemaVersion`:

```json
{"schemaVersion":1,"version":"dev","commit":"unknown","buildDate":"unknown"}
```

Release builds replace the three source-build sentinel values.

## `models --json`

Required fields:

- root: `schemaVersion`, `models`
- model: `provider`, `id`, `selector`

`models` is sorted by provider then ID. Every `selector` is the exact
`provider/id` form accepted by `run` and `config set`.

## `doctor --json`

Required fields:

- root: `schemaVersion`, `ready`, `checks`
- check: `name`, `status`, `message`

`ready` is a boolean. Check `status` is `ok`, `warning`, or `failed`. The five
check names and their order are `pi-executable`, `pi-version`, `config`,
`model-catalog`, and `default-model`.

A completed inspection emits the document even when readiness exit code 3 is
returned. A timed-out, cancelled, or internally aborted inspection emits no
partial document.

## `config show --json`

The config document is both public output and durable input:

```json
{"schemaVersion":1,"defaultModel":"provider/model"}
```

Both fields are required. `defaultModel` may be an empty string; otherwise it
must be one exact provider/model selector. Loading rejects unknown fields,
trailing data, invalid selectors, and unsupported schema versions.

## `skill receipt-path --json`

Required fields are `schemaVersion` and the absolute string `receiptPath`.

## `skill status --json`

Required root fields:

- `schemaVersion`
- `receiptPath`
- `status`
- `verifiedTargets`
- `trackedTargets`
- `affectedTargets`
- `recovery`
- `externalInspection`

`status` is `verified`, `missing`, `blocked`, `drifted`, `skipped`, or
`failed`. `verifiedTargets`, `trackedTargets`, and `recovery` are string
arrays. Each affected target has exactly `path`, `state`, and `recovery`;
`state` is `unmanaged`, `drifted`, or `conflicting`.

`externalInspection` always has `state` and `targets`. Its state is
`performed` or `unavailable`. `unavailable` always carries an empty target
array. Every performed target has `path` and an identity of `current`,
`legacy`, `unknown`, or `none`.

The standalone native binary reports external inspection as unavailable. The
npm launcher reports performed only after every resolved global target was
inspected; any resolver or target-read failure discards partial findings and
reports unavailable. External findings are informational and never change the
native status exit code.

## `run --json`

Required root fields are `schemaVersion`, `status`, and non-null `workers`.
Root status is `completed`, `partial`, `failed`, `timed-out`, or `cancelled`.
Workers remain in request order even when concurrent completion order differs.

Every worker requires:

- `model`: the requested exact selector
- `status`: `completed`, `failed`, `timed-out`, `cancelled`, `unavailable`, or
  `error`

Worker fields are conditionally present:

- `requestedThinkingLevel`: present for an explicit request
- `thinkingLevel`: present after Pi reports an effective level
- `thinkingFallback`: present and `true` only for a reported fallback
- `warning`: present with a fallback or other worker warning
- `explanation`: present when final assistant text exists
- `error`: present when the worker reports an error

Verification is additive and optional, so `schemaVersion` stays `1`. Root
`verification` appears only when `--verify` ran on a completed run; a run
without the flag, or one that did not complete, carries none:

- `argv`: the check command split into argv (always present)
- `exitCode`: the process exit code (always present); a passing check
  carries no other field
- `output`: present only for a failing check; the captured stdout and
  stderr in order, reduced to its first 2 KiB and last 6 KiB with the
  elided middle marked when `truncated`
- `truncated`: present and `true` only when the capture exceeded the
  excerpt budget
- `logFile`: present only when a truncated capture was also written in
  full to a `pi-worker-verify-*.log` file in the system temp directory

Git state is additive and optional, so `schemaVersion` stays `1`. Root
`git` appears only when a run moved the workspace's HEAD, its branch,
or a stash entry appeared or disappeared between the start and the end
of the run; a modified working tree alone (only `dirty` differing) does
not produce it. When present it carries `before`, `after`, and
optional `stash`; `before` and `after` each with:

- `head`: always present; the empty string when the branch is unborn
- `branch`: present only when HEAD is attached to a branch (a detached
  HEAD omits it)
- `dirty`: always present; true when the working tree has uncommitted
  changes
- `stashes`: always present; the number of stash entries

`stash` is present only when a stash entry appeared or disappeared
between the start and the end of the run. It carries `added` and
`removed`, each optional and omitted when empty, as arrays of
`"<sha> <subject>"` strings in `git stash list` order (newest first).
Entries are compared by identity, so a `stash@{N}` index shift is not
a change.

The change manifest is additive and optional, so `schemaVersion` stays `1`.
Root `changes` is present when the workspace is inside a git work tree and the
inspection of it succeeded. Only a workspace outside a git work tree, and an
environment with no `git` at all, reached with a live context at the
inspection, still carry no `changes` field — the same silent no-op `git`
makes; a dead context never reaches the guard, so the same directory carries
the `context already done` omission instead. Unlike `git` it is not gated by a
state change: a run that only left modified files behind still carries it,
because those files are what it names. It carries either a reason it could
not be measured or the measurement, never both:

- `omitted`: present only when the manifest could not be measured; one of
  `dirty before-state`, `unborn head`, `context already done`, or
  `measurement failed`. `files`, `totalFiles`, and `truncated` carry no
  meaning when it is present
- `totalFiles`: always present; the true number of changed paths, before the
  entry cap. A measured run that changed nothing carries `0` rather than
  omitting the field
- `files`: present only when at least one path changed; capped at 100
  entries
- `truncated`: present and `true` only when the cap dropped entries

Each entry in `files` carries:

- `path`: always present; the workspace-relative path
- `status`: always present; exactly one of `added`, `modified`, `deleted`
- `added` and `deleted`: always present; the line counts, both `0` for a
  binary file
- `binary`: present and `true` only when git reported the file as binary

The manifest is measured against the git state recorded before the first
worker started, so a run that committed its own work still lists the files
it changed.

The manifest covers the paths `git` tracks or would track. Ignore rules
exclude untracked paths only: an ignored path is outside both the manifest
and the write check when it is untracked — it cannot appear in `files`, it
does not count toward `totalFiles` or `undeclaredCount`, and a run that
wrote only ignored untracked paths reports a clean write verdict. A tracked
path is measured, and therefore checked, whether or not a rule matches it.

The write check is additive and optional, so `schemaVersion` stays `1`.
Root `writes` is present exactly when the request carried a write
declaration: a caller who declared always gets an answer — a verdict or a
stated skip reason — and a caller who never declared gets no field at all.
Silence is never a clean check. The check is run-level, not task-level:
which task wrote a given path is not knowable from a shared workspace, so
the undeclared set belongs to the run. It carries either a reason it could
not run or the verdict, never both:

- `skipped`: present only when the check could not run; a short reason,
  `not all tasks declared writes` — some task said nothing at all, the
  only state that triggers it, since a task that declared an empty set
  has declared — or `change manifest unavailable`.
  `undeclaredCount`, `undeclared`, and `truncated` carry no meaning when it
  is present
- `undeclaredCount`: always present on a verdict; the true number of
  changed paths no task declared, before the entry cap. A checked run that
  wrote nothing undeclared carries `0` rather than omitting the field
- `undeclared`: present only when at least one path was undeclared; capped
  at 100 entries
- `truncated`: present and `true` only when the cap dropped entries

Thinking values are `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or
`max`.

## Durable skill receipt

The receipt requires these root fields:

- `schemaVersion`
- `installerVersion`
- `skillsVersion`
- `outcome`
- `targets`
- `affectedTargets`
- `recovery`

Outcome is `installed`, `blocked`, `skipped`, or `failed`. Every target has
`path`, `kind`, and non-empty `files`; kind is `canonical`, `symlink`, or
`copy`. Every file has a normalized relative POSIX `path` and 64-digit hex
`sha256`. Affected targets use the same shape and enum documented for skill
status. All three root collections and every affected target's recovery array
are non-null.

Both the npm writer and native reader validate the exact receipt field set,
enum values, path forms, hashes, array invariants, and schema version. The
native reader additionally rejects trailing JSON.
