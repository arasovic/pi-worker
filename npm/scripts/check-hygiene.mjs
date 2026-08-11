#!/usr/bin/env node

import {
  closeSync,
  constants,
  fstatSync,
  lstatSync,
  openSync,
  readSync,
  realpathSync,
} from "node:fs";
import { isAbsolute, join, posix, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const MAX_INVENTORY_BYTES = 4 * 1024 * 1024;
const MAX_FILE_BYTES = 1024 * 1024;
const pieces = (...values) => values.join("");
const MACHINE_HOME_MARKER = pieces("/Us", "ers/");
const CODEX_MARKER = pieces(".", "cod", "ex");
const SUPERPOWERS_MARKER = pieces(".", "super", "powers");
const PRIVATE_PATH_MARKERS = Object.freeze([CODEX_MARKER, SUPERPOWERS_MARKER]);
const PRIVATE_CONTENT_MARKERS = Object.freeze([MACHINE_HOME_MARKER, SUPERPOWERS_MARKER]);
const CODEX_CONTENT_ALLOWLIST = new Set([
  "npm/generated/skills-rules.json",
  "npm/scripts/extract-skills-rules.mjs",
  "npm/test/skill-rules.test.mjs",
]);
const DEPENDENCY_DIR = pieces("node_", "modules");
const BUILD_DIR = pieces("di", "st");
const NATIVE_DIR = pieces("npm/", "native");
const CREDENTIAL_MARKERS = Object.freeze([
  pieces("cred", "ential="),
  pieces("pass", "word="),
  pieces("sec", "ret="),
  pieces("api", "_key="),
]);
const INTERNAL_NAME = /(?:^|[/.])(?:review|work[-_]?log|work[-_]?notes)(?:[-_.][^/]*)?(?:[/.]|$)/i;
const CREDENTIAL_NAME = /(?:^|[/.])(?:.*\.env(?:\..*)?|credentials?(?:\..*)?|id_(?:rsa|ed25519)|.*\.(?:pem|key|p12|pfx|jks))$/i;
const ARCHIVE_NAME = /\.(?:tgz|tar|tar\.gz|zip|gz|bz2|xz|7z|rar)$/i;
const EXECUTABLE_NAME = /\.(?:exe|dll|so|dylib|bin|appimage)$/i;
const GIT_OUTPUT_LIMIT = 4096;

class HygieneError extends Error {
  constructor(path, rule) {
    super(`${path} [${rule}]`);
    this.path = path;
    this.rule = rule;
  }
}

function fail(path, rule) {
  throw new HygieneError(path, rule);
}

function safeInventoryPath(path) {
  if (!path || path.includes("\\") || path.includes("\0") || isAbsolute(path)) return false;
  if (/^[A-Za-z]:($|\/)/.test(path)) return false;
  if (path.startsWith("./") || path.includes("//")) return false;
  const parts = path.split("/");
  if (parts.some((part) => part === "" || part === "." || part === "..")) return false;
  if (posix.normalize(path) !== path) return false;
  return !/[\u0000-\u001f\u007f]/.test(path);
}

function gitEnvironment() {
  return Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !name.toUpperCase().startsWith("GIT_")),
  );
}

function verifiedRepositoryRoot(root, env) {
  let canonicalRoot;
  try {
    canonicalRoot = realpathSync(root);
  } catch {
    fail("inventory", "H011");
  }
  const result = spawnSync("git", ["rev-parse", "--show-toplevel"], {
    cwd: canonicalRoot,
    encoding: "utf8",
    env,
    maxBuffer: GIT_OUTPUT_LIMIT,
    windowsHide: true,
  });
  if (result.error || result.status !== 0 || result.signal || result.stderr !== "") fail("inventory", "H011");
  let reportedRoot;
  try {
    reportedRoot = realpathSync(result.stdout.trim());
  } catch {
    fail("inventory", "H011");
  }
  if (reportedRoot !== canonicalRoot) fail("inventory", "H011");
  return canonicalRoot;
}

function trackedPaths(root) {
  const env = gitEnvironment();
  const canonicalRoot = verifiedRepositoryRoot(root, env);
  const result = spawnSync("git", ["ls-files", "-z", "--"], {
    cwd: canonicalRoot,
    encoding: "buffer",
    env,
    maxBuffer: MAX_INVENTORY_BYTES,
    windowsHide: true,
  });
  if (result.error || result.status !== 0 || result.signal) fail("inventory", "H011");
  if (!Buffer.isBuffer(result.stdout) || result.stdout.length > MAX_INVENTORY_BYTES) fail("inventory", "H010");
  if (result.stdout.length === 0) return { root: canonicalRoot, paths: [] };
  if (result.stdout[result.stdout.length - 1] !== 0) fail("inventory", "H010");

  let text;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(result.stdout);
  } catch {
    fail("inventory", "H010");
  }
  const paths = text.slice(0, -1).split("\0");
  const seen = new Set();
  for (const path of paths) {
    if (!safeInventoryPath(path) || seen.has(path)) fail("inventory", "H010");
    seen.add(path);
  }
  return { root: canonicalRoot, paths };
}

function openTrackedFile(root, relativePath) {
  const absolutePath = resolve(root, relativePath);
  const parts = relativePath.split("/");
  let parent = root;
  for (const part of parts.slice(0, -1)) {
    parent = join(parent, part);
    try {
      const parentEntry = lstatSync(parent);
      if (!parentEntry.isDirectory()) fail(relativePath, "H008");
    } catch (error) {
      if (error instanceof HygieneError) throw error;
      fail(relativePath, "H008");
    }
  }
  let entry;
  try {
    entry = lstatSync(absolutePath);
  } catch {
    fail(relativePath, "H008");
  }
  if (!entry.isFile()) fail(relativePath, "H008");
  if (constants.O_NOFOLLOW === undefined) fail(relativePath, "H008");

  let descriptor;
  try {
    descriptor = openSync(absolutePath, constants.O_RDONLY | constants.O_NONBLOCK | constants.O_NOFOLLOW);
    const opened = fstatSync(descriptor);
    if (!opened.isFile() || opened.dev !== entry.dev || opened.ino !== entry.ino) fail(relativePath, "H008");
    if (!Number.isSafeInteger(opened.size) || opened.size > MAX_FILE_BYTES) fail(relativePath, "H009");
    const bytes = Buffer.alloc(opened.size);
    let offset = 0;
    while (offset < opened.size) {
      const count = readSync(descriptor, bytes, offset, opened.size - offset, null);
      if (count === 0) fail(relativePath, "H008");
      offset += count;
    }
    const current = fstatSync(descriptor);
    if (current.size !== opened.size || current.dev !== opened.dev || current.ino !== opened.ino) fail(relativePath, "H008");
    if (readSync(descriptor, Buffer.alloc(1), 0, 1, null) !== 0) fail(relativePath, "H009");
    return bytes;
  } catch (error) {
    if (error instanceof HygieneError) throw error;
    fail(relativePath, "H008");
  } finally {
    if (descriptor !== undefined) closeSync(descriptor);
  }
}

function hasBytes(bytes, text) {
  return bytes.includes(Buffer.from(text, "utf8"));
}

function hasDirectory(relativePath, directory) {
  return relativePath === directory || relativePath.startsWith(`${directory}/`) || relativePath.includes(`/${directory}/`);
}

function checkPath(relativePath) {
  if (PRIVATE_PATH_MARKERS.some((marker) => relativePath.includes(marker))) return "H002";
  if (CREDENTIAL_NAME.test(relativePath)) return "H001";
  if (hasDirectory(relativePath, DEPENDENCY_DIR) || hasDirectory(relativePath, BUILD_DIR) || hasDirectory(relativePath, NATIVE_DIR)) return "H004";
  if (ARCHIVE_NAME.test(relativePath)) return "H005";
  if (EXECUTABLE_NAME.test(relativePath)) return "H006";
  if (INTERNAL_NAME.test(relativePath)) return "H003";
  return undefined;
}

function checkContent(relativePath, bytes) {
  for (const marker of PRIVATE_CONTENT_MARKERS) {
    if (hasBytes(bytes, marker)) return "H002";
  }
  if (!CODEX_CONTENT_ALLOWLIST.has(relativePath) && hasBytes(bytes, CODEX_MARKER)) return "H002";
  if (CREDENTIAL_MARKERS.some((marker) => hasBytes(bytes, marker))) return "H007";
  return undefined;
}

export function checkRepository(root = resolve(fileURLToPath(new URL("../..", import.meta.url)))) {
  const inventory = trackedPaths(root);
  const findings = [];
  for (const relativePath of inventory.paths) {
    const pathRule = checkPath(relativePath);
    if (pathRule) {
      findings.push(new HygieneError(relativePath, pathRule));
      continue;
    }
    const bytes = openTrackedFile(inventory.root, relativePath);
    const contentRule = checkContent(relativePath, bytes);
    if (contentRule) findings.push(new HygieneError(relativePath, contentRule));
  }
  if (findings.length > 0) throw findings;
}

function main() {
  try {
    checkRepository(process.cwd());
  } catch (error) {
    const findings = Array.isArray(error) ? error : [error];
    for (const finding of findings) {
      if (finding instanceof HygieneError) console.error(finding.message);
      else console.error("inventory [H010]");
    }
    process.exitCode = 1;
  }
}

const invoked = process.argv[1] ? realpathSync(process.argv[1]) : "";
if (invoked === realpathSync(fileURLToPath(import.meta.url))) main();
