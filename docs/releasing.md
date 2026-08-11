# Release snapshot runbook

This project currently supports a local, reproducible snapshot flow only.
Do not use these steps to publish.

## Requirements

- macOS or Linux.
- Node.js 22.20.0 or newer and npm.
- Go 1.26.1; the module language baseline is Go 1.25.0.
- A clean Git checkout.

## Local snapshot commands

From a clean checkout:

```sh
git status --short
npm ci --ignore-scripts
npm run verify

export RELEASE_VERSION=v0.1.0
export RELEASE_COMMIT="$(git rev-parse HEAD)"
export RELEASE_TIMESTAMP="$(git show -s --format=%ct "$RELEASE_COMMIT")"
export RELEASE_BUILD_DATE="$(node -e 'process.stdout.write(new Date(Number(process.argv[1]) * 1000).toISOString().replace(".000Z", "Z"))' "$RELEASE_TIMESTAMP")"

go run ./tools/release \
  --version "$RELEASE_VERSION" \
  --commit "$RELEASE_COMMIT" \
  --build-date "$RELEASE_BUILD_DATE" \
  --output dist

npm run stage -- --dist dist
PI_WORKER_ASSERT_STAGED=1 node --test --test-name-pattern='current checkout npm pack' npm/test/package.test.mjs
npm pack --json | tee dist/npm-pack.json
export NPM_TARBALL="$(node -p "JSON.parse(require('fs').readFileSync('dist/npm-pack.json','utf8'))[0].filename")"
test "$NPM_TARBALL" = "pi-worker-0.0.0-private.tgz"
mv "$NPM_TARBALL" "dist/$NPM_TARBALL"
```

## Exact artifact inventory

- `dist/pi-worker_v0.1.0_darwin_arm64.tar.gz`
- `dist/pi-worker_v0.1.0_darwin_amd64.tar.gz`
- `dist/pi-worker_v0.1.0_linux_arm64.tar.gz`
- `dist/pi-worker_v0.1.0_linux_amd64.tar.gz`
- `dist/checksums.txt`
- `dist/pi-worker-0.0.0-private.tgz`
- `dist/npm-pack.json`

## Checksum verification

macOS:

```sh
(cd dist && shasum -a 256 -c checksums.txt)
```

Linux:

```sh
(cd dist && sha256sum -c checksums.txt)
```

## Package inventory inspection and private guard

```sh
tar -tzf dist/pi-worker-0.0.0-private.tgz
node -e "const fs=require('fs');const data=JSON.parse(fs.readFileSync('dist/npm-pack.json','utf8'));if(data.length!==1||!data[0].filename){process.exit(1)}"
node -e "const fs=require('fs');const manifest=JSON.parse(fs.readFileSync('package.json','utf8'));if(!manifest.private){process.exit(1);}"
npm pack --dry-run --json
```

Inspect the package entry list and ensure expected release contents, then confirm `"private": true` before any publication step.

## Stop before branding and publication

This runbook is a snapshot gate only. Do not run a publication command.
The first future npm publication requires a user-approved single-use granular bootstrap
credential with provenance; then configure trusted publishing, and revoke that bootstrap
credential immediately.
