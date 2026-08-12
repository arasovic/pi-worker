import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("../scripts/release-metadata.mjs", import.meta.url));

function run(t, { tag = "v0.1.0", refType = "tag", version = "0.1.0" } = {}) {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-release-metadata-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const manifest = join(root, "package.json");
  const output = join(root, "github-output");
  writeFileSync(manifest, `${JSON.stringify({ name: "pi-worker", version })}\n`);
  const result = spawnSync(process.execPath, [script, "--tag", tag, "--ref-type", refType, "--package-json", manifest, "--github-output", output], {
    encoding: "utf8",
  });
  return { result, output };
}

test("emits one canonical release identity from a matching tag and package version", (t) => {
  const { result, output } = run(t);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(readFileSync(output, "utf8"), [
    "tag=v0.1.0",
    "version=0.1.0",
    "release_version=v0.1.0",
    "npm_tarball=pi-worker-0.1.0.tgz",
    "archive_prefix=pi-worker_v0.1.0_",
    "",
  ].join("\n"));
});

test("rejects branch dispatches, unstable versions, and tag-package mismatches", (t) => {
  for (const fixture of [
    { refType: "branch" },
    { tag: "v0.1.1" },
    { tag: "0.1.0" },
    { version: "0.1.1" },
    { tag: "v0.2.0-rc.1", version: "0.2.0-rc.1" },
    { tag: "v0.2.0+build.1", version: "0.2.0+build.1" },
  ]) {
    const { result } = run(t, fixture);
    assert.notEqual(result.status, 0, JSON.stringify(fixture));
  }
});
