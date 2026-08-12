import { createHash } from "node:crypto";
import { constants } from "node:fs";
import {
  lstat,
  open,
  opendir,
  readlink,
  realpath,
} from "node:fs/promises";
import path from "node:path";

import { validateReceipt } from "./skill-receipt.mjs";
import { PINNED_SKILLS_VERSION } from "./skill-rules.mjs";

export const IDENTITY_FILE = "PI_WORKER_IDENTITY";
export const IDENTITY_CONTENT = "pi-worker-skill/v1\n";
export const IDENTITY_SHA256 = createHash("sha256")
  .update(Buffer.from(IDENTITY_CONTENT, "utf8"))
  .digest("hex");
const LEGACY_IDENTITY_SHA256 = new Set([]);

const FILE_HASH_PATTERN = /^[0-9a-f]{64}$/i;
const MAX_TREE_DEPTH = 32;
const MAX_TREE_FILES = 256;
const MAX_TREE_ENTRIES = 1024;
const MAX_FILE_BYTES = 1024 * 1024;
const MAX_TREE_BYTES = 4 * 1024 * 1024;
const READ_CHUNK_BYTES = 64 * 1024;

class SkillTreeError extends Error {
  constructor(message, options = {}) {
    super(message, options);
    this.name = "SkillTreeError";
  }
}

function fail(message, options) {
  throw new SkillTreeError(message, options);
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function isSha256(value) {
  return typeof value === "string" && FILE_HASH_PATTERN.test(value);
}

function normalizeRelativePath(value, label = "path") {
  if (typeof value !== "string" || value.length === 0) {
    fail(`${label} must be a non-empty relative path`);
  }
  if (value.includes("\0") || value.includes("\\")) {
    fail(`${label} is not a safe relative path`);
  }
  if (value.startsWith("/") || /^[A-Za-z]:/.test(value)) {
    fail(`${label} must not escape the tree`);
  }

  const parts = value.split("/");
  if (parts.some((part) => part.length === 0 || part === "." || part === "..")) {
    fail(`${label} contains a path escape or empty segment`);
  }
  if (path.posix.normalize(value) !== value) {
    fail(`${label} is not normalized`);
  }
  return value;
}

function normalizedAbsolutePath(value, label = "path") {
  if (typeof value !== "string" || value.length === 0 || !path.isAbsolute(value)) {
    return null;
  }
  if (value.includes("\0")) return null;
  return path.normalize(value);
}

function assertInside(root, candidate) {
  const relative = path.relative(root, candidate);
  if (
    relative === "" ||
    path.isAbsolute(relative) ||
    relative === ".." ||
    relative.startsWith(`..${path.sep}`)
  ) {
    fail(`path escape while walking skill tree: ${candidate}`);
  }
  return normalizeRelativePath(relative.split(path.sep).join("/"), "tree entry path");
}

function assertReadableMode(info, kind, displayPath) {
  if ((info.mode & 0o444) === 0) {
    fail(`unreadable ${kind}: ${displayPath}`);
  }
  if (kind === "directory" && (info.mode & 0o111) === 0) {
    fail(`unreadable directory: ${displayPath}`);
  }
}

function directoryIdentity(info) {
  // Device/inode pairs let the walker fail closed even on filesystems that
  // expose a directory cycle without a symbolic link.
  if (typeof info.dev !== "number" || typeof info.ino !== "number") return null;
  return `${info.dev}:${info.ino}`;
}

function sameFileIdentity(left, right) {
  return (
    typeof left.dev === "number" &&
    typeof left.ino === "number" &&
    typeof right.dev === "number" &&
    typeof right.ino === "number" &&
    left.dev === right.dev &&
    left.ino === right.ino
  );
}

function assertFileSize(info, filePath) {
  if (!Number.isSafeInteger(info.size) || info.size < 0) {
    fail(`file size is unavailable: ${filePath}`);
  }
  if (info.size > MAX_FILE_BYTES) {
    fail(`file exceeds the ${MAX_FILE_BYTES}-byte limit: ${filePath}`);
  }
}

async function readRegularFile(filePath, expectedInfo = undefined) {
  let before;
  try {
    before = expectedInfo ?? await lstat(filePath);
  } catch (error) {
    fail(`unreadable file ${filePath}: ${error.message}`, { cause: error });
  }

  if (before.isSymbolicLink()) fail(`symlink is not allowed in skill tree: ${filePath}`);
  if (!before.isFile()) fail(`special file is not allowed in skill tree: ${filePath}`);
  assertReadableMode(before, "file", filePath);
  assertFileSize(before, filePath);

  let handle;
  try {
    let flags = constants.O_RDONLY;
    if (typeof constants.O_NOFOLLOW === "number") flags |= constants.O_NOFOLLOW;
    handle = await open(filePath, flags);
    const opened = await handle.stat();
    if (!opened.isFile()) fail(`special file is not allowed in skill tree: ${filePath}`);
    assertReadableMode(opened, "file", filePath);
    assertFileSize(opened, filePath);
    if (!sameFileIdentity(before, opened) || opened.size !== before.size) {
      fail(`skill tree entry changed while reading: ${filePath}`);
    }

    const chunks = [];
    let position = 0;
    let remaining = opened.size;
    while (remaining > 0) {
      const length = Math.min(READ_CHUNK_BYTES, remaining);
      const buffer = Buffer.alloc(length);
      const result = await handle.read(buffer, 0, length, position);
      if (result.bytesRead <= 0) fail(`skill tree entry changed while reading: ${filePath}`);
      chunks.push(result.bytesRead === buffer.length
        ? buffer
        : buffer.subarray(0, result.bytesRead));
      position += result.bytesRead;
      remaining -= result.bytesRead;
    }

    const afterHandle = await handle.stat();
    if (
      !afterHandle.isFile() ||
      !sameFileIdentity(opened, afterHandle) ||
      afterHandle.size !== opened.size
    ) {
      fail(`skill tree entry changed while reading: ${filePath}`);
    }
    const data = Buffer.concat(chunks, opened.size);
    const after = await lstat(filePath);
    if (after.isSymbolicLink()) fail(`symlink is not allowed in skill tree: ${filePath}`);
    if (!after.isFile()) fail(`special file is not allowed in skill tree: ${filePath}`);
    if (!sameFileIdentity(before, after) || after.size !== before.size) {
      fail(`skill tree entry changed while reading: ${filePath}`);
    }
    return data;
  } catch (error) {
    if (error instanceof SkillTreeError) throw error;
    fail(`unreadable file ${filePath}: ${error.message}`, { cause: error });
  } finally {
    if (handle) {
      try {
        await handle.close();
      } catch {
        // The read result is already invalid if closing the handle fails only
        // after the data was obtained; do not hide the primary result here.
      }
    }
  }
}

async function walkTree(root, current, state, files) {
  let info;
  try {
    info = await lstat(current);
  } catch (error) {
    fail(`unreadable skill tree entry ${current}: ${error.message}`, { cause: error });
  }

  if (info.isSymbolicLink()) fail(`symlink is not allowed in skill tree: ${current}`);
  if (!info.isDirectory()) fail(`skill tree root/entry is not a directory: ${current}`);
  assertReadableMode(info, "directory", current);

  const identity = directoryIdentity(info);
  if (identity !== null) {
    if (state.directories.has(identity)) {
      fail(`directory cycle or duplicate directory while walking: ${current}`);
    }
    state.directories.add(identity);
  }

  const entries = [];
  let directory;
  let listingError = null;
  try {
    directory = await opendir(current);
    while (true) {
      const entry = await directory.read();
      if (entry === null) break;
      state.entryCount += 1;
      if (state.entryCount > MAX_TREE_ENTRIES) {
        fail(`skill tree exceeds the ${MAX_TREE_ENTRIES}-entry limit`);
      }
      entries.push(entry);
    }
  } catch (error) {
    listingError = error;
  } finally {
    if (directory) {
      try {
        await directory.close();
      } catch (error) {
        listingError ??= error;
      }
    }
  }
  if (listingError) {
    if (listingError instanceof SkillTreeError) throw listingError;
    fail(`unreadable directory ${current}: ${listingError.message}`, { cause: listingError });
  }

  let afterListing;
  try {
    afterListing = await lstat(current);
  } catch (error) {
    fail(`directory changed while walking ${current}: ${error.message}`, { cause: error });
  }
  if (
    afterListing.isSymbolicLink() ||
    !afterListing.isDirectory() ||
    (identity !== null && directoryIdentity(afterListing) !== identity)
  ) {
    fail(`directory changed while walking: ${current}`);
  }

  entries.sort((left, right) => left.name < right.name ? -1 : left.name > right.name ? 1 : 0);

  if (current !== root && entries.length === 0) {
    fail(`empty directory is not allowed in skill tree: ${current}`);
  }

  for (const entry of entries) {
    if (
      entry.name.length === 0 ||
      entry.name === "." ||
      entry.name === ".." ||
      entry.name.includes("\0") ||
      entry.name.includes("/") ||
      entry.name.includes("\\")
    ) {
      fail(`unsafe tree entry name: ${entry.name}`);
    }

    const entryPath = path.resolve(current, entry.name);
    const relativePath = assertInside(root, entryPath);
    if (relativePath.split("/").length > MAX_TREE_DEPTH) {
      fail(`skill tree exceeds the ${MAX_TREE_DEPTH}-level depth limit: ${relativePath}`);
    }
    let entryInfo;
    try {
      entryInfo = await lstat(entryPath);
    } catch (error) {
      fail(`unreadable skill tree entry ${entryPath}: ${error.message}`, { cause: error });
    }

    if (entryInfo.isSymbolicLink()) {
      fail(`symlink is not allowed in skill tree: ${entryPath}`);
    }

    if (entryInfo.isDirectory()) {
      await walkTree(root, entryPath, state, files);
      continue;
    }

    if (!entryInfo.isFile()) {
      fail(`special file is not allowed in skill tree: ${entryPath}`);
    }
    if (state.fileCount >= MAX_TREE_FILES) {
      fail(`skill tree exceeds the ${MAX_TREE_FILES}-file limit`);
    }
    assertFileSize(entryInfo, entryPath);
    if (state.totalBytes + entryInfo.size > MAX_TREE_BYTES) {
      fail(`skill tree exceeds the ${MAX_TREE_BYTES}-byte total limit`);
    }

    state.fileCount += 1;
    const data = await readRegularFile(entryPath, entryInfo);
    state.totalBytes += data.length;
    if (state.totalBytes > MAX_TREE_BYTES) {
      fail(`skill tree exceeds the ${MAX_TREE_BYTES}-byte total limit`);
    }
    if (state.paths.has(relativePath)) {
      fail(`duplicate normalized path in skill tree: ${relativePath}`);
    }
    state.paths.add(relativePath);
    files.push({
      path: relativePath,
      sha256: createHash("sha256").update(data).digest("hex"),
    });
  }
}

function manifestEntries(value, label = "manifest") {
  let entries = value;
  if (isObject(value)) {
    if (Array.isArray(value.files)) entries = value.files;
    else if (Array.isArray(value.hashes)) entries = value.hashes;
    else if (Array.isArray(value.manifest)) entries = value.manifest;
  }
  if (!Array.isArray(entries)) fail(`${label} must be an array of file hashes`);

  const seen = new Set();
  const normalized = [];
  for (const [index, entry] of entries.entries()) {
    if (!isObject(entry)) fail(`${label}[${index}] must be an object`);
    const relativePath = normalizeRelativePath(entry.path, `${label}[${index}].path`);
    if (seen.has(relativePath)) {
      fail(`duplicate normalized path in ${label}: ${relativePath}`);
    }
    seen.add(relativePath);
    if (!isSha256(entry.sha256)) {
      fail(`${label}[${index}].sha256 must be a SHA-256 hex digest`);
    }
    normalized.push({ path: relativePath, sha256: entry.sha256.toLowerCase() });
  }
  return normalized;
}

function manifestMap(value, label) {
  const entries = manifestEntries(value, label);
  const map = new Map(entries.map((entry) => [entry.path, entry.sha256]));
  return { entries, map };
}

function treeResult(files) {
  files.sort((left, right) => left.path < right.path ? -1 : left.path > right.path ? 1 : 0);
  const result = { files };
  // These non-enumerable aliases keep the internal manifest useful to callers
  // that call it a hash list while keeping the receipt-shaped public value
  // deterministic and JSON-safe.
  Object.defineProperties(result, {
    hashes: { value: files, enumerable: false },
    manifest: { value: files, enumerable: false },
  });
  return result;
}

function checkManifest(expectedValue, actualTree) {
  if (expectedValue === undefined || expectedValue === null) return;
  const expected = manifestMap(expectedValue, "bundled manifest");
  const actual = manifestMap(actualTree, "hashed tree");
  for (const [relativePath, digest] of expected.map) {
    if (!actual.map.has(relativePath)) fail(`hashed tree is missing manifest path: ${relativePath}`);
    if (actual.map.get(relativePath) !== digest) {
      fail(`hashed tree hash differs from manifest for ${relativePath}`);
    }
  }
  for (const relativePath of actual.map.keys()) {
    if (!expected.map.has(relativePath)) {
      fail(`hashed tree contains manifest extra: ${relativePath}`);
    }
  }
}

function optionsManifest(options) {
  if (options === undefined || options === null) return undefined;
  if (Array.isArray(options)) return options;
  if (isObject(options)) {
    if (Object.hasOwn(options, "manifest")) return options.manifest;
    if (Object.hasOwn(options, "files")) return options.files;
    if (Object.hasOwn(options, "hashes")) return options.hashes;
  }
  fail("hashSkillTree options must contain a manifest");
}

/**
 * Hash every regular file below a skill root.
 *
 * The returned manifest is independent of traversal order: paths use `/`, are
 * sorted lexicographically, and digests are lowercase SHA-256 values.
 */
export async function hashSkillTree(rootPath, options = undefined) {
  if (typeof rootPath !== "string" || rootPath.length === 0 || rootPath.includes("\0")) {
    fail("skill tree path must be a non-empty path");
  }
  const requestedRoot = path.resolve(rootPath);
  let requestedInfo;
  try {
    requestedInfo = await lstat(requestedRoot);
  } catch (error) {
    fail(`unreadable skill tree root ${requestedRoot}: ${error.message}`, { cause: error });
  }
  if (requestedInfo.isSymbolicLink()) {
    fail(`symlink is not allowed in skill tree root: ${requestedRoot}`);
  }
  if (!requestedInfo.isDirectory()) {
    fail(`skill tree root is not a directory: ${requestedRoot}`);
  }
  assertReadableMode(requestedInfo, "directory", requestedRoot);

  let root;
  try {
    // Resolve the requested root once. Ancestor aliases such as macOS /var
    // are normal and are represented by this canonical path while walking.
    root = await realpath(requestedRoot);
  } catch (error) {
    fail(`unreadable skill tree root ${requestedRoot}: ${error.message}`, { cause: error });
  }
  let revalidatedRequestedInfo;
  try {
    revalidatedRequestedInfo = await lstat(requestedRoot);
  } catch (error) {
    fail(`unreadable skill tree root ${requestedRoot}: ${error.message}`, { cause: error });
  }
  if (
    revalidatedRequestedInfo.isSymbolicLink() ||
    !revalidatedRequestedInfo.isDirectory() ||
    (directoryIdentity(requestedInfo) !== null &&
      directoryIdentity(revalidatedRequestedInfo) !== directoryIdentity(requestedInfo))
  ) {
    fail(`skill tree root changed while canonicalizing: ${requestedRoot}`);
  }

  let canonicalInfo;
  try {
    canonicalInfo = await lstat(root);
  } catch (error) {
    fail(`unreadable canonical skill tree root ${root}: ${error.message}`, { cause: error });
  }
  if (
    canonicalInfo.isSymbolicLink() ||
    !canonicalInfo.isDirectory() ||
    (directoryIdentity(revalidatedRequestedInfo) !== null &&
      directoryIdentity(canonicalInfo) !== directoryIdentity(revalidatedRequestedInfo))
  ) {
    fail(`skill tree root changed while canonicalizing: ${requestedRoot}`);
  }

  const files = [];
  await walkTree(root, root, {
    directories: new Set(),
    paths: new Set(),
    entryCount: 0,
    fileCount: 0,
    totalBytes: 0,
  }, files);
  const result = treeResult(files);
  checkManifest(optionsManifest(options), result);
  return result;
}

function normalizeTree(value, label) {
  return manifestMap(value, label);
}

function normalizeTarget(target) {
  const rawPath = typeof target === "string" ? target : target?.path;
  const normalizedPath = normalizedAbsolutePath(rawPath, "target path");
  if (normalizedPath === null) {
    throw new TypeError("target path must be absolute");
  }
  const expectedKind = typeof target === "object" && target !== null
    ? target.expectedKind
    : undefined;
  if (expectedKind !== undefined && !["canonical", "copy", "symlink"].includes(expectedKind)) {
    throw new TypeError("target expectedKind is not recognized");
  }
  // expectedKind is a trusted role expectation supplied by the installer, but
  // the filesystem entry remains authoritative for the actual topology.
  return { path: normalizedPath, expectedKind };
}

async function existingTargetState(targetPath) {
  try {
    const info = await lstat(targetPath);
    if (info.isSymbolicLink()) return { kind: "symlink" };
    if (!info.isDirectory()) return { kind: "non-directory" };
    return { kind: "directory" };
  } catch (error) {
    if (error?.code === "ENOENT") return { kind: "absent" };
    return { kind: "unreadable", error };
  }
}

function declaresPiWorkerSkill(content) {
  const text = content.toString("utf8");
  const lines = text.split(/\r?\n/);
  if (lines.length < 3 || lines[0].trim() !== "---") return false;

  let closing = -1;
  for (let index = 1; index < lines.length; index += 1) {
    if (lines[index].trim() === "---") {
      closing = index;
      break;
    }
  }
  if (closing < 0) return false;

  let nameCount = 0;
  let nameValue = null;
  for (const line of lines.slice(1, closing)) {
    const trimmed = line.trim();
    if (trimmed.length === 0 || trimmed.startsWith("#")) continue;

    const match = line.match(/^\s*([A-Za-z_][A-Za-z0-9_-]*)\s*:\s*(.*?)\s*$/);
    if (!match) return false;
    const [, key, rawValue] = match;
    if (key !== "name") continue;

    nameCount += 1;
    let value = rawValue;
    if (value.length >= 2) {
      const first = value[0];
      const last = value[value.length - 1];
      if ((first === "\"" && last === "\"") || (first === "'" && last === "'")) {
        value = value.slice(1, -1);
      }
    }
    nameValue = value;
  }

  return nameCount === 1 && nameValue === "pi-worker";
}

async function hasIdentityEvidence(targetPath, currentMap) {
  if (currentMap.get(IDENTITY_FILE) !== IDENTITY_SHA256) return false;
  const skillPath = path.join(targetPath, "SKILL.md");
  try {
    // Front-matter is evidence, so it receives the same no-follow, size, and
    // race checks as every manifest file.
    const content = await readRegularFile(skillPath);
    return declaresPiWorkerSkill(content);
  } catch {
    return false;
  }
}

// Inspect public identity without conferring ownership. The marker identifies
// the Pi Worker product family; only a validated receipt can authorize writes.
export async function inspectSkillIdentity(targetPath) {
  const normalized = normalizedAbsolutePath(targetPath, "target path");
  if (normalized === null) throw new TypeError("target path must be absolute");

  const state = await existingTargetState(normalized);
  if (state.kind === "absent") return "absent";
  if (state.kind === "non-directory" || state.kind === "unreadable") {
    fail(`external skill target is not safely readable: ${normalized}`, { cause: state.error });
  }

  let root = normalized;
  if (state.kind === "symlink") {
    try {
      root = path.normalize(await realpath(normalized));
    } catch (error) {
      fail(`external skill target cannot be resolved: ${normalized}`, { cause: error });
    }
    const resolved = await existingTargetState(root);
    if (resolved.kind !== "directory") {
      fail(`external skill target does not resolve to a directory: ${normalized}`);
    }
  }

  const tree = normalizeTree(await hashSkillTree(root), "external skill tree");
  let declaresIdentity = false;
  try {
    declaresIdentity = declaresPiWorkerSkill(await readRegularFile(path.join(root, "SKILL.md")));
  } catch {
    declaresIdentity = false;
  }
  if (!declaresIdentity) return "none";

  const markerDigest = tree.map.get(IDENTITY_FILE);
  if (markerDigest === undefined) return "none";
  if (markerDigest === IDENTITY_SHA256) return "current";
  if (LEGACY_IDENTITY_SHA256.has(markerDigest)) return "legacy";
  return "unknown";
}

function validatedOwnershipReceipt(value) {
  if (value === undefined || value === null) return null;
  let validated;
  try {
    validated = validateReceipt(value);
  } catch {
    return null;
  }
  const canonicals = validated.targets.filter((candidate) => candidate.kind === "canonical");
  if (
    validated.skillsVersion !== PINNED_SKILLS_VERSION ||
    validated.outcome !== "installed" ||
    validated.affectedTargets.length !== 0 ||
    canonicals.length !== 1 ||
    !recordsIdentity(canonicals[0])
  ) {
    return null;
  }
  return validated;
}

function matchingReceiptTarget(receipt, targetPath, kinds) {
  if (!receipt) return null;
  const normalizedPath = path.normalize(targetPath);
  return receipt.targets.find((candidate) => (
    path.normalize(candidate.path) === normalizedPath && kinds.includes(candidate.kind)
  )) ?? null;
}

function recordsIdentity(target) {
  return target?.files.some((file) => (
    file.path === IDENTITY_FILE && file.sha256.toLowerCase() === IDENTITY_SHA256
  )) ?? false;
}

async function receiptOwnsCanonicalDirectory(receipt, resolvedPath) {
  if (!receipt) return false;
  const canonicals = receipt.targets.filter((candidate) => candidate.kind === "canonical");
  if (canonicals.length !== 1 || !recordsIdentity(canonicals[0])) return false;
  const canonical = canonicals[0];
  try {
    const state = await existingTargetState(canonical.path);
    if (state.kind !== "directory") return false;
    if (path.normalize(await realpath(canonical.path)) !== path.normalize(resolvedPath)) {
      return false;
    }

    // A symlink is owned only after independently checking the current
    // canonical tree. The receipt's canonical manifest and identity evidence
    // are part of this check; the installer must not preflight it separately.
    const current = normalizeTree(await hashSkillTree(canonical.path), "canonical tree");
    return compareDirectoryManifest(current.map, canonical.files) === "owned" &&
      await hasIdentityEvidence(canonical.path, current.map);
  } catch {
    return false;
  }
}

function compareDirectoryManifest(currentMap, recordedFiles) {
  const recordedMap = new Map(recordedFiles.map((file) => [file.path, file.sha256]));

  // An unrecorded current file is a conflict: a receipt must not silently
  // adopt content that it did not name.
  for (const relativePath of currentMap.keys()) {
    if (!recordedMap.has(relativePath)) return "conflicting";
  }
  for (const [relativePath, digest] of recordedMap) {
    if (!currentMap.has(relativePath) || currentMap.get(relativePath) !== digest) {
      return "drifted";
    }
  }
  return "owned";
}

async function resolvedSymlinkTree(targetPath) {
  let resolvedPath;
  try {
    resolvedPath = await realpath(targetPath);
    const resolvedState = await existingTargetState(resolvedPath);
    if (resolvedState.kind !== "directory") return null;
    return { path: path.normalize(resolvedPath) };
  } catch {
    // A dangling or non-directory destination fails closed. The destination's
    // full manifest is intentionally deferred until ownership evidence fails;
    // the canonical target is preflighted independently by the installer.
    return null;
  }
}

function matchingSymlinkTarget(receipt, parentPath, entryName) {
  const candidate = matchingReceiptTarget(receipt, parentPath, ["symlink"]);
  if (!candidate || candidate.files.length !== 1) return null;
  const file = candidate.files[0];
  if (file.path !== entryName) return null;
  return { candidate, file };
}

/**
 * Classify a target without mutating it. The filesystem entry, rather than a
 * caller-supplied kind, determines whether the target is a directory or a
 * root symlink.
 */
export async function classifyTarget({ target, bundledTree, receipt } = {}) {
  const targetInfo = normalizeTarget(target);
  const bundled = normalizeTree(bundledTree, "bundled tree");
  const state = await existingTargetState(targetInfo.path);
  if (state.kind === "absent") return "absent";
  if (state.kind === "non-directory" || state.kind === "unreadable") return "conflicting";

  const ownershipReceipt = validatedOwnershipReceipt(receipt);

  if (state.kind === "symlink") {
    const resolved = await resolvedSymlinkTree(targetInfo.path);
    if (!resolved) return "conflicting";

    let destination;
    try {
      destination = await readlink(targetInfo.path, { encoding: "buffer" });
    } catch {
      return "conflicting";
    }
    const linkDigest = createHash("sha256")
      .update(destination)
      .digest("hex");
    if (targetInfo.expectedKind && targetInfo.expectedKind !== "symlink") return "conflicting";
    const symlinkTarget = matchingSymlinkTarget(
      ownershipReceipt,
      path.dirname(targetInfo.path),
      path.basename(targetInfo.path),
    );
    if (symlinkTarget) {
      if (
        symlinkTarget.file.sha256.toLowerCase() === linkDigest &&
        await receiptOwnsCanonicalDirectory(ownershipReceipt, resolved.path)
      ) {
        return "owned";
      }
      if (symlinkTarget.file.sha256.toLowerCase() === linkDigest) {
        let resolvedManifest;
        try {
          resolvedManifest = normalizeTree(await hashSkillTree(resolved.path), "resolved symlink tree");
        } catch {
          return "conflicting";
        }
        for (const relativePath of resolvedManifest.map.keys()) {
          if (!bundled.map.has(relativePath)) return "conflicting";
        }
        if (await hasIdentityEvidence(resolved.path, resolvedManifest.map)) return "unmanaged";
        return "conflicting";
      }
      return "drifted";
    }

    let resolvedManifest;
    try {
      resolvedManifest = normalizeTree(await hashSkillTree(resolved.path), "resolved symlink tree");
    } catch {
      return "conflicting";
    }
    for (const relativePath of resolvedManifest.map.keys()) {
      if (!bundled.map.has(relativePath)) return "conflicting";
    }
    if (await hasIdentityEvidence(resolved.path, resolvedManifest.map)) return "unmanaged";
    return "conflicting";
  }

  if (targetInfo.expectedKind === "symlink") return "conflicting";
  let current;
  try {
    current = await hashSkillTree(targetInfo.path);
  } catch {
    // An unreadable, special, symlinked, or otherwise unstable existing tree
    // is never safe to adopt or overwrite.
    return "conflicting";
  }
  const currentManifest = normalizeTree(current, "target tree");
  const receiptKinds = targetInfo.expectedKind
    ? [targetInfo.expectedKind]
    : ["canonical", "copy"];
  const receiptTarget = matchingReceiptTarget(
    ownershipReceipt,
    targetInfo.path,
    receiptKinds,
  );
  if (targetInfo.expectedKind && targetInfo.expectedKind !== "symlink") {
    const contradictoryTarget = matchingReceiptTarget(
      ownershipReceipt,
      targetInfo.path,
      targetInfo.expectedKind === "canonical" ? ["copy"] : ["canonical"],
    );
    if (contradictoryTarget) return "conflicting";
  }
  if (receiptTarget && recordsIdentity(receiptTarget)) {
    return compareDirectoryManifest(currentManifest.map, receiptTarget.files);
  }

  // Bundled-manifest containment is only an unmanaged/conflict rule. A
  // receipt-authorized prior version may legitimately contain files absent
  // from the new bundle during an upgrade.
  for (const relativePath of currentManifest.map.keys()) {
    if (!bundled.map.has(relativePath)) return "conflicting";
  }

  if (await hasIdentityEvidence(targetInfo.path, currentManifest.map)) return "unmanaged";
  return "conflicting";
}

export { SkillTreeError };
