# Release runbook

The local snapshot gate is safe to run without remote access. The publication
sections require explicit authorization because they change GitHub and npm.

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

export PACKAGE_VERSION="$(node -p "require('./package.json').version")"
export RELEASE_VERSION="v$PACKAGE_VERSION"
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
test "$NPM_TARBALL" = "pi-worker-$PACKAGE_VERSION.tgz"
mv "$NPM_TARBALL" "dist/$NPM_TARBALL"
```

## Exact artifact inventory

The four native archives use the archive prefix `pi-worker_${RELEASE_VERSION}_`
followed by the target tuple, and the npm tarball uses
`pi-worker-${PACKAGE_VERSION}.tgz`. `$RELEASE_VERSION` and `$PACKAGE_VERSION`
are exactly the variables the Local snapshot commands block exports.

- `dist/pi-worker_${RELEASE_VERSION}_darwin_arm64.tar.gz`
- `dist/pi-worker_${RELEASE_VERSION}_darwin_amd64.tar.gz`
- `dist/pi-worker_${RELEASE_VERSION}_linux_arm64.tar.gz`
- `dist/pi-worker_${RELEASE_VERSION}_linux_amd64.tar.gz`
- `dist/checksums.txt`
- `dist/pi-worker-${PACKAGE_VERSION}.tgz`
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

## Package inventory and public metadata inspection

```sh
tar -tzf dist/pi-worker-$PACKAGE_VERSION.tgz
node -e "const fs=require('fs');const data=JSON.parse(fs.readFileSync('dist/npm-pack.json','utf8'));if(data.length!==1||!data[0].filename){process.exit(1)}"
node -e "const fs=require('fs');const p=JSON.parse(fs.readFileSync('package.json','utf8'));if(p.name!=='pi-worker'||p.version!=='$PACKAGE_VERSION'||p.private===true||!p.repository.url.endsWith('github.com/arasovic/pi-worker.git')){process.exit(1)}"
npm pack --dry-run --json
```

Inspect the package entry list and confirm the public name, version, repository,
license, homepage, and issue URL before any publication step.

## Subsequent releases

1. Update `package.json` to the intended version and complete the local snapshot
   gate.
2. Push the release commit to public `main`.
3. Create and push the exact matching `vX.Y.Z` tag.
4. Verify the `Release` workflow publishes through OIDC and attaches the four
   native archives plus `checksums.txt` to the GitHub Release.

The tag, package version, Go release version, npm tarball name, and native
archive prefix are derived and checked as one release identity before builds or
publication begin.

## Publishing access

After the OIDC release succeeds, set package publishing access to require 2FA
and disallow traditional tokens if that policy fits the account.
