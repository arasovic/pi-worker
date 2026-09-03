# JSON contracts

Pi Worker publishes eight versioned JSON documents. This page is the v1
contract for agents and other machine consumers.

## Shared rules

- Every complete document is one JSON object followed by one LF byte.
- `schemaVersion` is required and is the integer `1`.
- Required arrays are always arrays, including when empty; they are never
  `null` or omitted.
- An unsupported `schemaVersion` must be rejected.
- A consumer must ignore fields it does not recognise on a
  `schemaVersion` it supports.
- Removing a field, changing a field type or enum meaning, or making a
  required field optional requires a new schema version everywhere:
  each takes away a guarantee a consumer relied on. A new required
  field adds a guarantee instead, so it may stay in v1 while both
  skews are covered: a reader older than the writer is safe when
  it ignores unknown fields or ships with the writer and can never
  be out of step, and a file older than the reader is safe only
  when the document does not persist, because an older file lacks
  the new field. The run document never persists and its readers
  validate `schemaVersion` and ignore unknown fields; the npm
  launcher rejects unknown fields on `skill status` but reads the
  document from a binary shipped in the same package, so the two
  are always in step. Config and skill receipts persist and their
  readers reject unknown fields, so a new required field needs a
  new version there.
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

`ready` is a boolean. Check `status` is `ok`, `warning`, or `failed`. The six
check names and their order are `pi-executable`, `pi-version`, `config`,
`model-catalog`, `default-model`, and `workspace`. The `workspace` check is
advisory: it reports `ok` or `warning`, never `failed`, and a warning never
makes `ready` false.

A completed inspection emits the document even when readiness exit code 3 is
returned. A timed-out, cancelled, or internally aborted inspection emits no
partial document.

## `config show --json`

The config document is both public output and durable input:

```json
{"schemaVersion":1,"defaultModel":"provider/model"}
```

On input, `schemaVersion` is required and `defaultModel` is optional; a
missing `defaultModel` key means the same thing as an empty string. On
output both keys always appear. `defaultModel` may be an empty string;
otherwise it must be one exact provider/model selector. Loading rejects
unknown fields, trailing data, invalid selectors, and unsupported schema
versions.

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

`status` is `verified`, `missing`, `blocked`, `drifted`, `skipped`, `failed`, or
`stale`. `verifiedTargets`, `trackedTargets`, and `recovery` are string
arrays. Each affected target has exactly `path`, `state`, and `recovery`;
`state` is `unmanaged`, `drifted`, or `conflicting`.

The optional root fields `installerVersion` and `programVersion`, when present,
are strings. `installerVersion` is the version recorded when the install ran;
`programVersion` is the version of the binary producing the document. Both are
reported without a leading `v` and are omitted when their value is empty, so a
missing receipt omits both fields. A valid receipt reports `installerVersion`,
and the running program reports `programVersion` (including `dev` for a source
build); the fields are not limited to `status: "stale"`. `stale` means every
managed file still matches the receipt but the two versions differ, ignoring a
single leading `v` on either side; a source build with `programVersion: "dev"`
never reports `stale`. Like the other non-verified statuses, `stale` exits `3`.

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

Required root fields are `schemaVersion`, `status`, `outcome`, and non-null
`workers`. Root status is `completed`, `partial`, `failed`, `timed-out`, or
`cancelled`. Workers remain in request order even when concurrent completion
order differs.

`outcome` is a new required field, and the run document is safe on
both versioning skews — it never persists and its readers ignore
unknown fields — so `schemaVersion` stays `1`. Within an emitted
run document, root `outcome` is always present, as is `changes` (the
CLI always configures the git inspector): unlike `writes` and `git`,
the two have no absent form, so they are never read by
presence. Its value is the same decision as the exit code, in words,
from one place in the code:

| `outcome` | exit code |
| --- | --- |
| `completed` | `0` |
| `workers-unavailable` | `3` |
| `undeclared-writes` | `4` |
| `task-failed` | `5` |
| `partial` | `5` |
| `verification-failed` | `6` |
| `timeout` | `7` |
| `cancelled` | `8` |
| `internal-error` | `9` |

Exit `5` joins two words: `task-failed` for a run whose status is
`failed`, and `partial` for one whose status is `partial`. The `usage`
word has no row because a usage error fails before a run exists and it
never reaches a document.

`completed` means the run finished and no check contradicted it. It does
not mean every check ran: a skipped write check leaves `outcome` at
`completed`, and `writes.skipped` is where a caller learns the question
went unanswered.

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
- `partialExplanation`: present when the run ended without a final
  assistant text but assistant text was already observed; carries the
  text as it streamed, and never appears together with `explanation`
- `error`: present when the worker reports an error
- `data`: present only when the task carried `--data` files; one entry
  per carried file, each with `path`, `byteCount`, and `sha256`
- `usage`: present only when at least one assistant message reported a
  non-zero usage figure; the token counts and dollar figures Pi
  computed, passed through unchanged

Partial text reporting is additive and optional, so `schemaVersion`
stays `1`. Worker `partialExplanation` appears only when a run ended
without a final text — timed out, cancelled, or failed before
settlement — and the last assistant message had already streamed text;
it carries exactly the `text_delta` content observed, in stream order,
and nothing more. It is never present together with `explanation`: a
consumer reading `explanation` can always assume the model finished, so
truncated text under that name can never turn an interrupted run into a
complete-looking answer.

Carried material is additive and optional, so `schemaVersion` stays `1`.
Per worker, `data` appears only when the task carried `--data` files, in
the same order the files were declared:

- `path`: always present; the path as composed into the prompt as the
  section label
- `byteCount`: always present; the length of the content actually read
  and composed
- `sha256`: always present; the SHA-256 of the content as read, lowercase
  hex, matching what a checksum of the file on disk produces

Content is never reported: the document carries the path, the byte
count, and the hash, nothing more.

Usage reporting is additive and optional, so `schemaVersion` stays `1`.
Worker `usage` appears only when at least one assistant message reported
a non-zero usage figure; the numbers are Pi's own, copied unchanged —
`cost` is in US dollars as Pi computed it, and pi-worker derives no
price of its own. The field is present when a message reported a
non-zero figure, and absent otherwise — including when the provider
reported nothing but zeros: a completed run cannot genuinely consume
zero tokens, so an all-zero report is no measurement. `cacheWrite1h`
and `reasoning` are present only when some message reported them.

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
The `omitted` enum is not byte-identical to 0.3.1's: `dirty before-state`
retired — 0.4.0 measures a dirty tree instead of omitting it — and `work
tree not confirmed` arrived. The retirement takes away no guarantee: no
field was removed, no type changed, and no value a 0.4.0 document still
emits means anything different than it did — a consumer branching on the
retired reason finds that branch unreachable, not misread — while a bump
to `2` would make every 0.3.1 consumer reject all output, a total break
to signal a change that is not one.
Root `changes` never vanishes from real output: the CLI always configures
the git inspector, and with one configured the field always carries a
value. Only a
controller built without the git inspector omits the field entirely. Unlike
`git` it is not gated by a state change: a run that only left modified
files behind still carries it, because those files are what it names. It
carries either a reason it could not be measured or the measurement, never
both:

- `omitted`: present only when the manifest could not be measured; one of
  `unborn head`, `context already done`, `measurement failed`, or
  `work tree not confirmed`. `files`, `totalFiles`, and `truncated` carry
  no meaning when it is present
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
- `dirtyBefore`: present and `true` only when the path was already dirty
  before the run started; the line counts are measured against the last
  commit rather than against the pre-run content, so they include work
  that was already there and the run's share cannot be separated out
- `noFinalNewline`: present and `true` only when the file's last byte is
  not a newline; it is descriptive, not a verdict — it makes no claim
  that this is wrong and no claim about who did it. Together with
  `status: "added"` it means the run produced the file that way;
  together with `status: "modified"` it is ambiguous, because the file
  may have been like that before the run. It is an attribute of listed
  files, so a path the cap dropped carries no field. An absent field
  means either that the file ends with a newline or that it could not
  be examined; the field asserts something only when present

`work tree not confirmed` is the reason the inspector could not confirm a
work tree: the directory is not a git work tree, git is missing entirely,
or the guard failed for a transient reason. The code cannot tell the three
apart, so the reason names only what is known and claims none of them — it
does not say which cause it is, because the code does not know. It is
distinct from `measurement failed`, which covers the other failure position:
a guard that passed and a later command that failed.

The manifest is measured against the git state recorded before the first
worker started, so a run that committed its own work still lists the files
it changed. Because the measurement compares the workspace before the run
against the workspace after it, it cannot tell the run's writes from
anyone else's: a file an editor or a watcher saves while the run is in
flight appears as a change the run made, and with `--writes` declared the
check reports it undeclared and the run exits `4`. While a run is in
flight, keep one run at a time per workspace and leave the workspace
alone. Paths already dirty when the run started are stamped up front
with size, modification time, and the executable bit — the one mode bit
git tracks, so a chmod between two non-executable modes does not register
as a change — and the ones whose stamp never moved are subtracted from the
result: they were equally dirty before the run and name no change it made.
That subtraction accepts one false negative, and it is deliberate rather
than an oversight: on a coarse-granularity filesystem — FAT, exFAT, some
NFS mounts, older ext3 — a restore that lands within the same tick as
the pre-run stamp leaves size and modification time unchanged, so the
path is absent from the manifest even though the run wrote it, which is
defensible because net change is zero. On the sub-second-resolution
filesystems that are the normal case, the write moves the modification
time, the stamp does not match, and the path stays.

The manifest covers the paths `git` tracks or would track. Ignore rules
exclude untracked paths only: an ignored path is outside both the manifest
and the write check when it is untracked — it cannot appear in `files`, it
does not count toward `totalFiles` or `undeclaredCount`, and a run that
wrote only ignored untracked paths reports a clean write verdict. A tracked
path is measured, and therefore checked, whether or not a rule matches it.
Submodules are the other exclusion: every path-computing diff ignores them,
so a dirty submodule is never a changed path and can never be reported as
an undeclared write — the manifest measures this workspace's files, and a
submodule's contents are another repository's business.

The write check is additive and optional, so `schemaVersion` stays `1`.
Root `writes` is present exactly when the request carried a write
declaration: a caller who declared always gets an answer — a verdict or a
stated skip reason — and a caller who never declared gets no field at all.
Silence is never a clean check. The check is run-level, not task-level:
which task wrote a given path is not knowable from a shared workspace, so
the undeclared set belongs to the run. The same limit holds one level up:
a concurrent writer — another run, an editor, a build — lands in whichever
run is measuring, so while any run declares `--writes`, that workspace must
have one run at a time. Root `writes` carries either a reason it could not run
or the verdict, never both:

- `skipped`: present only when the check could not run, with the single
  reason `change manifest unavailable`, reached by the four manifest
  omissions above and by an absent manifest, which carries no `omitted`
  field to consult (the run had no git inspector): a dirty before-state
  is measured now, so it never triggers the skip. A declaration where
  some tasks declare and others do not is rejected before the run as a
  usage error, never reported as a skip. `undeclaredCount`, `undeclared`,
  and `truncated` carry no meaning when `skipped` is present
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
