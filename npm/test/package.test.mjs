import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { gunzipSync, gzipSync } from "node:zlib";
import {
  chmodSync,
  cpSync,
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  symlinkSync,
  truncateSync,
  writeFileSync,
} from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const repository = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const targets = [
  ["darwin_arm64", "darwin-arm64"],
  ["darwin_amd64", "darwin-x64"],
  ["linux_arm64", "linux-arm64"],
  ["linux_amd64", "linux-x64"],
];

function tarHeader(name, size, mode = 0o644, type = "0", linkName = "", prefix = "") {
  const header = Buffer.alloc(512);
  const put = (offset, length, value) => header.write(String(value), offset, length, "utf8");
  put(0, 100, name);
  put(100, 8, `${mode.toString(8).padStart(7, "0")}\0`);
  put(108, 8, "0000000\0");
  put(116, 8, "0000000\0");
  put(124, 12, `${size.toString(8).padStart(11, "0")}\0`);
  put(136, 12, "00000000000\0");
  header.fill(0x20, 148, 156);
  put(156, 1, type);
  put(157, 100, linkName);
  put(257, 6, "ustar\0");
  put(345, 155, prefix);
  put(263, 2, "00");
  const checksum = [...header].reduce((sum, byte) => sum + byte, 0);
  put(148, 8, `${checksum.toString(8).padStart(6, "0")}\0 `);
  return header;
}

function makeArchive(entries) {
  const chunks = [];
  for (const entry of entries) {
    const data = Buffer.from(entry.data ?? "", "utf8");
    chunks.push(tarHeader(entry.name, data.length, entry.mode ?? 0o644, entry.type ?? "0", entry.linkName, entry.prefix));
    chunks.push(data, Buffer.alloc((512 - (data.length % 512)) % 512));
  }
  chunks.push(Buffer.alloc(1024));
  return gzipSync(Buffer.concat(chunks), { mtime: 0 });
}

function releaseEntries(entries = [{ name: "LICENSE", data: "license\n" }, { name: "THIRD_PARTY_NOTICES", data: "notice\n" }, { name: "pi-worker", data: "binary\n", mode: 0o755 }]) {
  return entries;
}

function readPackedEntries(path) {
  const tar = requireGunzip(readFileSync(path));
  const entries = new Map();
  for (let offset = 0; offset < tar.length;) {
    const header = tar.subarray(offset, offset + 512);
    if (header.every((byte) => byte === 0)) break;
    const nameEnd = header.indexOf(0);
    const name = header.toString("utf8", 0, nameEnd < 0 ? 100 : nameEnd).replace(/^package\//, "");
    const sizeEnd = header.indexOf(0, 124);
    const size = Number.parseInt(header.toString("utf8", 124, sizeEnd < 0 ? 136 : sizeEnd).trim(), 8);
    const dataStart = offset + 512;
    entries.set(name, {
      mode: Number.parseInt(header.toString("utf8", 100, 108).replace(/\0/g, "").trim(), 8),
      data: tar.subarray(dataStart, dataStart + size),
    });
    offset = dataStart + Math.ceil(size / 512) * 512;
  }
  return entries;
}

function requireGunzip(bytes) {
  // Keep the fixture test dependency-free while using the same standard format npm writes.
  return gunzipSync(bytes);
}

function archiveBinary(bytes) {
  const tar = requireGunzip(bytes);
  let offset = 0;
  while (offset < tar.length) {
    const header = tar.subarray(offset, offset + 512);
    if (header.every((byte) => byte === 0)) break;
    const nameEnd = header.indexOf(0);
    const name = header.toString("utf8", 0, nameEnd < 0 ? 100 : nameEnd);
    const sizeEnd = header.indexOf(0, 124);
    const size = Number.parseInt(header.toString("utf8", 124, sizeEnd < 0 ? 136 : sizeEnd).trim(), 8);
    const dataStart = offset + 512;
    if (name === "pi-worker") return Buffer.from(tar.subarray(dataStart, dataStart + size));
    offset = dataStart + Math.ceil(size / 512) * 512;
  }
  throw new Error("fixture archive has no pi-worker entry");
}

function writeReleaseSet(root, overrides = {}, version = "v0.1.0") {
  const checksums = [];
  for (const [suffix] of targets) {
    const name = `pi-worker_${version}_${suffix}.tar.gz`;
    const bytes = makeArchive(overrides[suffix] ?? releaseEntries());
    writeFileSync(join(root, name), bytes);
    checksums.push(`${createHash("sha256").update(bytes).digest("hex")}  ${name}`);
  }
  writeFileSync(join(root, "checksums.txt"), `${checksums.join("\n")}\n`);
}

function runStage(packageRoot, dist = join(packageRoot, "dist")) {
  return spawnSync(process.execPath, [join(packageRoot, "npm/scripts/stage-package.mjs"), "--dist", dist], {
    cwd: packageRoot,
    encoding: "utf8",
  });
}

function copyPackageFixture(root) {
  for (const path of ["package.json", "README.md", "CONTRIBUTING.md", "SECURITY.md", "LICENSE", "THIRD_PARTY_NOTICES", "skills", "npm/bin", "npm/generated", "npm/lib", "npm/scripts", "npm/test", "go.mod", "go.sum", "tools", "internal/releasenotice"]) {
    cpSync(join(repository, path), join(root, path), { recursive: true });
  }
  const initialized = spawnSync("git", ["init", "--quiet"], { cwd: root, encoding: "utf8" });
  assert.equal(initialized.status, 0, initialized.stderr);
  const tracked = spawnSync("git", ["add", "--all"], { cwd: root, encoding: "utf8" });
  assert.equal(tracked.status, 0, tracked.stderr);
  mkdirSync(join(root, "node_modules"));
  symlinkSync(join(repository, "node_modules/skills"), join(root, "node_modules/skills"), "dir");
}

const expectedFiles = [
  "CONTRIBUTING.md",
  "LICENSE",
  "README.md",
  "SECURITY.md",
  "THIRD_PARTY_NOTICES",
  "npm/bin/pi-worker.mjs",
  "npm/generated/skills-rules.json",
  "npm/lib/native.mjs",
  "npm/lib/skill-install.mjs",
  "npm/lib/skill-receipt.mjs",
  "npm/lib/skill-rules.mjs",
  "npm/lib/skill-tree.mjs",
  "npm/native/darwin-arm64/pi-worker",
  "npm/native/darwin-x64/pi-worker",
  "npm/native/linux-arm64/pi-worker",
  "npm/native/linux-x64/pi-worker",
  "npm/scripts/postinstall.mjs",
  "package.json",
  "skills/pi-worker/PI_WORKER_IDENTITY",
  "skills/pi-worker/SKILL.md",
  "skills/pi-worker/agents/openai.yaml",
];

test("stages exactly four checked release binaries into npm/native", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-test-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);

  const result = runStage(root, dist);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(readdirSync(join(root, "npm/native")).sort(), targets.map(([, dir]) => dir).sort());
  for (const [suffix, dir] of targets) {
    const binary = join(root, "npm/native", dir, "pi-worker");
    const archive = join(dist, `pi-worker_v0.1.0_${suffix}.tar.gz`);
    assert.equal(lstatSync(binary).isFile(), true);
    assert.equal(lstatSync(binary).mode & 0o777, 0o755);
    assert.deepEqual(readFileSync(binary), archiveBinary(readFileSync(archive)));
  }
  assert.equal(existsSync(join(root, "npm/.native-stage.lock")), false);
});

test("rejects unsafe or incomplete release input without touching native staging", (t) => {
  const cases = [
    ["checksum mismatch", (dist) => {
      writeReleaseSet(dist);
      const archive = join(dist, "pi-worker_v0.1.0_linux_amd64.tar.gz");
      writeFileSync(archive, makeArchive([
        { name: "LICENSE", data: "license\n" },
        { name: "THIRD_PARTY_NOTICES", data: "notice\n" },
        { name: "pi-worker", data: "different valid binary\n", mode: 0o755 },
      ]));
    }],
    ["symlinked archive path", (dist) => {
      writeReleaseSet(dist);
      const archive = join(dist, "pi-worker_v0.1.0_linux_amd64.tar.gz");
      const symlinkTarget = join(dist, "pi-worker_v0.1.0_linux_arm64.tar.gz");
      rmSync(archive);
      symlinkSync(symlinkTarget, archive);
    }],
    ["extra release file", (dist) => {
      writeReleaseSet(dist);
      writeFileSync(join(dist, "unexpected.tar.gz"), "extra");
    }],
    ["symlink archive entry", (dist) => {
      writeReleaseSet(dist, { linux_amd64: [{ name: "pi-worker", type: "2", linkName: "/tmp/escape" }] });
    }],
    ["metadata mode with extra bits", (dist) => {
      writeReleaseSet(dist, { linux_amd64: releaseEntries([
        { name: "LICENSE", data: "license\n", mode: 0o1644 },
        { name: "THIRD_PARTY_NOTICES", data: "notice\n" },
        { name: "pi-worker", data: "binary\n", mode: 0o755 },
      ]) });
    }],
    ["binary mode with extra bits", (dist) => {
      writeReleaseSet(dist, { linux_amd64: releaseEntries([
        { name: "LICENSE", data: "license\n" },
        { name: "THIRD_PARTY_NOTICES", data: "notice\n" },
        { name: "pi-worker", data: "binary\n", mode: 0o4755 },
      ]) });
    }],
  ];

  for (const [name, prepare] of cases) {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-package-failure-"));
    t.after(() => rmSync(root, { recursive: true, force: true }));
    copyPackageFixture(root);
    const dist = join(root, "dist");
    mkdirSync(dist);
    prepare(dist);
    const result = runStage(root, dist);
    assert.notEqual(result.status, 0, `${name} unexpectedly passed`);
    assert.equal(existsSync(join(root, "npm/native")), false, `${name} created native`);
    assert.deepEqual(readdirSync(join(root, "npm")).filter((entry) => entry.startsWith(".native-")), []);
    assert.equal(existsSync(join(root, "npm/.native-stage.lock")), false);
  }
});

test("rejects a non-empty USTAR prefix with a valid checksum", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-prefix-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist, {
    linux_amd64: releaseEntries([
      { name: "LICENSE", data: "license\n", prefix: "../outside" },
      { name: "THIRD_PARTY_NOTICES", data: "notice\n" },
      { name: "pi-worker", data: "binary\n", mode: 0o755 },
    ]),
  });

  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /tar prefix/);
  assert.equal(existsSync(join(root, "npm/native")), false);
  assert.deepEqual(readdirSync(join(root, "npm")).filter((entry) => entry.startsWith(".native-")), []);
  assert.equal(existsSync(join(root, "npm/.native-stage.lock")), false);
});

test("rejects oversized checksums before reading it", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-checksums-oversized-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);
  const oversized = Buffer.alloc(4 * 1024 + 1, "x");
  oversized[oversized.length - 1] = 0x0a;
  writeFileSync(join(dist, "checksums.txt"), oversized);

  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /checksums\.txt is too large/);
  assert.equal(existsSync(join(root, "npm/native")), false);
  assert.deepEqual(readdirSync(join(root, "npm")).filter((entry) => entry.startsWith(".native-")), []);
  assert.equal(existsSync(join(root, "npm/.native-stage.lock")), false);
});

test("rejects symlinked checksums without following it", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-checksums-symlink-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);
  const checksumPath = join(dist, "checksums.txt");
  const target = join(root, "checksums-target.txt");
  writeFileSync(target, readFileSync(checksumPath));
  rmSync(checksumPath);
  symlinkSync(target, checksumPath);

  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /checksums\.txt is not a regular file/);
  assert.equal(existsSync(join(root, "npm/native")), false);
  assert.deepEqual(readdirSync(join(root, "npm")).filter((entry) => entry.startsWith(".native-")), []);
  assert.equal(existsSync(join(root, "npm/.native-stage.lock")), false);
});

test("rejects a checksum-mismatched malformed archive before parsing", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-checksum-order-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);

  const archive = join(dist, "pi-worker_v0.1.0_linux_amd64.tar.gz");
  writeFileSync(archive, Buffer.from("not-gzip-data", "utf8"));

  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /checksum mismatch for pi-worker_v0.1.0_linux_amd64\.tar\.gz/);
  assert.equal(existsSync(join(root, "npm/native")), false);
  assert.deepEqual(readdirSync(join(root, "npm")).filter((entry) => entry.startsWith(".native-")), []);
});

test("requires release filenames to use releaseartifact SemVer", (t) => {
  for (const version of ["v01.2.3", "v1.2.3-01", "v1.2.3-", "v1.2.3+build+again"]) {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-package-version-"));
    t.after(() => rmSync(root, { recursive: true, force: true }));
    copyPackageFixture(root);
    const dist = join(root, "dist");
    mkdirSync(dist);
    writeReleaseSet(dist, {}, version);
    const result = runStage(root, dist);
    assert.notEqual(result.status, 0, `${version} unexpectedly passed`);
    assert.equal(existsSync(join(root, "npm/native")), false);
  }

  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-version-valid-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist, {}, "v1.2.3-alpha.1+build.7");
  const result = runStage(root, dist);
  assert.equal(result.status, 0, result.stderr);
});

test("rejects an oversized regular archive before reading it", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-oversized-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);
  const archive = join(dist, "pi-worker_v0.1.0_linux_amd64.tar.gz");
  truncateSync(archive, 64 * 1024 * 1024 + 1);
  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /too large|oversized/);
  assert.equal(existsSync(join(root, "npm/native")), false);
});

test("rejects a non-regular archive through the file handle", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-nonregular-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);
  const archive = join(dist, "pi-worker_v0.1.0_linux_amd64.tar.gz");
  rmSync(archive);
  mkdirSync(archive);
  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /not a regular file/);
  assert.equal(existsSync(join(root, "npm/native")), false);
});

test("bounds gzip expansion during decompression", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-expansion-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);
  const archiveName = "pi-worker_v0.1.0_linux_amd64.tar.gz";
  const expanded = makeArchive([
    { name: "LICENSE", data: "license\n" },
    { name: "THIRD_PARTY_NOTICES", data: "notice\n" },
    { name: "pi-worker", data: Buffer.alloc(64 * 1024 * 1024 - 1, 0), mode: 0o755 },
  ]);
  writeFileSync(join(dist, archiveName), expanded);
  const checksums = readFileSync(join(dist, "checksums.txt"), "utf8").trimEnd().split("\n");
  const replacement = `${createHash("sha256").update(expanded).digest("hex")}  ${archiveName}`;
  writeFileSync(join(dist, "checksums.txt"), `${checksums.map((line) => line.endsWith(archiveName) ? replacement : line).join("\n")}\n`);

  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /not valid gzip|malformed tar/);
  assert.equal(existsSync(join(root, "npm/native")), false);
});

test("refuses a pre-existing staging lock without removing it", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-lock-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);
  const lock = join(root, "npm/.native-stage.lock");
  mkdirSync(lock);

  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.equal(existsSync(lock), true);
  assert.equal(existsSync(join(root, "npm/native")), false);
});

test("never replaces a pre-existing npm/native directory", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-stale-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);
  const native = join(root, "npm/native");
  mkdirSync(native, { recursive: true });
  writeFileSync(join(native, "stale"), "keep me");

  const result = runStage(root, dist);
  assert.notEqual(result.status, 0);
  assert.equal(readFileSync(join(native, "stale"), "utf8"), "keep me");
});

test("prepack executes the configured notice, skill-rule, and hygiene checks", (t) => {
  const manifest = JSON.parse(readFileSync(join(repository, "package.json"), "utf8"));
  assert.equal(manifest.scripts.prepack, "npm run check:notices && npm run check:rules && npm run check:hygiene");

  const cases = [
    ["notice", (root) => writeFileSync(join(root, "THIRD_PARTY_NOTICES"), "controlled failure\n")],
    ["skills rules", (root) => writeFileSync(join(root, "npm/generated/skills-rules.json"), "{}\n")],
    ["hygiene", (root) => writeFileSync(join(root, "README.md"), `private ${["/Us", "ers/example"].join("")}\n`)],
  ];
  for (const [name, corrupt] of cases) {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-package-prepack-"));
    t.after(() => rmSync(root, { recursive: true, force: true }));
    copyPackageFixture(root);
    corrupt(root);
    const result = spawnSync("npm", ["pack", "--json"], { cwd: root, encoding: "utf8" });
    assert.notEqual(result.status, 0, `${name} failure was bypassed`);
  }
});

test("manifest allowlist includes all ignored binaries and excludes npm/test", () => {
  const manifest = JSON.parse(readFileSync(join(repository, "package.json"), "utf8"));
  assert.ok(manifest.files.includes("npm/native/"));
  for (const nativePath of expectedFiles.filter((path) => path.startsWith("npm/native/"))) {
    assert.ok(manifest.files.some((entry) => nativePath === entry || nativePath.startsWith(`${entry.replace(/\/$/, "")}/`)), nativePath);
  }
  assert.ok(manifest.files.every((entry) => !entry.startsWith("npm/test")));
});

test("manifest exposes one non-duplicated local verification pipeline", () => {
  const manifest = JSON.parse(readFileSync(join(repository, "package.json"), "utf8"));
  assert.equal(manifest.scripts["check:rules"], "node npm/scripts/extract-skills-rules.mjs --check npm/generated/skills-rules.json");
  assert.equal(manifest.scripts["check:notices"], "go run ./tools/notices --check THIRD_PARTY_NOTICES");
  assert.equal(manifest.scripts["check:hygiene"], "node npm/scripts/check-hygiene.mjs");
  assert.match(manifest.scripts.test, /npm\/test\/\*\.test\.mjs/);
  assert.equal(manifest.scripts.verify, "npm test && npm run check:rules && npm run check:notices && npm run check:hygiene");
});

test("npm pack has the exact allowlisted inventory, modes, and identity bytes", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-package-pack-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyPackageFixture(root);
  const dist = join(root, "dist");
  mkdirSync(dist);
  writeReleaseSet(dist);
  const staged = runStage(root, dist);
  assert.equal(staged.status, 0, staged.stderr);

  const packed = spawnSync("npm", ["pack", "--json", "--ignore-scripts"], { cwd: root, encoding: "utf8" });
  assert.equal(packed.status, 0, packed.stderr);
  const metadata = JSON.parse(packed.stdout)[0];
  assert.deepEqual(metadata.files.map((file) => file.path).sort(), expectedFiles);

  const tarball = join(root, metadata.filename);
  assert.equal(existsSync(tarball), true);
  const packedEntries = readPackedEntries(tarball);
  assert.deepEqual([...packedEntries.keys()].sort(), expectedFiles);
  assert.deepEqual(packedEntries.get("skills/pi-worker/PI_WORKER_IDENTITY").data, Buffer.from("pi-worker-skill/v1\n", "utf8"));
  assert.equal(packedEntries.get("npm/bin/pi-worker.mjs").mode & 0o111, 0o111);
  assert.equal(packedEntries.get("LICENSE").mode, 0o644);
  assert.equal(packedEntries.get("THIRD_PARTY_NOTICES").mode, 0o644);
  for (const [suffix, dir] of targets) {
    const packedBinary = packedEntries.get(`npm/native/${dir}/pi-worker`);
    assert.equal(packedBinary.mode, 0o755);
    assert.deepEqual(packedBinary.data, archiveBinary(readFileSync(join(dist, `pi-worker_v0.1.0_${suffix}.tar.gz`))));
  }
});

test("current checkout npm pack has the exact staged inventory when requested", {
  skip: process.env.PI_WORKER_ASSERT_STAGED !== "1",
}, () => {
  for (const nativePath of expectedFiles.filter((path) => path.startsWith("npm/native/"))) {
    const entry = lstatSync(join(repository, nativePath));
    assert.equal(entry.isFile(), true, `${nativePath} is a regular file`);
    assert.equal(entry.mode & 0o777, 0o755, `${nativePath} is executable`);
  }
  const packed = spawnSync("npm", ["pack", "--dry-run", "--json", "--ignore-scripts"], {
    cwd: repository,
    encoding: "utf8",
  });
  assert.equal(packed.status, 0, packed.stderr);
  const metadata = JSON.parse(packed.stdout)[0];
  assert.deepEqual(metadata.files.map((file) => file.path).sort(), expectedFiles);
  assert.equal(existsSync(join(repository, metadata.filename)), false, "dry run leaves no tarball");
});
