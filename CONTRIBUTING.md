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
- npm: `npm test` and `npm run verify` when available; run the rules and notices checks when relevant (`npm run skills-rules:check` and `npm run notices:check`).
- All changes: `git diff --check`.

Integration tests start workers that can execute `bash` with host-user
permissions in the current writable workspace; workers have no sandbox. Use a
trusted workspace and keep parallel file ownership disjoint.

Do not commit dist, npm/native, tgz files, credentials, Pi profiles, provider configuration, prompts, workspace contents, or generated local artifacts.

Security reports follow SECURITY.md, not public issues.
