# Versioning

From 1.0.0, Pi Worker follows [Semantic Versioning](https://semver.org/).
This page lists what a version number promises: anything listed as
covered changes incompatibly only in a new major version.

## Covered

- **Commands and flags.** Command names, flag names, what each flag means,
  and the argument forms documented in [detailed usage](./usage.md) and
  `--help`.
- **Exit codes.** Each code and the outcome it stands for. A new exit code
  or a new `outcome` value is a major change, because a caller that
  branches on the full set would misread it.
- **JSON documents.** Every `--json` document follows the rules in
  [JSON contracts](./json-contracts.md). Removing a field, changing its
  type or the meaning of an enum value, or making a required field
  optional needs a new `schemaVersion`, and a new `schemaVersion` of a
  document is a major change. A new optional field is not.
- **The configuration file.** A release reads the configuration the
  previous release wrote, or migrates it. Schema 1 configurations are
  read as schema 2 today.
- **Platforms.** macOS and Linux on arm64 and x64, with Node.js 22.20 or
  newer for the npm package. Dropping a platform or raising the minimum
  Node.js major version is a major change.

## Not covered

These can change in any release:

- Human-readable output: tables, stderr lines, and their wording. Machine
  consumers read `--json`.
- The wording of free-text JSON fields such as `error`, `warning`, and
  `explanation`. Their presence and meaning are covered; the sentence is
  not.
- `--debug` lines.
- The files in the state directory, such as `runs/<id>/` and what is in
  it. Read runs through the `runs` commands. The `transcript` path in a
  run result points to a file Pi writes, in Pi's format.
- Go packages under `internal/`. Pi Worker has no Go API.

## Minor and patch releases

- A **minor** release adds something: a command, a flag, an optional JSON
  field, a newly verified Pi version.
- A **patch** release fixes something. A fix that brings behaviour back to
  what the documentation says is a patch, even when its output changes.

## Pi

Each release names the Pi version it verified, in
[the Pi compatibility surface](./pi-cli-surface.md). Other Pi versions
still run, with a warning. Moving to a newly verified Pi version is a
minor release unless it changes something covered above.

## Deprecation

Something covered is removed only in a major release. At least one minor
release before that marks it deprecated in the documentation and `--help`,
and prints a warning on stderr when it is used, where that is possible.
