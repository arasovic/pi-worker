# Contributing

## Prerequisites

- Go using the language and toolchain versions declared in `go.mod` (Go 1.25 and toolchain Go 1.26.1).
- Node.js >=22.20.0 and npm.
- Pi 0.84.1 only when relevant to integration or dogfood testing.

## Workflow

Fork the repository and create a purpose-named branch such as `fix/...` or
`docs/...`. Make focused changes. Use English for code, comments,
documentation, and commit messages.

Run checks appropriate to the changed surface:

- Go: `gofmt` formatting, `go vet ./...`, `go test -race -count=1 ./...`, and `go build ./...`.
- npm: `npm test` and `npm run verify`; focused checks are `npm run check:rules`, `npm run check:notices`, and `npm run check:hygiene`.
- All changes: `git diff --check`.

The hygiene command is a narrow accidental-artifact gate, not a general secret scanner. Inspect staged changes before every commit.

Integration tests start workers that can execute `bash` with host-user
permissions in the current writable workspace; workers have no sandbox. Use a
trusted workspace and keep parallel file ownership disjoint.

Do not commit dist, npm/native, tgz files, credentials, Pi profiles, provider configuration, prompts, workspace contents, or generated local artifacts.

## Dependency updates

Every dependency is also pinned somewhere the version resolver does not reach,
so a Dependabot pull request lands red by design and needs one companion commit
on its own branch:

- Go modules: THIRD_PARTY_NOTICES is rendered from `fixedInventory` in
  `internal/releasenotice/notices.go`, not from `go.mod`, so regenerating alone
  is a no-op. Edit the versions in `fixedInventory` first, then replace the same
  versions in `internal/releasenotice/notices_test.go`, whose fixture module
  cache is keyed by `module@version`, and only then run
  `go run ./tools/notices --write THIRD_PARTY_NOTICES`.
  `TestInventoryMatchesTargetDependencyUnion` compares the inventory against the
  real module graph for every release target, so transitive bumps must be
  carried too: updating gopsutil also moves purego.

  After changing modules, run `go mod download all` and commit any resulting
  `go.sum` change. `go mod tidy` alone is not sufficient: the
  `release-snapshot` job runs `go mod download all` on a clean checkout, which
  re-adds `go.sum` entries that `go mod tidy` pruned, and `tools/release`
  refuses to build a snapshot from a dirty tree. That job fails with
  `working tree has uncommitted changes`.
- GitHub Actions: update the expected reference in `npm/test/hygiene.test.mjs`,
  which asserts the exact action versions the release workflows use. Read the
  action's release notes first: these pins guard the publication path.
- `skills`: run `node npm/scripts/extract-skills-rules.mjs --write npm/generated/skills-rules.json`.
  The generated rules record the version they were extracted from, and
  `check:rules` rejects a mismatch.

The red run is the intended signal, not a failure to work around. Do not relax
a check to make the update merge.

Security reports follow SECURITY.md, not public issues.
