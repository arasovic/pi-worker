#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  chmodSync,
  closeSync,
  constants,
  fstatSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readdirSync,
  readSync,
  realpathSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { gunzipSync } from "node:zlib";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const TARGETS = Object.freeze({
  darwin_arm64: "darwin-arm64",
  darwin_amd64: "darwin-x64",
  linux_arm64: "linux-arm64",
  linux_amd64: "linux-x64",
});
const ARCHIVE_PATTERN = /^pi-worker_(v[^_]+)_(darwin_arm64|darwin_amd64|linux_arm64|linux_amd64)\.tar\.gz$/;
const ARCHIVE_ENTRIES = new Set(["LICENSE", "THIRD_PARTY_NOTICES", "pi-worker"]);
const MAX_ARCHIVE_BYTES = 64 * 1024 * 1024;
const MAX_TAR_BYTES = 64 * 1024 * 1024;
const MAX_BINARY_BYTES = 64 * 1024 * 1024;
const MAX_CHECKSUM_BYTES = 4 * 1024;
const STAGE_LOCK_NAME = ".native-stage.lock";

function fail(message) {
  throw new Error(`package staging failed: ${message}`);
}

function validNumericIdentifier(value) {
  return /^[0-9]+$/.test(value) && (value.length === 1 || value[0] !== "0");
}

function validIdentifiers(value, prerelease) {
  if (value.length === 0) return false;
  return value.split(".").every((identifier) => {
    if (!/^[0-9A-Za-z-]+$/.test(identifier)) return false;
    return !prerelease || !/^0[0-9]+$/.test(identifier);
  });
}

function validReleaseVersion(version) {
  if (!version.startsWith("v") || version.length < 2) return false;
  const value = version.slice(1);
  const plus = value.indexOf("+");
  const coreAndPrerelease = plus < 0 ? value : value.slice(0, plus);
  if (plus >= 0 && (value.indexOf("+", plus + 1) >= 0 || !validIdentifiers(value.slice(plus + 1), false))) {
    return false;
  }
  const dash = coreAndPrerelease.indexOf("-");
  const core = dash < 0 ? coreAndPrerelease : coreAndPrerelease.slice(0, dash);
  if (dash >= 0 && !validIdentifiers(coreAndPrerelease.slice(dash + 1), true)) return false;
  const parts = core.split(".");
  return parts.length === 3 && parts.every(validNumericIdentifier);
}

function textField(block, offset, length) {
  const end = block.indexOf(0, offset + 1);
  const limit = end < 0 || end > offset + length ? offset + length : end;
  return block.toString("utf8", offset, limit);
}

function octalField(block, offset, length, fieldName) {
  const value = textField(block, offset, length).trim();
  if (!/^[0-7]+$/.test(value)) fail(`malformed tar ${fieldName}`);
  return Number.parseInt(value, 8);
}

function isZeroBlock(block) {
  for (const byte of block) if (byte !== 0) return false;
  return true;
}

function verifyTarHeader(block) {
  if (block.length !== 512 || textField(block, 257, 6) !== "ustar") fail("malformed tar header");
  const expected = octalField(block, 148, 8, "checksum");
  let actual = 0;
  for (let index = 0; index < 512; index += 1) {
    actual += index >= 148 && index < 156 ? 0x20 : block[index];
  }
  if (actual !== expected) fail("invalid tar header checksum");
}

function parseArchive(bytes, archiveName) {
  if (bytes.length > MAX_ARCHIVE_BYTES) fail(`${archiveName} is too large`);
  let tar;
  try {
    tar = gunzipSync(bytes, { maxOutputLength: MAX_TAR_BYTES });
  } catch (error) {
    fail(`${archiveName} is not valid gzip data (${error.message})`);
  }
  if (tar.length > MAX_TAR_BYTES || tar.length < 1024 || tar.length % 512 !== 0) fail(`${archiveName} has malformed tar padding`);

  const entries = new Map();
  let offset = 0;
  for (;;) {
    const header = tar.subarray(offset, offset + 512);
    if (header.length !== 512) fail(`${archiveName} has a truncated tar header`);
    if (isZeroBlock(header)) {
      if (!isZeroBlock(tar.subarray(offset + 512, offset + 1024)) || offset + 1024 !== tar.length) {
        fail(`${archiveName} has malformed tar termination`);
      }
      break;
    }
    verifyTarHeader(header);
    const prefix = header.subarray(345, 500);
    if (prefix.some((byte) => byte !== 0)) fail(`${archiveName} contains unsupported tar prefix`);
    const name = textField(header, 0, 100);
    if (name.includes("\\") || name.startsWith("/") || name.split("/").includes("..")) {
      fail(`${archiveName} contains unsafe archive path ${JSON.stringify(name)}`);
    }
    if (!ARCHIVE_ENTRIES.has(name)) fail(`${archiveName} contains unexpected entry ${JSON.stringify(name)}`);
    if (entries.has(name)) fail(`${archiveName} contains duplicate entry ${JSON.stringify(name)}`);
    const type = textField(header, 156, 1) || "0";
    if (type !== "0") fail(`${archiveName} contains non-regular entry ${JSON.stringify(name)}`);
    const size = octalField(header, 124, 12, `${name} size`);
    const dataStart = offset + 512;
    const dataEnd = dataStart + size;
    if (!Number.isSafeInteger(size) || dataEnd > tar.length) fail(`${archiveName} contains truncated entry ${JSON.stringify(name)}`);
    entries.set(name, {
      data: tar.subarray(dataStart, dataEnd),
      mode: octalField(header, 100, 8, `${name} mode`),
    });
    offset = dataStart + Math.ceil(size / 512) * 512;
    if (offset > tar.length) fail(`${archiveName} contains truncated entry padding`);
  }

  if (entries.size !== ARCHIVE_ENTRIES.size || [...ARCHIVE_ENTRIES].some((name) => !entries.has(name))) {
    fail(`${archiveName} does not contain exactly LICENSE, THIRD_PARTY_NOTICES, and pi-worker`);
  }
  for (const metadataName of ["LICENSE", "THIRD_PARTY_NOTICES"]) {
    if (entries.get(metadataName).mode !== 0o644) fail(`${archiveName} ${metadataName} is not mode 0644`);
  }
  const binary = entries.get("pi-worker");
  if (binary.mode !== 0o755) fail(`${archiveName} pi-worker is not mode 0755`);
  if (binary.data.length > MAX_BINARY_BYTES) fail(`${archiveName} pi-worker is too large`);
  return Buffer.from(binary.data);
}

function expectedArchives(distPath) {
  let names;
  try {
    names = readdirSync(distPath);
  } catch (error) {
    fail(`unable to read dist directory: ${error.message}`);
  }
  const expected = new Map();
  let version;
  for (const name of names) {
    if (name === "checksums.txt") continue;
    const match = ARCHIVE_PATTERN.exec(name);
    if (!match) fail(`unexpected release input ${JSON.stringify(name)}`);
    if (!validReleaseVersion(match[1])) fail(`invalid release archive version ${JSON.stringify(match[1])}`);
    if (version === undefined) version = match[1];
    if (version !== match[1]) fail("release archives use different versions");
    if (expected.has(match[2])) fail(`duplicate release target ${match[2]}`);
    expected.set(match[2], name);
  }
  if (expected.size !== Object.keys(TARGETS).length) fail("expected exactly four release archives");
  return expected;
}

function readBoundedRegularFile(path, name, maxBytes = MAX_ARCHIVE_BYTES) {
  let descriptor;
  let entry;
  try {
    entry = lstatSync(path);
    if (!entry.isFile()) fail(`${name} is not a regular file`);

    let openFlags = constants.O_RDONLY | constants.O_NONBLOCK;
    if (constants.O_NOFOLLOW !== undefined) openFlags |= constants.O_NOFOLLOW;
    descriptor = openSync(path, openFlags);

    const stat = fstatSync(descriptor);
    if (!stat.isFile()) fail(`${name} is not a regular file`);
    if (stat.dev !== entry.dev || stat.ino !== entry.ino) {
      fail(`${name} is not a regular file`);
    }

    if (!Number.isSafeInteger(stat.size) || stat.size > maxBytes) fail(`${name} is too large`);
    const bytes = Buffer.alloc(stat.size);
    let offset = 0;
    while (offset < stat.size) {
      const count = readSync(descriptor, bytes, offset, stat.size - offset, null);
      if (count === 0) fail(`${name} ended while reading`);
      offset += count;
    }
    const eofCheck = Buffer.alloc(1);
    if (readSync(descriptor, eofCheck, 0, 1, null) !== 0) fail(`${name} changed while reading`);
    return bytes;
  } catch (error) {
    if (error?.code === "ELOOP") fail(`${name} is not a regular file`);
    if (error?.message?.startsWith("package staging failed:")) throw error;
    fail(`unable to read ${name}: ${error.message}`);
  } finally {
    if (descriptor !== undefined) closeSync(descriptor);
  }
}

function parseChecksums(distPath, archives) {
  const archiveNames = new Set(archives.values());
  const checksumPath = join(distPath, "checksums.txt");
  const text = readBoundedRegularFile(checksumPath, "checksums.txt", MAX_CHECKSUM_BYTES).toString("utf8");
  if (!text.endsWith("\n")) fail("checksums.txt must end with a newline");
  const lines = text.slice(0, -1).split("\n");
  if (lines.length !== archives.size || lines.some((line) => line.length === 0)) {
    fail("checksums.txt must list exactly the four release archives");
  }
  const listed = new Map();
  for (const line of lines) {
    const match = /^([0-9a-f]{64})  (.+)$/.exec(line);
    if (!match || listed.has(match[2])) fail("malformed or duplicate checksums.txt entry");
    listed.set(match[2], match[1]);
  }
  if (listed.size !== archives.size) fail("checksums.txt contains an unexpected release archive");
  for (const name of archiveNames) {
    if (!listed.has(name)) fail(`checksums.txt is missing ${name}`);
  }
  for (const name of listed.keys()) {
    if (!archiveNames.has(name)) fail("checksums.txt contains an unexpected release archive");
  }
  return listed;
}

function refuseExistingNative(nativePath) {
  try {
    lstatSync(nativePath);
    fail("npm/native already exists; refusing to replace it");
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
}

export function stagePackage(distArgument, packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../..")) {
  if (!distArgument) fail("--dist is required");
  const distPath = resolve(packageRoot, distArgument);
  const npmRoot = join(packageRoot, "npm");
  const nativePath = join(npmRoot, "native");
  if (!lstatSync(npmRoot).isDirectory()) fail("npm is not a directory");

  let temporary;
  let lockAcquired = false;
  const lockPath = join(npmRoot, STAGE_LOCK_NAME);
  try {
    try {
      mkdirSync(lockPath);
      lockAcquired = true;
    } catch (error) {
      if (error.code === "EEXIST") fail("another package staging invocation is already running");
      throw error;
    }
    refuseExistingNative(nativePath);

    const archives = expectedArchives(distPath);
    const checksums = parseChecksums(distPath, archives);
    const binaries = new Map();
    for (const [target, name] of archives) {
      const archiveBytes = readBoundedRegularFile(join(distPath, name), name);
      const expectedChecksum = checksums.get(name);
      const actualChecksum = createHash("sha256").update(archiveBytes).digest("hex");
      if (actualChecksum !== expectedChecksum) fail(`checksum mismatch for ${name}`);
      binaries.set(target, parseArchive(archiveBytes, name));
    }

    temporary = mkdtempSync(join(npmRoot, ".native-stage-"));
    for (const [archiveTarget, directory] of Object.entries(TARGETS)) {
      const targetDirectory = join(temporary, directory);
      mkdirSync(targetDirectory);
      const binaryPath = join(targetDirectory, "pi-worker");
      writeFileSync(binaryPath, binaries.get(archiveTarget), { mode: 0o755, flag: "wx" });
      chmodSync(binaryPath, 0o755);
    }
    refuseExistingNative(nativePath);
    renameSync(temporary, nativePath);
    temporary = undefined;
  } finally {
    if (temporary) rmSync(temporary, { recursive: true, force: true });
    if (lockAcquired) rmSync(lockPath, { recursive: true, force: true });
  }
}

function parseArguments(argv) {
  if (argv.length !== 2 || argv[0] !== "--dist" || argv[1].length === 0) {
    fail("usage: stage-package.mjs --dist <directory>");
  }
  return argv[1];
}

const invokedPath = process.argv[1] ? realpathSync(process.argv[1]) : "";
if (invokedPath === realpathSync(fileURLToPath(import.meta.url))) {
  try {
    stagePackage(parseArguments(process.argv.slice(2)));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
