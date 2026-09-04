# Pi Worker v0 Usage

This documents the current public v0 behavior.

## Agent skill

The canonical provider-neutral agent skill is
[`skills/pi-worker`](../skills/pi-worker). Use it when delegating through Pi,
selecting cheaper or separately metered models, or assigning one to three Pi
workers. It resolves informal model names from the catalog, uses an exact
explicit selector without fallback, and otherwise lets the configured default
apply. It keeps model and explicit reasoning effort separate, reports any
thinking fallback to Pi's confirmed default, uses external task files, keeps
parallel work disjoint, and returns the parsed final JSON result without debug
or raw protocol output.

## Prerequisites

- Node.js 22.20.0 or newer and npm.
- Installed Pi CLI. Version **0.84.4** is the verified compatibility surface;
  other semantic versions remain usable with an explicit warning.
- Provider authentication is configured in Pi itself. Do not pass credentials/secrets via `pi-worker` argv.
  - Open Pi interactively and use Pi's own authentication flow.

The npm package supports macOS and Linux on arm64 and x64. Windows requires a
source build and is compile-checked, but it is not runtime-tested in the current
release gates.

Install the public package normally, or keep installer diagnostics visible:

```sh
npm install -g pi-worker
npm install -g --foreground-scripts pi-worker
```

The binary alone can be installed from the public Go module:

```sh
go install github.com/arasovic/pi-worker/cmd/pi-worker@latest
```

The binary lands in `go env GOBIN`, or in `$(go env GOPATH)/bin` when GOBIN
is unset. That directory must be on PATH for the `pi-worker` command to
resolve. Confirm the location with:

```sh
gobin=$(go env GOBIN)
ls "${gobin:-$(go env GOPATH)/bin}/pi-worker"
command -v pi-worker
```

## Source build

For the current checkout, the repository declares Go 1.25 language compatibility
and a Go 1.26.1 toolchain:

```sh
go build -o ./bin/pi-worker ./cmd/pi-worker
```

After a source build, use `./bin/pi-worker` in place of `pi-worker` for the
commands in this document.

> Source-build metadata defaults to `version=dev`, `commit=unknown`, and
> `build date=unknown`; the human version output is `pi-worker dev`. Release
> artifacts inject all three values and print the full 40-hex commit.

## npm postinstall

During `npm install`, npm attempts to install the bundled provider-neutral
`pi-worker` skill for detected agent targets via pinned `skills@1.5.23`. It
records an `installed`, `blocked`, `skipped`, or `failed` outcome in the durable
receipt. Existing conflicts may block, skip, or fail without overwriting them.
On an unsupported npm platform or architecture, setup can skip before the
native CLI creates a receipt; `pi-worker skill status` is unavailable when the
launcher itself is unsupported.

Interactive terminals show a compact status block with the package version,
skill outcome, installed target count, and next command. The outcome word is
the only colored element. CI, `NO_COLOR`, and non-interactive installs retain
one plain diagnostic line.

## Supported commands

- `pi-worker version [--json]`
- `pi-worker models ...`
- `pi-worker doctor [--timeout <duration>] [--json] [--debug]`
- `pi-worker config show [--json]`
- `pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]`
- `pi-worker skill status [--json]`
- `pi-worker skill receipt-path [--json]`
- `pi-worker runs list [--json]`
- `pi-worker runs prune --keep <n> [--yes] [--json]`
- `pi-worker run ...`

## Version

```text
pi-worker version [--json]
```

- Human output identifies the build as `pi-worker <version>` and includes the
  release commit and build date when injected.
- `--json` emits one complete `schemaVersion: 1` document with `version`,
  `commit`, and `buildDate`. Source builds report `dev`, `unknown`, and
  `unknown` explicitly.

## Model catalog

```text
pi-worker models [--timeout <duration>] [--json] [--debug]
```

- `models` queries Pi once with `get_available_models`; it never activates a
  model or submits a prompt.
- The catalog process enables only `read,grep,find,ls`; it cannot write files
  or run shell commands through Pi.
- Human output is one sorted exact `provider/id` selector per line.
- `--json` emits one object with `schemaVersion: 1` and sorted `models`, each
  containing `provider`, `id`, and `selector`.
- The default timeout is `30s`. An empty catalog is a readiness failure (exit
  code `3`), as is a missing or unavailable Pi executable. Malformed Pi data is
  a protocol/internal failure (exit code `9`).

## Local readiness doctor

```text
pi-worker doctor [--timeout <duration>] [--json] [--debug]
```

- `doctor` is inspection-only: it never repairs configuration, logs in,
  switches providers or models, reads Pi profile/auth files, invokes a model,
  or submits a prompt.
- It performs these checks in this exact order: `pi-executable`, `pi-version`,
  `config`, `model-catalog`, `default-model`, and `workspace`.
- The `workspace` check is advisory and never makes the environment unready.
  It asks the same guard a run asks, so `doctor` and `run` cannot disagree
  about the same directory. A warning means the workspace is not inside a
  confirmed git work tree (or its git state could not be fully measured): the
  run still works, but it cannot prove what it changed — its change manifest
  is omitted with the work-tree-unconfirmed reason and its declared-writes
  check is skipped. A directory outside a work tree is not a broken
  environment; it is a reduction in what a run can report.
- Pi `0.84.4` is verified and reports `ok`. A different valid semantic version
  reports `warning` and leaves the environment ready. Unreadable or malformed
  version output reports `failed`. A missing configuration is a warning;
  warnings leave the environment ready. A failed check makes it not ready.
- Skill installation status is reported separately by `pi-worker skill status`.
- `model-catalog` sends only `get_available_models`; it does not activate a
  model or send a prompt. An empty catalog is failed.
- The default timeout is `30s`. Human output has one line per ordered check and
  an overall readiness line. For a completed inspection, `--json` writes exactly
  one `doctor.Result` document to stdout. A timed-out, cancelled, or internally
  aborted inspection writes no result to stdout; `--debug` and diagnostics write
  only to stderr.
- Exit codes are `0` for ready or warning-only results, `3` for readiness
  failures, `7` for timeout, `8` for cancellation, and `9` for protocol or
  internal failures. Invalid flags return `2` before an inspection starts.

## Skill installation status

```text
pi-worker skill receipt-path [--json]
pi-worker skill status [--json]
```

- `skill receipt-path` reports the absolute path where the installer receipt is
  stored.
- `skill status` reads and verifies that receipt, including managed/affected
  target status and recovery instructions. The npm launcher also probes live
  global skill targets without adopting or changing them.
- The command is read-only: it never installs, repairs, removes, or adopts a
  skill.
- `--json` emits one complete `schemaVersion: 1` document with `status`,
  `receiptPath`, `installerVersion` (the version recorded when the install
  ran), `programVersion` (the version of the binary producing the document),
  verified and receipt-tracked targets, affected targets, recovery
  instructions, and `externalInspection`. Both version fields are reported
  without a leading `v`, so a release tag and the version it records look
  alike.
- `status` is `stale` when every managed file still matches the receipt but
  the receipt's `installerVersion` differs from the running program's
  `programVersion`, with a single leading `v` ignored on either side so a
  release tag (`v0.7.0`) and the version it records (`0.7.0`) compare as the
  same: the skill on disk is the one the older installer shipped, while the
  program running now ships a newer skill. The document carries both
  versions, and `recovery` names the safe reinstall command. A source
  build reports `programVersion` `dev` and never reports `stale`, because a
  dev build does not name a release it could compare against.
- `externalInspection.state` is always `performed` or `unavailable`, and
  `targets` is always an array. Standalone native binaries report
  `unavailable`; the npm launcher reports `performed` only after every global
  target was resolved and inspected. Any partial failure discards all external
  findings. Missing paths are successful absence.
- External findings are informational and never change the receipt-derived
  exit code. Known identity markers are externally managed and may be stale;
  unknown or absent markers require manual inspection.
- Exit code `0` means the inspection reports `verified`. Missing receipts,
  missing or drifted targets, blocked, skipped, and failed completions, and
  `stale` reports exit `3`.
- A missing receipt file reports `status: "missing"` and exits `3` with a complete JSON document.
- Malformed or unreadable receipts return code `9`.
- Usage, cancellation, and internal failures emit no `stdout` document.

## Run records

```text
pi-worker runs list [--json]
pi-worker runs prune --keep <n> [--yes] [--json]
```

- A run record is one `.jsonl` file per run, one line per event: the
  start line written before any worker starts, one line per started
  worker, and the finish line when the run completes. Records live in
  the `runs` directory inside pi-worker's user configuration directory
  — the operating system's user configuration directory plus
  `pi-worker/runs`, the location `runlog.Dir()` resolves. Only
  `*.jsonl` files in that directory are records; `reported.json`, its
  `.tmp-*` stages, and any other file are not.
- `runs list` is read-only: it prints one line per record, newest
  first, and writes nothing — no marker, no watermark, no
  interrupted-run warning.
- A record's outcome is one of: the run's own outcome, verbatim, when
  its finish line carried a result; `error` when the finish line
  carried the error arm instead; `running` when there is no finish
  line and the start line's process is still alive (a liveness error
  counts as alive); `interrupted` when there is no finish line and
  that process is gone; `unknown` when the record has no usable start
  line at all.
- `runs prune --keep <n>` keeps the newest `n` records and deletes the
  rest — one file at a time, stale `unknown` records included — and
  never deletes a record whose run is still running, whatever its age.
  `--keep 0` keeps none by age; running runs are still spared. An
  `unknown` record is deleted like any other, except while its file
  has changed within the last hour: prune cannot tell an unreadable
  record apart from one being written, so a record that changed
  recently is kept and reported — a later prune deletes it once it
  stops changing. That freshness question is asked twice, when the
  records are chosen and again at the moment each one is deleted,
  because the confirmation question in between waits on a person for
  as long as they take. A stale symlink named like a record is
  deleted like any other unreadable record — the link is removed,
  never whatever it points at. Pruned ids are never removed from the
  interrupted-run marker, and the marker itself is never a deletion
  target.
- Without `--yes`, prune lists on stderr the records it is about to
  delete and asks `delete <n> run records? [y/N]`; only `y` or `yes`
  (case-insensitive, after trimming spaces) proceeds, and anything
  else — `n`, an empty line, an EOF — deletes nothing, prints
  `nothing deleted`, and exits `0`. With neither `--yes` nor a
  terminal on stdin, and always with `--json` without `--yes`, prune
  refuses, deletes nothing, and exits `2` — the refusal comes before
  everything, the records directory not even resolved first, so it
  exits `2` even when the directory cannot be resolved or read. A
  Ctrl-C stops the prune, including while the confirmation question
  is on screen: what was already deleted stays deleted and is still
  reported, nothing further is removed, and the exit is `9`.
- The human output prints one line per record as it goes — `deleted
  <id>` for a record it removed, `kept <id>` for one it spared at the
  last moment — and then a summary line, `kept <n> newest`, plus one
  clause per spared kind — `, <m> still running` and `, <m>
  unreadable` — each clause appearing only when at least one record
  of that kind was spared. The unreadable clause says only that the
  record could not be read; the two ways a record ends up in
  `keptUnreadable` are listed below.
- At the moment of each delete the name is re-checked against what
  was listed: a record whose name no longer holds the file that was
  listed — a replacement, a freshly written file under the same name,
  or nothing the selection ever captured — is refused, reported on
  stderr, and not counted as deleted, except an `unknown` record
  whose file has changed within the last hour, which is spared before
  the identity check ever runs and is reported as kept even when the
  name now holds a different file; and the same for a record that
  cannot be examined at all. When the name still holds the listed
  file, a regular file or a symlink is removed — the symlink removal
  takes the link, never its target — and anything else under a
  record's name is refused. An `unknown` record whose file has
  changed within the last hour is spared before the identity check,
  as above.
- Both commands emit one `schemaVersion: 1` document with `--json`.
  `runs list --json` carries a `runs` array — empty, never null, when
  there are no records. `runs prune --json` (which requires `--yes`)
  carries `deleted`, the run ids actually deleted, and the two kept
  arrays, `keptRunning` and `keptUnreadable`: a run still running
  lands in the first, a record that could not be read in the second —
  a record ends up there when its file changed within the last hour
  or its timestamp could not be read at all — the split is by what
  is known about the record, each id in its own array, and an
  unreadable record is never reported as running. It also carries
  `keptNewest`, how many records the `--keep` rule kept (capped by
  the records that exist). The three arrays are empty, never null,
  whether nothing was deleted, no running run was spared, or nothing
  unreadable was spared.
- Exit codes: `0` on success — for prune, that includes nothing to
  prune and the user declining; `2` for a usage error or the prune
  refusal; `9` when the records directory cannot be resolved or read,
  when a prune delete fails (a failure is reported on stderr, does
  not stop the other deletions, and the exit is `9`), or when the
  prune is cancelled.
- At the start of every run, pi-worker also checks whether an earlier
  settled run left processes running, and warns on stderr when it
  finds any: one line per run naming the record's full path and the
  leftover pids — up to ten, then a count of the rest, inside the same
  parentheses — capped at five runs with one summary line naming the
  records directory for the remainder. The warning appears at the
  start of the next run and is repeated on every run for as long as
  the processes are still running; it only reports, and never kills
  or signals the processes it names.
- A process the worker started in a group of its own is not reported:
  the warning finds survivors by the process group the run's worker
  led, and a command started in a fresh group carries no link back to
  the run once the worker is gone.
- After a run's group has fully emptied, the operating system may hand
  its number to something else, and the warning can then name a
  process that has nothing to do with the run. It is rare, and it
  costs one wrong line of text — the product never acts on the number.

## Exact run command

```text
pi-worker run [--task <prompt> | --task-file <path>]... [--model <provider/model>] [--thinking <level>] [--data <paths>] [--writes <paths>] [--timeout <duration>] [--verify <command>] [--worktree <name>] [--json] [--debug]
```

## Personal default model

`pi-worker` stores a two-field, versioned JSON configuration document in the
operating system's user configuration directory. It contains only
`schemaVersion` and `defaultModel`; the empty default is provider-neutral.

```text
pi-worker config show [--json]
pi-worker config set default-model <provider/model> [--debug] [--timeout <duration>]
```

Replace the provider/model placeholder with one exact selector printed by
`pi-worker models` before running `config set`.

- `config show` reads the local document only. It never launches Pi. A missing
  configuration is an empty default and exits `0`; an invalid configuration
  exits `9`.
- `config set` requires an exact `provider/model` selector, queries Pi's model
  catalog once, and writes the default only when that exact selector is
  available. A missing or unavailable Pi executable is a readiness failure
  (exit code `3`); protocol/internal failures use exit code `9`. There is no
  fallback.
- Configuration writes use a same-directory temporary file, file sync, atomic
  replacement, and owner-only permissions where supported.
- When the configuration path itself is a symbolic link, `config set` refuses
  before anything is touched: it never replaces or writes through the link it
  inspected, prints the reason to stderr, exits `9`, and leaves the link and
  its target exactly as they were. A dangling link — one whose target does not
  exist — is refused the same way, and its target is never created. Only the
  final `config.json` component is checked: a symlinked parent directory is
  not a refusal, and the write lands in the directory the parent link names.
  Reads still resolve the link: `config show`, `run`, and `doctor` report the
  document the link points at.
- The refusal is one check at one instant, not a lock: another process can
  replace the path after the guard has passed it, and the save then replaces
  whatever holds the name — a swapped-in link included — because the write is
  an atomic rename over the destination, never a follow into a target. The
  guarantee is that a final component observed as a symlink is refused before
  anything is touched and that a save can replace a link but never write
  through one.
- Model precedence is a task's own `--model`, then the run-level
  `--model`, then the configured `defaultModel`, then a usage error
  (exit `2`). When no model resolves, stdin is not read.

## Behavior

### Model selection

- Before starting workers, `run` probes `pi --version` once for the whole run.
  An unverified, unreadable, timed-out, or otherwise failed probe writes one
  bounded warning to stderr and execution continues. It never changes JSON
  stdout or creates a new exit path; the RPC lifecycle still validates the
  effective model and thinking state fail-closed.

- `--model` must be an exact `provider/model` string from the catalog. A
  name the catalog offers is accepted whatever the id contains — an id may
  carry its own upstream slash, a colon, or anything else a routing provider
  reports — and a name the catalog does not offer is refused by the
  catalog-membership check.
- A task's own `--model` wins over the run-level one, and an explicit
  `--model` at either level always wins over the configured default.
  `--model` never rewrites the configuration document, and the configured
  default is read only when some task will fall back to it — never when
  every task names its own model.
- `run` resolves the model by:
  1. `get_available_models`
  2. exact catalog match
  3. `set_model`
  4. confirmation check that `set_model` returns the exact same `provider` and `id`
- If no exact model match exists, or confirmation differs/missing, execution stops with an error. There is **no** pattern matching, fallback, or switching.

### Thinking level

- `--thinking` accepts exactly `off`, `minimal`, `low`, `medium`, `high`,
  `xhigh`, or `max`, binding per task like `--model`: one that follows a
  `--task` or `--task-file` is that task's level, at most once per task,
  and one that precedes every task is the run-level value.
- Model and effort are separate. Effort is a separate flag (`--thinking`);
  a name is accepted when the catalog offers it, whatever the id contains,
  so a name is never a way to smuggle effort into `--model`.
- After exact model activation, every worker calls `get_state` to confirm the
  active model and capture Pi's default `thinkingLevel`.
- With explicit `--thinking`, the worker queries
  `get_available_thinking_levels`, applies the exact level when supported, and
  confirms the effective value with a second `get_state`.
- If the explicit level is unsupported or `set_thinking_level` returns a
  well-formed rejection, the worker keeps the captured Pi default, emits a
  warning, and continues. A successful task still exits `0`.
- Malformed RPC data, transport failure, active-model mismatch, or a successful
  set that is not confirmed are hard failures; they never fall back.
- Omitting the flag is not the same as `off`: `off` is an explicit choice.
  A task without its own level takes the run-level value, and when no
  level is given anywhere the run uses Pi's confirmed default and reports
  it. The configuration file does not persist a thinking default.
- `models` and `doctor` do not enumerate thinking support because doing so
  requires activating a model; both commands remain inspection-only.

### Workspace verification

- `--verify <command>` may be given at most once and runs one check command
  in the workspace after the workers settle, once per run.
- No shell is involved: the value is split on whitespace into argv, so
  shell syntax cannot work. A value containing `|`, `&`, `;`, `<`, `>`,
  `$`, a backtick, a newline, a quote, or a backslash is rejected as a
  usage error naming the offending character, and an empty or
  whitespace-only value is rejected too. A quoted or escaped argument is
  refused rather than silently mis-split into stray fragments, so a
  command that needs shell quoting or escaping must be put in a script
  the check invokes instead. `--verify "go build ./... && go test
  ./..."` fails up front with the `&` named instead of executing
  something surprising.
- Verification runs only when the run completed with the context intact: a
  partial or failed run leaves the workspace half-written, and a timed-out
  or cancelled context skips the check.
- A passing check exits `0`. In human mode it prints one short
  `verification: ok` line after the worker summaries and carries no further
  output on purpose; in `--json` mode the result carries `verification`
  with `argv` and `exitCode: 0` only.
- A failing check (non-zero exit) exits the process `6` without changing
  the run `status` field or any worker status: those describe worker
  outcomes, and the process exit code, the reported error, and the root
  `outcome` carry the verification failure. Human mode prints the exit
  code and the captured excerpt (the first 2 KiB and the last 6 KiB when
  long, with the elided middle marked), plus the full
  `pi-worker-verify-*.log` path in the system temp directory when one
  was written; `--json` mode carries `output`, `truncated`, and
  `logFile` in the `verification` object.
- A context that expires while the check runs is not a verification
  failure: the run ran out of time and exits the way a timed-out run
  exits (`7`).

### Workspace git state

- When the current directory is inside a git work tree, `run` records
  the git state once before any worker starts and once after every
  worker settles: HEAD, the current branch, the dirty flag, and the
  stash count. There is no flag for this; it happens on every run.
- When the run moved HEAD, the branch, or the stash list, human mode
  prints one `pi-worker: warning: the run changed git state: ...` line
  on stderr naming what moved, and `--json` mode carries a `git` object
  with `before` and `after` states. A modified working tree alone does
  not trigger it: leaving modified files behind is the point of a run.
- The after state is collected on every terminal status, including a
  timed-out or cancelled run, under a fresh five-second budget when the
  parent context is already done.
- A workspace outside a git work tree, or a failed inspection, is a
  silent no-op: the result carries no `git` object and no warning is
  printed, and the run status and exit code are unchanged.

### Workspace change manifest

- When the current directory is inside a git work tree, `run` measures
  which paths the run changed and by how much, diffing against the git
  state recorded before the first worker started. This is pi-worker's own
  measurement, not the worker's account of its work. Because the
  measurement compares the workspace before the run against the workspace
  after it, it cannot tell the run's writes from anyone else's: a file an
  editor or a watcher saves while the run is in flight appears as a change
  the run made, and with `--writes` declared the check reports it
  undeclared and the run exits `4`. While a run is in flight, keep one run
  at a time per workspace and leave the workspace alone. The manifest
  covers the paths `git` tracks or would track:
  ignore rules exclude untracked paths only, so an ignored path is outside
  both the manifest and the write check when it is untracked and a run that
  wrote only such paths reports a clean verdict; a tracked path is measured,
  and therefore checked, whether or not a rule matches it. Submodules are
  the other exclusion: every diff that measures paths ignores them, so a
  dirty submodule is never a changed path and can never be reported as an
  undeclared write — the manifest measures this workspace's files, and a
  submodule's contents are another repository's business.
- Human mode prints one `changes: <n> files, +<a>/-<d>` line (singular
  `file` at one) on stdout after the worker summaries, followed by up to
  five paths most churn first. When at least one listed entry was already
  dirty before the run, the line appends `(N already modified before the
  run)`: those entries' counts are measured against the last commit rather
  than against pre-run content, so they include work that was already
  there. The sums and the clause count the carried entries only, capped
  at 100, not all `<n>` files; the line is information, not a warning, so
  it carries no `warning:` prefix.
- `--json` mode carries a `changes` object. It is not gated by the git
  tripwire: a run that only left modified files behind carries `changes`
  and no `git`.
- The manifest is measured on every terminal status, including a timed-out
  or cancelled run, under its own thirty-second budget: a run that stopped
  mid-edit is exactly the run whose changes a caller most needs.
- A dirty working tree is measured by subtraction, not guessed: paths
  already dirty when the run started are stamped up front, and the ones
  whose identity never moved are subtracted — they were equally dirty
  before the run and name no change it made. A stamp holds size,
  modification time, and the executable bit — the one mode bit git
  tracks, so a chmod between two non-executable modes does not register
  as a change — plus, for tracked paths, the SHA-256 of the content,
  read once up front and never retained, reported, or transmitted. The
  hash exists because a same-size rewrite with a restored modification
  time makes the stat stamp match by construction: only the bytes can
  say the file moved. That tracked rewrite is therefore reported, with
  `dirtyBefore` and its counts measured against the last commit, while
  a legitimate net-zero restoration still subtracts — when the bytes
  are the pre-run bytes again, the manifest is quiet because net change
  is zero. Untracked pre-run files are stamped by stat alone, never
  hashed, so a same-size rewrite of one with a restored modification
  time is the one deliberate false negative left in the subtraction;
  on a coarse-granularity filesystem a same-tick restore of one is
  invisible too, which is defensible because net change is zero.
  Dirtiness never depends on the repository's display preference: the
  status command forces `status.showUntrackedFiles=all`, so a
  repository that hides untracked files from `git status` still records
  a tree that is genuinely dirty.
- The measurement trusts the path queries git itself runs, so a
  repository whose trust state could make those queries hide a write is
  never answered with a confident clean: the manifest is omitted with
  `measurement failed` and a declared-writes check skips when the
  state was already unsafe before the run started or a trust input
  moved while it ran. The watched inputs, captured from the repository
  before the first worker starts and re-read after the run ends, are
  the index visibility markers (`skip-worktree`, `assume-unchanged`),
  the effective `core.trustctime` value, and the ignore-rule files
  beyond the tree — `$GIT_DIR/info/exclude` and the effective
  `core.excludesFile` file — each stamped without reading its contents.
  A marker or unsafe value already present at the start, a marker that
  appears during the run, a trust value that changes, or an ignore file
  whose stamp or effective value moves all make the measurement
  unavailable: a wrong "clean" is worse than an admitted "unavailable".
  Ordinary run activity never trips the watch: staging and committing
  change no trust input, and ignore rules that already existed when the
  run started applied equally to both passes.
- An unborn HEAD, a context already done when the inspection ran, a
  measurement that failed after the workspace was confirmed to be a git
  work tree, and a work tree that could not be confirmed — the
  directory is not a git work tree, git is missing entirely, or the
  guard failed for a transient reason, which the code cannot tell apart
  and the reason does not claim to — are all omitted with a stated
  reason, and human mode prints `changes: omitted: <reason>` for them.
  The manifest never vanishes from a real run: the CLI always
  configures the git inspector, so a workspace outside a git work tree
  reads `changes: omitted: work tree not confirmed` rather than
  nothing.

### Exactly one input mechanism

- `--task` and `--task-file` may each repeat up to 3 total tasks.
- Only one input source is allowed:
  - `--task` repeated 1..3 times, or
  - `--task-file` repeated 1..3 times, or
  - no task flags: one task read from stdin.
- Mixing `--task` and `--task-file` is rejected.
- Default timeout is `30m` and applies to the whole foreground run (all workers share one deadline).

### Workspace and worker sharing

- The current working directory is the writable workspace.
- Each worker gets a fresh temporary session directory created with `os.MkdirTemp("", "pi-worker-v0-*")`.
- For more than one worker, all workers share that workspace and a warning is printed:

```text
pi-worker: warning: N workers share the writable current workspace; tasks must use disjoint files
```

- Assign disjoint files, or use only one worker; `--worktree` (below) is the way to give one run a workspace of its own.
- Every v0 worker always enables `read,grep,find,ls,edit,write,bash` with `--no-approve`; `bash` can execute arbitrary shell commands with the current user's host permissions, and this is not a sandbox.

### Isolated run workspace

- `--worktree <name>` optionally gives one run a private git checkout
  instead of the current working directory. The name is validated at
  argv-parse time: 1 to 64 characters of lowercase letters, digits and
  hyphens, starting and ending with a letter or digit; anything else is
  a usage error (exit `2`), like every other argv mistake.
- The checkout is created at `<repo-root>/.pi-worker/worktrees/<name>`
  on branch `run/<name>`, from `HEAD` — the current `HEAD` of the
  caller's tree. Uncommitted work in the caller's tree is not carried
  into it.
- The checkout is a separate working directory, not containment: `bash`
  still runs with the user's host permissions and a worker can reach
  outside the checkout. `--worktree` bounds where the run works by
  default, not what it can reach.
- A name whose checkout directory already exists, or whose branch
  `run/<name>` already exists, refuses the run before it starts —
  before the run record is written and before any worker launches —
  exiting `2` and naming the leftover. A checkout git itself refuses
  to create is refused the same way, with git's own words in the
  message. Outside a git work tree the flag is refused the same way.
- Nothing is ever removed: a leftover checkout or branch is reported by
  the next run that collides with its name, never cleaned up by the
  product.
- The checkout path and branch are printed to stderr at creation time
  (the only trace a killed run leaves) and ride the `--json` result as
  `worktree.path` and `worktree.branch`.
- A repository that uses this should ignore `/.pi-worker/`: the
  checkouts live under the repository root, and an unignored one lands
  in every other run's change manifest.
- The one-run-at-a-time rule under `### Declared writes` is a rule per
  *workspace*: two runs given different `--worktree` names work in
  different checkouts and no longer share one workspace.

### Declared writes

- `--writes <paths>` optionally declares, as a comma-separated list, the
  workspace-relative paths a task intends to write; whitespace around
  each comma-separated path is ignored, and each value is cleaned where
  it is compared, so `src/a/`, `./src/a`, `src//a`, `src/./a`, and a
  non-escaping interior `..` all name `src/a`. An absolute path is
  rejected as a usage error before any worker starts, exiting `2`, and
  the rejection names the remedy — declare paths relative to the
  workspace. In a run with more than one task, `--writes` binds
  positionally, to the task most recently introduced by `--task` or
  `--task-file`, and must follow the task it applies to — placed before
  them all, no task can be named and the run is rejected with the remedy
  stated. In a run with exactly one task the order carries nothing:
  `--writes` may appear anywhere in the argument list, before the
  `--task` or `--task-file` included, and a prompt read from stdin — a
  run with no task flag at all — declares its writes the same way. It may
  appear at most once per task. An empty value, `--writes ""`, declares
  that the task writes nothing; whitespace-only is the same declaration
  because the flag already trims. The empty string is the one spelling
  that cannot collide with a real path. A task that declared the empty
  set has declared: when every task in the run declared — the empty
  declaration included — and the change manifest was measured, the
  post-run check runs, so a read-only round can be proven to have written
  nothing rather than merely asserted to. Only a measured manifest makes
  that proof: on an unborn HEAD, a dead context, a failed measurement, an
  unconfirmed work tree, or a manifest omitted for any of those reasons,
  the check skips with `change manifest unavailable` and the run exits
  `0`, whatever was declared. A dirty before-state is measured rather
  than skipped, so there the check runs — except when the trust state
  makes the measurement unavailable, in which case the manifest is
  omitted with `measurement failed` and the check skips like any other
  omitted manifest. A checked run that changed nothing reports a clean
  verdict; one that changed a path reports it undeclared and exits `4`.
- A declared path covers everything beneath it on a segment boundary:
  `src/a` covers `src/a/b.go` and does not cover `src/ab.go`, whether
  `src/a` names a file or a directory. Comparison is byte-exact per
  segment with no case folding: on a case-insensitive filesystem a
  declaration differing from the changed path only in case still does not
  cover it. Folding case would make the check miss real violations on a
  case-sensitive filesystem, and a check whose job is to accuse must not
  accuse less than it should.
- The declaration is optional and is checked twice: before any worker
  starts, a run whose declared sets overlap is rejected up front as a
  usage error (exit `2`), and after the run the declaration is compared
  against the paths the run actually changed. The declaration is
  all-or-none: every task in the run declares, or none does. A run where
  some tasks declared and others did not is rejected up front as a usage
  error (exit `2`) before any worker starts, and the empty declaration
  is how a task that writes nothing takes part. It is a pre-flight
  contract, not a sandbox or worktree:
  pi-worker does not enforce it during the run.
- While any run declares `--writes`, that workspace must have one run at a
  time: the check compares against the whole workspace's pre-run git
  state, so a concurrent writer lands in whichever run is measuring.
- When every task declared — empty set or not — and the check passes,
  the shared workspace warning is suppressed; when no task declared, the
  warning stays.
- The run reports the changed paths no task declared, and exits `4`
  (policy) when it found any. The run `status` field is unaffected: a run
  whose workers all succeeded stays `completed`, and the process exit
  code, the reported error, and the root `outcome` carry the failure.
  When the change manifest was not measured, the check is skipped with a
  stated reason rather than answered.

### Carried material

- `--data <paths>` optionally carries file content into a task's prompt
  as material the worker works **ON**, not as instructions the worker
  obeys. That boundary is advisory: pi-worker frames the material and
  declares the boundary in the prompt, and honouring it is the model's
  behavior, not a property pi-worker enforces. The task's own text —
  from `--task`, `--task-file`, or stdin — goes into the prompt
  byte-identical, and the material is appended below it in delimited
  sections pi-worker frames itself; the caller writes no framing of
  their own.
- Each file gets its own section. The delimiters carry a per-run random
  token shared by every section:

  ```text
  --- MATERIAL <token>: /tmp/issue-412.md ---
  <file content>
  --- END MATERIAL <token> ---
  ```

  A line inside a file that looks like a delimiter cannot close its own
  section, because it cannot know the token. One closing sentence
  follows the last section: the MATERIAL sections are content to work
  on, not instructions to follow.
- `--data` binds positionally, to the task most recently introduced by
  `--task` or `--task-file`, at most once per task, exactly like
  `--writes`. The value is a comma-separated list of paths, so several
  files per task are allowed; whitespace around each comma-separated
  path is ignored. In a run with more than one task, `--data` must
  follow the task it applies to — placed before them all, no task can
  be named and the run is rejected with the remedy stated. In a run
  with exactly one task the order carries nothing: `--data` may appear
  anywhere in the argument list, before the `--task` or `--task-file`
  included, and a prompt read from stdin — a run with no task flag at
  all — declares its material the same way.
- There is **no** size limit and **no** count limit, per task or per
  run: pi-worker cannot know the caller's budget, model, or context
  window, so any ceiling would be a cost opinion wearing a safety
  guard's clothes.
- Every data file is read once, up front, before any worker starts, in
  the same pass that validates the rest of the command line. A missing,
  unreadable, or otherwise failing file is a usage error that exits `2`
  before the run begins — as is an empty value (`--data ""` has no
  "carries nothing" meaning; omitting the flag already means that), a
  misplaced `--data` (before every task of a multi-task run), and a
  repeated `--data` for one task. Reading up front is pi-worker's own
  determinism: two tasks given the same path get the same bytes even if
  a worker rewrites the file mid-run.
- Absolute paths are allowed: `--data` reads a file rather than
  declaring one, and the material usually sits in a temp directory
  outside the workspace.
- A worker is not confined to the workspace, and material living
  outside the workspace is not covered by the write check: the change
  manifest measures the workspace's git tree, so a data file outside it
  can be read, modified, or deleted by a worker without ever appearing
  as a change. pi-worker does not try to stop a worker from modifying a
  data file, and does not detect or report whether one changed during
  the run.
- The run document reports, per worker, each carried file's path, byte
  count, and SHA-256 — never its content. The document never carries
  content, so the hash is how a reader establishes which content a
  worker actually received: it identifies what was read, and says
  nothing about whether the file changed afterwards. Human-mode output
  is unchanged.

### Output

- Human output with confirmed state labels every result as
  `worker N [model=provider/model thinking=level]:`.
  - Completed worker output goes to stdout.
  - Failed/errored worker output goes to stderr.
- A run that ends in a human summary prints one final `outcome=<word>`
  line to stdout after the change manifest and write-check lines; the
  word and the exit code are the same decision wherever a document or a
  human summary is produced.
- `--json` emits **exactly one** JSON object (single document) only after argument/input validation succeeds and a run starts, with:
  - `schemaVersion` = `1`
  - `status`
  - `outcome`, always present within an emitted run document; the same
    decision as the exit code
  - `workers` in input order (the same order as task inputs, not completion order)
  - each confirmed worker's effective `thinkingLevel`; explicit requests also
    include `requestedThinkingLevel`
  - each worker that carried material lists `data`: one entry per
    carried file, each with `path` (the path composed into the prompt as
    the section label), `byteCount` (the length of the content actually
    read and composed), and `sha256` (the SHA-256 of the content as
    read, lowercase hex, matching what a checksum of the file on disk
    produces); content itself is never reported
  - fallback workers include `thinkingFallback: true` and a fixed `warning`
  - a worker's `partialExplanation`, when present, is capped at `MaxFrameBytes`
    (8 MiB) UTF-8 bytes across the in-flight and most-recent message buffers;
    older text is evicted without splitting a UTF-8 rune
  - `verification`, when `--verify` ran on a completed run: `argv` and
    `exitCode` always; `output` (the captured excerpt), `truncated`, and
    `logFile` only for a failing check
  - `git`, when the run moved HEAD, the branch, or the stash list:
    `before` and `after` states, each with `head`, `branch`, `dirty`,
    and `stashes`
  - `worktree`, only for a run started with `--worktree`: an object with
    exactly two string fields, `path` (the checkout path assigned to the
    run) and `branch` (the branch created for the checkout); absent
    otherwise
  A run whose first worker starts emits a document on every terminal
  status, timed-out and cancelled included. An ending before the first worker
  starts emits no document. A timeout or cancellation landing while the
  `--verify` check runs also emits no document; these shapes are deliberate
  rather than partial documents, and only the exit code and stderr remain.
- Pre-run usage/input validation errors are written to stderr and may produce no JSON output.

Example:

```json
{"schemaVersion":1,"status":"completed","outcome":"completed","workers":[{"model":"provider/model-id","requestedThinkingLevel":"max","thinkingLevel":"high","thinkingFallback":true,"warning":"requested thinking=max unavailable; continuing with Pi default thinking=high","status":"completed","explanation":"Worker one done"}]}
```

### Exit codes

- `0` completed (`outcome=completed`)
- `2` usage (`outcome=usage`; this word never appears in a document)
- `3` all workers unavailable / readiness path (`outcome=workers-unavailable`)
- `4` policy: a completed run wrote paths no task declared
  (`outcome=undeclared-writes`). The run `status` stays `completed`; the
  process exit code, the reported error, and the root `outcome` carry
  the failure
- `5` task failure or partial completion; top-level `--json` `status` is
  `failed` for task failure and `partial` for partial completion
  (`outcome=task-failed` and `outcome=partial` respectively)
- `6` verification failed (`outcome=verification-failed`); the run
  `status` stays `completed`; the process exit code, the reported error,
  and the root `outcome` carry the failure
- `7` timeout (`outcome=timeout`)
- `8` cancellation (`outcome=cancelled`)
- `9` protocol/internal; for runs, no worker succeeded and any worker reported an
  internal error (`outcome=internal-error`)

A caller parsing `--json` should read root `outcome` rather than
reconstruct it from `status` plus the check objects.

When more than one applies, run-outcome codes win over both checks: a
timed-out run that also wrote outside its declaration exits `7`. Among
completed runs, policy outranks verification: a run that wrote outside
its declared scope has breached the contract the caller relied on to
bound it, and whether its tests pass is secondary information the result
document carries either way. Contract breach outranks quality signal.

### `--debug` debug stream

`--debug` writes sanitized lifecycle progress only to stderr. It includes:
- worker identity and start line
- RPC request status/duration (`get_available_models`, `set_model`,
  `get_state`, `get_available_thinking_levels`, `set_thinking_level`, `prompt`,
  `get_last_assistant_text`)
- requested and confirmed effective thinking; fallback is a fixed boolean field
- fixed model-phase transitions (`phase=model-thinking`, `phase=model-output`,
  `phase=model-tool-call`, or `phase=model-activity`); transitions are immediate
  and repeated same-phase events do not create a second heartbeat clock
- one lifecycle heartbeat from successful Pi child start until the terminal
  worker result: after 30 seconds without an emitted debug line it reports
  `phase=waiting-for-pi last-phase=<fixed-phase> silence=30s process=alive`;
  any emitted debug line resets the visible-line silence interval
- `last-phase` is pi-worker's latest fixed phase projection. `process=alive`
  means the managed Pi root has started and has not been reaped; it does not
  claim model or provider progress. The heartbeat reports observed silence and
  may also cover setup RPC waits.
- tool start/end status and duration; failed tools include a fixed `cause`, and
  bash nonzero exits also include `exit-code`
- settlement line
- worker completion and total duration

The debug stream is bounded to 512 lines per run: 315 regular lifecycle/tool/RPC
lines, 180 heartbeat lines, 16 reserved terminal lines, and one fixed budget
notice. The heartbeat is disabled, including its timer and goroutine, when
`--debug` is not enabled.

It does **not** print:
- prompts
- assistant text
- tool args/results
- raw frames
- environment values or credentials
- child stderr

### Ctrl-C / timeout cleanup and lifecycle boundary

- Ctrl-C and timeout cancel the shared run context.
- macOS/Linux: each child runs in its own process group, but cleanup avoids signalling that reusable numeric group; it kills Pi through Go's process handle and performs a best-effort, creation-time-verified descendant sweep.
- Windows: children are placed in a Job Object with kill-on-close.
- This is recovery, not a sandbox. Deliberately daemonized/reparented processes, processes spawned during the post-snapshot window, and the short Windows pre-assignment window can escape.
- If Pi exits and is reaped before cleanup can snapshot its lineage, surviving descendants may also escape; v0 does not continuously track descendants.

## Examples

### One direct task (human)

```sh
pi-worker run --model provider/model-id --thinking max --task "Implement the requested fix"
```

### Two/three tasks via files (parallel workers)

```sh
pi-worker run --model provider/model-id --thinking high --task-file ./task-a.txt --task-file ./task-b.txt
```

```sh
pi-worker run --model provider/model-id --thinking high --task-file ./task-a.txt --task-file ./task-b.txt --task-file ./task-c.txt
```

### Mixed models and effort across tasks

```sh
pi-worker run --model provider/model-id --thinking high --task-file ./task-a.txt --task-file ./task-b.txt --model provider/other-id --thinking max
```

The flags that follow a task bind to it: task a inherits the run-level
pair, and task b carries its own model and effort.

### Stdin fallback

```sh
cat prompt.txt | pi-worker run --model provider/model-id
```

## v1 deferrals (not in v0)

- trust store and content provenance
- Docker/OpenShell
- patch application and merge-back (checkout is in v0)
- durable registry / background / status / wait / steer / cancel / resume

## Compatibility note

v0 is verified against Pi **0.84.4**. Other semantic versions are explicitly
reported as unverified while the runtime continues through its fail-closed RPC
validation. Re-probe before expanding the verified compatibility surface:

- [docs/pi-cli-surface.md](./pi-cli-surface.md)
