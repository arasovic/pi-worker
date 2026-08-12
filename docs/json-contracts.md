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
