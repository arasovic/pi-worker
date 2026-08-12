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
test "$NPM_TARBALL" = "pi-worker-0.1.0.tgz"
mv "$NPM_TARBALL" "dist/$NPM_TARBALL"
```

## Exact artifact inventory

- `dist/pi-worker_v0.1.0_darwin_arm64.tar.gz`
- `dist/pi-worker_v0.1.0_darwin_amd64.tar.gz`
- `dist/pi-worker_v0.1.0_linux_arm64.tar.gz`
- `dist/pi-worker_v0.1.0_linux_amd64.tar.gz`
- `dist/checksums.txt`
- `dist/pi-worker-0.1.0.tgz`
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
tar -tzf dist/pi-worker-0.1.0.tgz
node -e "const fs=require('fs');const data=JSON.parse(fs.readFileSync('dist/npm-pack.json','utf8'));if(data.length!==1||!data[0].filename){process.exit(1)}"
node -e "const fs=require('fs');const p=JSON.parse(fs.readFileSync('package.json','utf8'));if(p.name!=='pi-worker'||p.version!=='0.1.0'||p.private===true||!p.repository.url.endsWith('github.com/arasovic/pi-worker.git')){process.exit(1)}"
npm pack --dry-run --json
```

Inspect the package entry list and confirm the public name, version, repository,
license, homepage, and issue URL before any publication step.

## Required publication order

The README image is served from the public repository. A future authorized
release must use this order:

1. Create the GitHub repository and push `main`.
2. Make the repository public if it was created privately; the repository must
   be public before npm publication and provenance generation.
3. Verify the README image loads without authentication from its
   `raw.githubusercontent.com` URL.
4. Only then publish the npm package.

Do not publish while the repository is private or before the pushed `main`
contains `assets/brand/github-social-preview.png`.

## Initial npm publication

The package must exist before npm exposes its trusted-publisher settings. The
initial `v0.1.0` publication therefore uses the isolated
`.github/workflows/bootstrap-publish.yml` workflow. The permanent
`.github/workflows/release.yml` never reads an npm token.

1. Create a granular npm token with all-packages read/write access, the shortest
   practical expiry, and `bypass 2FA` if the account requires 2FA for writes.
   The new unscoped package cannot yet be selected as the token target, so keep
   this broader credential alive only for the bootstrap run.
2. Add it to the GitHub repository as the `NPM_BOOTSTRAP_TOKEN` Actions secret.
3. Confirm `package.json` is version `0.1.0`, create tag `v0.1.0`, and push the
   tag. The permanent release job intentionally skips this one bootstrap tag.
4. Run `Bootstrap npm Publish` with the `v0.1.0` tag selected as the workflow
   ref. A branch selection is rejected.
5. Verify npm shows `pi-worker@0.1.0` with provenance and the GitHub Release has
   four native archives plus `checksums.txt`.
6. In the npm package settings, configure the GitHub Actions trusted publisher
   for owner `arasovic`, repository `pi-worker`, workflow `release.yml`, and the
   `npm publish` action.
7. Revoke the granular token immediately and delete the
   `NPM_BOOTSTRAP_TOKEN` repository secret.
8. Delete `.github/workflows/bootstrap-publish.yml` and its bootstrap-only test
   assertions, then remove the `github.ref_name != 'v0.1.0'` bootstrap guard
   from `release.yml` in the next commit. Do not add token authentication to
   `release.yml`.

After the OIDC release succeeds, set package publishing access to require 2FA
and disallow traditional tokens if that policy fits the account.

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
