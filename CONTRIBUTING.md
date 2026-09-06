# Contributing

## Prerequisites

- Go using the language and toolchain versions declared in `go.mod` (Go 1.25 and toolchain Go 1.26.1).
- Node.js >=22.20.0 and npm.
- Pi 0.85.1 only when relevant to integration or dogfood testing.

## Workflow

Fork the repository and create a purpose-named branch such as `fix/...` or
`docs/...`. Make focused changes. Use English for code, comments,
documentation, and commit messages.

Run checks appropriate to the changed surface:

- `npm run verify` covers formatting (`gofmt`), vetting (`go vet ./...`), the Go suite with race detection (`go test -race -count=1 ./...`), the JavaScript suite (`npm test`), and the four project checks (`check:rules`, `check:notices`, `check:piversion`, `check:hygiene`).
- Focused per-check commands: `npm run check:gofmt`, `npm run check:govet`, `npm run check:gotest`, `npm run check:rules`, `npm run check:notices`, `npm run check:piversion`, and `npm run check:hygiene`.
- `verify` does not cover `go build ./...`, which CI runs, or `git diff --check`, which you run before every commit.

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
- `skills`: the pin is declared twice — `PINNED_SKILLS_VERSION` in
  `npm/lib/skill-rules.mjs` (JavaScript) and `PinnedSkillsVersion` in
  `internal/skillinstall/receipt.go` (Go) — and a Dependabot bump of the npm
  dependency lands red until both move together. The companion commit updates
  the two constants, writes the measured
  `EXPECTED_AGENT_COUNT`, `EXPECTED_GLOBAL_TARGET_COUNT`, and
  `EXPECTED_NO_GLOBAL_TARGET_COUNT` in `npm/lib/skill-rules.mjs`, updates the
  `skills@…` prose in `README.md` and `docs/v0-usage.md`, and regenerates the
  bundle with `node npm/scripts/extract-skills-rules.mjs --write
  npm/generated/skills-rules.json`. The counts are not to be guessed: the
  generator prints the measured values in its
  `rule count mismatch (agents=…, global=…, no-global=…)` error, and those are
  what gets written. If the `engines.node` derivation assertion in
  `npm/test/hygiene.test.mjs` goes red, the package's Node floor has moved with
  the dependency: read the new floor from the installed `skills` package and
  write the same number in `package.json`, `README.md`, `docs/v0-usage.md`,
  `docs/releasing.md`, and the literal in `npm/test/readme.test.mjs`.
- Pi: the pin lives in `compat/pi/package.json`, which Dependabot watches
  weekly. The coupled artifacts are `internal/piversion/version.go` and the
  prose mentions, and `npm run check:piversion` is what goes red. The
  companion commit is not just running `--write`: `VerifiedVersion` claims a
  version was verified, and a bot bumping the pin verifies nothing. Exercise
  the new Pi release and re-probe the surface first, then run
  `go run ./tools/piversion --write`. Running `--write` on its own converts
  an unverified bump into a false claim of verification.
  `docs/pi-cli-surface.md` is deliberately not rewritten by the tool: it
  records what was actually observed when the surface was probed, not claims
  about the current pin, and rewriting it would falsify history.

The red run is the intended signal, not a failure to work around. Do not relax
a check to make the update merge.

Security reports follow SECURITY.md, not public issues.
