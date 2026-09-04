# JSON contracts

Pi Worker publishes ten versioned JSON documents. This page is the v1
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

Loading also distinguishes an absent file from a dangling link: a missing
`config.json` is a valid empty configuration (exit `0` for `config show`,
`run` reading a default, `doctor` warning), while a `config.json` that is
itself a symbolic link whose target does not exist fails every read path
with a clear dangling-link error and leaves the link untouched. Only the
final component produces that error: a dangling link in a parent directory
is an ordinary missing file, and a link whose target exists resolves like
any other document.

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

## `runs list --json`

The root object has exactly `schemaVersion` and `runs`. `runs` is always an
array, ordered newest first. Each entry has exactly these fields:

- `runId`: the record filename without `.jsonl`
- `startedAt`: the start timestamp, or `""` when the start fields are unreadable
- `workspace`: the recorded workspace, or `""` when unreadable
- `tasks`: the number of recorded tasks, or `0` when unreadable
- `outcome`: the recorded outcome, `error`, `running`, `interrupted`, or
  `unknown`
- `path`: the record path

No root or entry field is omitted. Missing or unreadable display fields use
their zero values. A record with no usable start line remains an entry with
`outcome: "unknown"` and its filename-derived `runId` and `path`; its other
entry fields carry their zero values. A successful command exits `0`, including
when `runs` is empty. Usage errors exit `2`; a records-directory resolution or
read failure exits `9`. Those failure paths emit no JSON document.

## `runs prune --json`

The root object has exactly `schemaVersion`, `deleted`, `keptNewest`,
`keptRunning`, and `keptUnreadable`:

- `deleted`: the run IDs actually deleted, as a string array
- `keptNewest`: the number of records retained by `--keep`, capped at the
  number of records found
- `keptRunning`: run IDs spared because their runs are still running
- `keptUnreadable`: run IDs spared because their records could not be safely
  classified

All three ID arrays are always present and non-null, including when empty; no
root field is omitted. `--json` requires `--yes`: without it, prune emits no
document and exits `2` before resolving the records directory. A successful prune exits `0`, including when nothing is deleted. A
records-directory resolution, read, or open failure exits `9` and emits no
document. A delete failure or cancellation exits `9` after selection and emits
the document, reporting the IDs known to have been deleted or kept.

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
  assistant text but retained assistant text exists; carries the most
  recent retained assistant text, which may precede the message in flight,
  is at most `MaxFrameBytes` (8 MiB) in UTF-8 bytes, and never appears
  together with `explanation`
- `error`: present when the worker reports an error. When the latest assistant
  `message_end` has the stable `stopReason: "error"`, the worker reports the
  exact fixed wording `upstream/model turn ended with an error`. This does not
  claim that no text existed or attribute the error to a particular provider
  mechanism. Text emitted by that failed turn is partial evidence in
  `partialExplanation`, never a final `explanation`; the assistant
  `errorMessage` is never copied. A newer assistant message supersedes the
  prior classification; user and tool-result messages do not, and a missing
  or malformed stopReason does not inherit an earlier error. A settled empty
  assistant message with another stop reason retains the generic empty-answer
  failure wording.
- `data`: present only when the task carried `--data` files; one entry
  per carried file, each with `path`, `byteCount`, and `sha256`
- `usage`: present only when at least one assistant message reported a
  non-zero usage figure; the token counts and dollar figures Pi
  computed, passed through unchanged

Partial text reporting is additive and optional, so `schemaVersion`
stays `1`. Worker `partialExplanation` appears only when a run ended
without a final text — timed out, cancelled, or failed before
settlement — and retained assistant text exists. It carries the most
recent retained assistant text, which may come from the message preceding
the one in flight when that message has no assistant text. The two retained
message buffers share one `MaxFrameBytes` (8 MiB) UTF-8 byte budget; when the
budget is exceeded, older text is evicted, including the oldest prefix of the
current message when necessary. It is never present together with
`explanation`: a consumer reading `explanation`
can always assume the model finished, so truncated text under that name
can never turn an interrupted run into a complete-looking answer.

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
without the flag, or one that did not complete, carries none. The evidence
objects describe the delegated workers after they settle: workspace git
state, the change manifest, and the `--writes` verdict are captured before
the verification command runs, so files the check creates, changes,
commits, or removes never appear in them. The verification result records
only the command's own exit code and output; a caller who needs a clean
evidence report must keep the check read-only or inspect its artifacts
separately.

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

The worktree result is additive and optional, so `schemaVersion` stays `1`.
Root `worktree` is present only for a run started with `--worktree`; it is
absent otherwise. When present, it is an object with exactly two string
fields:

- `path`: the checkout path assigned to the run
- `branch`: the branch created for the checkout

Git state is additive and optional, so `schemaVersion` stays `1`. Root
`git` appears only when a run moved the workspace's HEAD, its branch,
or a stash entry appeared or disappeared between the start and the end
of the run; a modified working tree alone (only `dirty` differing) does
not produce it. The after state is captured before the verification
command runs, so a check that moves git state is not part of the `git`
object. When present it carries `before`, `after`, and
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
distinct from `measurement failed`, which covers the other failure
positions: a guard that passed and a later command that failed, a git
trust state that was already unsafe when the run started, and a trust
input that changed while the run was in flight. The trust paragraph
below names each input and why an unsafe or moved one makes the
manifest unavailable rather than clean.

The manifest is measured against the git state recorded before the first
worker started, so a run that committed its own work still lists the files
it changed, and before the verification command runs, so a check that
writes files is not part of the manifest. Because the measurement
compares the workspace before the run against the workspace after it, it
cannot tell the run's writes from
anyone else's: a file an editor or a watcher saves while the run is in
flight appears as a change the run made, and with `--writes` declared the
check reports it undeclared and the run exits `4`. While a run is in
flight, keep one run at a time per workspace and leave the workspace
alone. Paths already dirty when the run started are stamped up front,
and the ones whose identity never moved are subtracted from the result:
they were equally dirty before the run and name no change it made. A
stamp holds size, modification time, and the executable bit — the one
mode bit git tracks, so a chmod between two non-executable modes does
not register as a change — plus the SHA-256 of the path's content,
read once up front, reduced to a hash, and never retained, reported, or
transmitted. The content identity is captured for every entry kind it
defines: a tracked path, whose stat cache git may trust, and an
untracked regular file or symlink, whose in-place rewrite the stat
fields cannot see either. The content identity exists because the stat
fields alone cannot tell an untouched file from a deliberate same-size
rewrite with a restored modification time: the rewrite makes the stat
stamp match by construction, and only the bytes can say the file moved
— for a symlink, the target string is its content, and re-pointing it
at a same-length target is the same shape of invisible move. A rewrite
of that shape therefore stays in the manifest — with
`dirtyBefore` true and its counts measured against the last commit for
a tracked path, against the empty side for an untracked one —
while a legitimate net-zero restoration still subtracts: when the bytes
are exactly the pre-run bytes again, the hash matches and the manifest
is quiet because net change is zero. An untracked directory tree is
the one deliberately unhashed shape: it cannot rewrite in place, so its
pre-run stat stamp alone is enough, and the subtraction stays exact for
the files whose content it names. On a
coarse-granularity filesystem — FAT, exFAT, some NFS mounts, older
ext3 — a same-tick restore of any path is invisible too,
which is equally defensible because net change is zero; a hash still
protects the paths whose content the stamp carries.

The measurement trusts the path queries git itself runs, so a
repository whose trust state could make those queries hide a write is
never answered with a confident clean: the manifest is omitted with the
`measurement failed` reason, and a declared-writes check skips, when
the state was already unsafe before the run started or a trust input
moved while the run was in flight. Five families of inputs are
watched, captured from the repository itself before the first worker
starts and re-read after the run ends:

- Index visibility markers. A `skip-worktree` or `assume-unchanged`
  entry makes git suppress the worktree comparison for that file, so a
  rewrite of it is invisible to every diff the measurement runs: an
  entry already marked before the run hides with no drift to detect,
  and a marker that appears during the run is drift.
- `core.trustctime`. Git trusts the stat cache without comparing
  content when size and modification time match and ctime is not
  trusted, so `core.trustctime=false` can make git itself report
  nothing for a same-size rewrite with a restored modification time.
  The effective value (false, true, or the true default) is recorded
  before the run and compared after it: an unsafe pre-existing value
  and a value that changed during the run are both unavailable.
- `core.fileMode`. Git suppresses every tracked mode-only difference
  when it is false, so a chmod the run made on an untouched file would
  be invisible to every diff the measurement runs. The effective value
  (false, true, or the true default) is recorded before the run and
  compared after it: an unsafe pre-existing value and a value that
  changed during the run are both unavailable.
- Ignore-rule inputs beyond the tree. The untracked listing honours
  `$GIT_DIR/info/exclude` and the effective `core.excludesFile` (the
  configured file, or the XDG default when unset), so a rule appended
  to either during the run can hide untracked paths the run wrote.
  Each file is stamped without reading its contents, and the effective
  value is recorded; a moved stamp or a moved value is drift.
- In-tree `.gitignore` rule files. The untracked listing honours every
  `.gitignore` rule file git consults in the tree, so a rule appended
  to one during the run — or a new rule file created during the run,
  including one whose own rules exclude it — can hide untracked paths
  the run wrote. The rule files are enumerated with git's own listings
  (the tracked ones, the visible untracked ones, and the ones git
  itself ignores, each restricted to `.gitignore` names), never with a
  filesystem walk and never by entering a nested repository, and their
  set and local content identity are recorded: a rule file that
  appears, disappears, or changes content during the run is drift.
  Content identity means the bytes — for a symlink, its target string
  — so a same-size rewrite with a restored modification time is still
  drift, while a write that leaves the bytes identical is not. This is
  a watch on the rule files git's own listings name, not a full audit
  of every ignore source git could read.

Any of these makes the measurement unavailable rather than clean — a
wrong "clean" is worse than an admitted "unavailable". Ordinary worker
activity never trips the watch: staging and committing change no trust
input, and ignore rules that already existed when the run started
applied equally to the pre-run and post-run passes.

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
  is measured rather than skipped, so it never triggers the skip on its
  own — an unsafe or drifted trust state still omits the manifest with
  `measurement failed`, and the check then skips like any other. A
  declaration where
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
