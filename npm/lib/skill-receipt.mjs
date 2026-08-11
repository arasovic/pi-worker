import { randomUUID } from "node:crypto";
import {
  chmod as chmodFile,
  lstat as lstatFile,
  mkdir as makeDirectory,
  open as openFile,
  realpath as realpathFile,
  rename as renameFile,
  stat as statFile,
  unlink as unlinkFile,
} from "node:fs/promises";
import { spawn as childProcessSpawn } from "node:child_process";
import { basename, dirname, isAbsolute, join, normalize, posix } from "node:path";

export const RECEIPT_SCHEMA_VERSION = 1;
export const RECEIPT_OUTCOMES = Object.freeze([
  "installed",
  "blocked",
  "skipped",
  "failed",
]);
export const AFFECTED_STATES = Object.freeze([
  "unmanaged",
  "drifted",
  "conflicting",
]);
export const TARGET_KINDS = Object.freeze([
  "canonical",
  "symlink",
  "copy",
]);

const TOP_LEVEL_FIELDS = Object.freeze([
  "schemaVersion",
  "installerVersion",
  "skillsVersion",
  "outcome",
  "targets",
  "affectedTargets",
  "recovery",
]);
const TARGET_FIELDS = Object.freeze(["path", "kind", "files"]);
const FILE_FIELDS = Object.freeze(["path", "sha256"]);
const AFFECTED_FIELDS = Object.freeze(["path", "state", "recovery"]);
const UNIX_PLATFORMS = new Set(["darwin", "linux"]);
const HEX_SHA256 = /^[0-9a-fA-F]{64}$/;
const MAX_NATIVE_OUTPUT_BYTES = 64 * 1024;
const DEFAULT_NATIVE_TIMEOUT_MS = 10 * 1000;
const NATIVE_CHILD_GRACE_MS = 100;

const receiptFs = Object.freeze({
  lstat: lstatFile,
  chmod: chmodFile,
  close: (handle) => handle.close(),
  mkdir: makeDirectory,
  open: openFile,
  realpath: realpathFile,
  rename: renameFile,
  stat: statFile,
  sync: (handle) => handle.sync(),
  unlink: unlinkFile,
  writeFile: (handle, data) => handle.writeFile(data, { encoding: "utf8" }),
});

function invalidReceipt(message) {
  throw new TypeError(`Invalid receipt: ${message}`);
}

function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasExactFields(value, expected, name) {
  if (!isRecord(value)) invalidReceipt(`${name} must be an object`);
  const actual = Object.keys(value);
  if (
    actual.length !== expected.length ||
    expected.some((field) => !Object.hasOwn(value, field))
  ) {
    invalidReceipt(`${name} must contain exactly ${expected.join(", ")}`);
  }
}

function requireString(value, name) {
  if (typeof value !== "string" || value.length === 0 || value.includes("\0")) {
    invalidReceipt(`${name} must be a non-empty string`);
  }
}

function requireAbsolutePath(value, name) {
  requireString(value, name);
  if (!isAbsolute(value)) invalidReceipt(`${name} must be absolute`);
}

function requireRecovery(value, name) {
  if (!Array.isArray(value)) invalidReceipt(`${name} must be an array`);
  for (const [index, command] of value.entries()) {
    requireString(command, `${name}[${index}]`);
    if (command.trim().length === 0) {
      invalidReceipt(`${name}[${index}] must not be empty`);
    }
  }
}

function requireRelativeFilePath(value, name) {
  requireString(value, name);
  if (
    isAbsolute(value) ||
    value.includes("\\") ||
    value === "." ||
    posix.normalize(value) !== value
  ) {
    invalidReceipt(`${name} must be a normalized relative POSIX path`);
  }
  const segments = value.split("/");
  if (segments.some((segment) => segment === "" || segment === "." || segment === "..")) {
    invalidReceipt(`${name} must not escape its target`);
  }
}

function mapArrayValues(array, callback) {
  const values = [];
  for (let index = 0; index < array.length; index += 1) {
    values.push(callback(array[index], index));
  }
  return values;
}

function validateFile(file, targetIndex, fileIndex) {
  const name = `targets[${targetIndex}].files[${fileIndex}]`;
  hasExactFields(file, FILE_FIELDS, name);
  requireRelativeFilePath(file.path, `${name}.path`);
  if (typeof file.sha256 !== "string" || !HEX_SHA256.test(file.sha256)) {
    invalidReceipt(`${name}.sha256 must be a SHA-256 hex string`);
  }

  return {
    path: file.path,
    sha256: file.sha256.toLowerCase(),
  };
}

function validateTarget(target, index) {
  const name = `targets[${index}]`;
  hasExactFields(target, TARGET_FIELDS, name);
  requireAbsolutePath(target.path, `${name}.path`);
  if (!TARGET_KINDS.includes(target.kind)) {
    invalidReceipt(`${name}.kind is not a recognized target kind`);
  }
  if (!Array.isArray(target.files)) invalidReceipt(`${name}.files must be an array`);
  if (target.files.length === 0) invalidReceipt(`${name}.files must not be empty`);

  const seenFiles = new Set();
  const files = mapArrayValues(target.files, (file, fileIndex) => {
    const normalizedFile = validateFile(file, index, fileIndex);
    if (seenFiles.has(normalizedFile.path)) {
      invalidReceipt(`${name}.files contains a duplicate path`);
    }
    seenFiles.add(normalizedFile.path);
    return normalizedFile;
  });

  return {
    path: target.path,
    kind: target.kind,
    files,
  };
}

function validateAffectedTarget(target, index) {
  const name = `affectedTargets[${index}]`;
  hasExactFields(target, AFFECTED_FIELDS, name);
  requireAbsolutePath(target.path, `${name}.path`);
  if (!AFFECTED_STATES.includes(target.state)) {
    invalidReceipt(`${name}.state is not a recognized affected state`);
  }
  requireRecovery(target.recovery, `${name}.recovery`);

  return {
    path: target.path,
    state: target.state,
    recovery: [...target.recovery],
  };
}

/**
 * Validate and copy the public receipt document. The copy is deliberately
 * constructed field by field so upstream command output cannot be persisted.
 */
export function validateReceipt(receipt) {
  hasExactFields(receipt, TOP_LEVEL_FIELDS, "receipt");
  if (receipt.schemaVersion !== RECEIPT_SCHEMA_VERSION) {
    invalidReceipt(`schemaVersion must be ${RECEIPT_SCHEMA_VERSION}`);
  }
  requireString(receipt.installerVersion, "installerVersion");
  requireString(receipt.skillsVersion, "skillsVersion");
  if (!RECEIPT_OUTCOMES.includes(receipt.outcome)) {
    invalidReceipt("outcome is not recognized");
  }
  if (!Array.isArray(receipt.targets)) invalidReceipt("targets must be an array");
  if (!Array.isArray(receipt.affectedTargets)) {
    invalidReceipt("affectedTargets must be an array");
  }
  requireRecovery(receipt.recovery, "recovery");
  if (receipt.outcome === "installed" && receipt.targets.length === 0) {
    invalidReceipt("installed outcome requires at least one target");
  }

  const seenTargets = new Set();
  const targets = mapArrayValues(receipt.targets, (target, index) => {
    const normalizedTarget = validateTarget(target, index);
    const key = normalize(normalizedTarget.path);
    if (seenTargets.has(key)) invalidReceipt("targets contains a duplicate path");
    seenTargets.add(key);
    return normalizedTarget;
  });

  const seenAffectedTargets = new Set();
  const affectedTargets = mapArrayValues(receipt.affectedTargets, (target, index) => {
    const normalizedTarget = validateAffectedTarget(target, index);
    const key = normalize(normalizedTarget.path);
    if (seenAffectedTargets.has(key)) {
      invalidReceipt("affectedTargets contains a duplicate path");
    }
    seenAffectedTargets.add(key);
    return normalizedTarget;
  });

  if (receipt.outcome === "installed" && affectedTargets.length !== 0) {
    invalidReceipt("installed outcome must not contain affectedTargets");
  }
  if (receipt.outcome === "blocked" && affectedTargets.length === 0) {
    invalidReceipt("blocked outcome requires affectedTargets");
  }

  return {
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    installerVersion: receipt.installerVersion,
    skillsVersion: receipt.skillsVersion,
    outcome: receipt.outcome,
    targets,
    affectedTargets,
    recovery: [...receipt.recovery],
  };
}

function nativeReceiptError(message) {
  return new Error(`Unable to resolve native receipt path: ${message}`);
}

function collectStream(stream, chunks, onOverflow) {
  let size = 0;
  const append = (chunk) => {
    const data = Buffer.isBuffer(chunk)
      ? chunk
      : chunk instanceof Uint8Array
        ? Buffer.from(chunk)
        : Buffer.from(String(chunk));
    if (data.length > MAX_NATIVE_OUTPUT_BYTES - size) {
      onOverflow();
      return;
    }
    chunks.push(data);
    size += data.length;
  };

  if (typeof stream === "string" || Buffer.isBuffer(stream) || stream instanceof Uint8Array) {
    append(stream);
    return () => {};
  }
  if (!stream || typeof stream.on !== "function") return () => {};
  const listener = (chunk) => append(chunk);
  stream.on("data", listener);
  return () => {
    if (typeof stream.removeListener === "function") stream.removeListener("data", listener);
  };
}

function addChildListener(child, event, listener) {
  if (typeof child.once === "function") {
    child.once(event, listener);
    return true;
  }
  if (typeof child.on === "function") {
    child.on(event, listener);
    return true;
  }
  return false;
}

function exactObjectFields(value, fields) {
  return isRecord(value) && Object.keys(value).length === fields.length && fields.every((field) => Object.hasOwn(value, field));
}

function parseNativeReceiptPath(stdout, stderr) {
  if (stderr.length !== 0) {
    throw nativeReceiptError("native output contained unexpected stderr");
  }

  const text = stdout.toString("utf8").trim();
  if (text.length === 0) throw nativeReceiptError("native output was empty");

  let document;
  try {
    document = JSON.parse(text);
  } catch (error) {
    throw nativeReceiptError("native output was not exactly one JSON document");
  }

  if (!exactObjectFields(document, ["schemaVersion", "receiptPath"])) {
    throw nativeReceiptError("native JSON document has an unexpected schema");
  }
  if (document.schemaVersion !== RECEIPT_SCHEMA_VERSION) {
    throw nativeReceiptError(`native schemaVersion must be ${RECEIPT_SCHEMA_VERSION}`);
  }
  if (
    typeof document.receiptPath !== "string" ||
    document.receiptPath.length === 0 ||
    document.receiptPath.includes("\0") ||
    !isAbsolute(document.receiptPath)
  ) {
    throw nativeReceiptError("native receiptPath must be absolute");
  }

  return document.receiptPath;
}

/**
 * Ask the staged native executable for the receipt path without invoking a
 * shell. The child inherits the normal host environment so the Go binary can
 * locate HOME/XDG_CONFIG_HOME; no environment values are logged or persisted.
 */
export async function receiptPathFromNative({
  binary,
  spawn = childProcessSpawn,
  timeoutMs = undefined,
  timeout = undefined,
} = {}) {
  if (typeof binary !== "string" || binary.length === 0) {
    throw new TypeError("receiptPathFromNative requires a native binary");
  }
  if (typeof spawn !== "function") {
    throw new TypeError("receiptPathFromNative requires a spawn function");
  }
  const configuredTimeout = timeoutMs ?? timeout ?? DEFAULT_NATIVE_TIMEOUT_MS;
  if (!Number.isFinite(configuredTimeout) || configuredTimeout < 0) {
    throw new TypeError("receiptPathFromNative timeout must be a non-negative number");
  }

  const stdout = [];
  const stderr = [];
  const args = ["skill", "receipt-path", "--json"];

  return new Promise((resolve, reject) => {
    let child;
    try {
      child = spawn(binary, args, {
        stdio: ["ignore", "pipe", "pipe"],
        shell: false,
        windowsHide: true,
      });
    } catch {
      reject(nativeReceiptError("native process could not be started"));
      return;
    }

    if (!child || (typeof child.on !== "function" && typeof child.once !== "function")) {
      reject(nativeReceiptError("native process did not provide a child process"));
      return;
    }

    let settled = false;
    let stopping = false;
    let stopError = null;
    let timer = null;
    let graceTimer = null;
    let fallbackTimer = null;
    const childListeners = [];
    const streamCleanups = [];
    const removeListeners = () => {
      for (const { event, listener } of childListeners) {
        if (typeof child.removeListener === "function") child.removeListener(event, listener);
      }
      for (const cleanup of streamCleanups) cleanup();
      if (timer !== null) {
        clearTimeout(timer);
        timer = null;
      }
      if (graceTimer !== null) {
        clearTimeout(graceTimer);
        graceTimer = null;
      }
      if (fallbackTimer !== null) {
        clearTimeout(fallbackTimer);
        fallbackTimer = null;
      }
    };
    const finish = (callback) => {
      if (settled) return false;
      settled = true;
      removeListeners();
      callback();
      return true;
    };
    const killChild = (signal) => {
      try {
        if (typeof child.kill === "function") child.kill(signal);
      } catch {
        // The timeout/overflow is already a failed operation.
      }
    };
    const addListener = (event, listener) => {
      if (!addChildListener(child, event, listener)) return false;
      childListeners.push({ event, listener });
      return true;
    };
    const beginStop = (error) => {
      if (settled || stopping) return;
      stopping = true;
      stopError = error;
      killChild("SIGTERM");
      graceTimer = setTimeout(() => {
        killChild("SIGKILL");
        fallbackTimer = setTimeout(() => {
          finish(() => reject(nativeReceiptError(`${error.message}; native cleanup incomplete`)));
        }, NATIVE_CHILD_GRACE_MS);
      }, NATIVE_CHILD_GRACE_MS);
    };
    const overflow = (streamName) => {
      beginStop(nativeReceiptError(`native ${streamName} exceeded the ${MAX_NATIVE_OUTPUT_BYTES}-byte limit`));
    };

    addListener("error", () => {
      if (stopping) return;
      finish(() => reject(nativeReceiptError("native process could not be started")));
    });
    addListener("close", (code, signal) => {
      finish(() => {
        if (stopping) {
          reject(stopError);
          return;
        }
        if (signal) {
          reject(nativeReceiptError(`native process terminated by ${signal}`));
          return;
        }
        if (code !== 0) {
          reject(nativeReceiptError(`native process exited with code ${String(code)}`));
          return;
        }
        try {
          resolve(parseNativeReceiptPath(Buffer.concat(stdout), Buffer.concat(stderr)));
        } catch (error) {
          reject(error);
        }
      });
    });

    const stdoutCleanup = collectStream(child.stdout, stdout, () => overflow("stdout"));
    streamCleanups.push(stdoutCleanup);
    if (settled) {
      stdoutCleanup();
      return;
    }
    const stderrCleanup = collectStream(child.stderr, stderr, () => overflow("stderr"));
    streamCleanups.push(stderrCleanup);
    if (settled) {
      stderrCleanup();
      return;
    }

    timer = setTimeout(() => {
      beginStop(nativeReceiptError(`native process timeout after ${configuredTimeout} ms`));
    }, configuredTimeout);
  });
}

function operationOverrides(options) {
  if (!options || typeof options !== "object") return {};
  const candidate = options.fs && typeof options.fs === "object" ? options.fs : options;
  return Object.fromEntries(
    Object.keys(receiptFs)
      .filter((key) => typeof candidate[key] === "function")
      .map((key) => [key, candidate[key]])
  );
}

async function cleanupTemporary(ops, temporaryPath, created) {
  if (!created) return;
  try {
    await ops.unlink(temporaryPath);
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
}

async function closeHandle(handle, priorError, closeOperation) {
  if (!handle) return priorError;
  try {
    await closeOperation(handle);
  } catch (error) {
    return priorError ?? error;
  }
  return priorError;
}

/**
 * Persist a validated receipt through a same-directory temporary file and an
 * atomic rename. The optional third argument is a private test seam for file
 * operation failure injection; normal callers use the two-argument API.
 */
function directoryIdentity(info) {
  if (typeof info?.dev !== "number" || typeof info?.ino !== "number") return null;
  return `${info.dev}:${info.ino}`;
}

function assertReceiptParentDirectory(info, displayPath) {
  if (info?.isSymbolicLink?.()) {
    throw new Error(`receipt parent must not be a symlink: ${displayPath}`);
  }
  if (!info?.isDirectory?.()) {
    throw new Error(`receipt parent is not a directory: ${displayPath}`);
  }
}

export async function writeReceipt(path, receipt, options = undefined) {
  requireAbsolutePath(path, "receipt path");
  const document = validateReceipt(receipt);
  const serialized = `${JSON.stringify(document)}\n`;
  const parent = dirname(path);
  const ops = { ...receiptFs, ...operationOverrides(options) };
  const platform = options && typeof options === "object" && typeof options.platform === "string"
    ? options.platform
    : process.platform;

  // This is intentionally before opening the temporary file. In particular,
  // a chmod failure cannot leave a partially written receipt or replace an
  // existing one.
  await ops.mkdir(parent, { recursive: true, mode: 0o700 });
  const parentInfo = await ops.lstat(parent);
  assertReceiptParentDirectory(parentInfo, parent);
  const realParent = await ops.realpath(parent);
  const initialIdentity = directoryIdentity(await ops.stat(realParent));
  if (initialIdentity === null) {
    throw new Error(`receipt parent identity is unavailable: ${realParent}`);
  }
  if (UNIX_PLATFORMS.has(platform)) {
    await ops.chmod(realParent, 0o700);
  }

  const temporaryPath = join(
    realParent,
    `.${basename(path)}.${process.pid}.${randomUUID()}.tmp`
  );
  const destinationPath = join(realParent, basename(path));
  let handle = null;
  let created = false;

  try {
    handle = await ops.open(temporaryPath, "wx", 0o600);
    created = true;
    await ops.chmod(temporaryPath, 0o600);

    let writeError = null;
    try {
      await ops.writeFile(handle, serialized);
      await ops.sync(handle);
    } catch (error) {
      writeError = error;
    }
    writeError = await closeHandle(handle, writeError, ops.close);
    handle = null;
    if (writeError) throw writeError;

    // Recheck both the path's final component and the directory identity just
    // before the atomic rename. The temporary and destination names are both
    // in the captured real directory, never in a later path alias.
    const currentParentInfo = await ops.lstat(parent);
    assertReceiptParentDirectory(currentParentInfo, parent);
    const currentRealParent = await ops.realpath(parent);
    if (normalize(currentRealParent) !== normalize(realParent)) {
      throw new Error("receipt parent changed while writing");
    }
    const currentIdentity = directoryIdentity(await ops.stat(realParent));
    if (currentIdentity !== initialIdentity) {
      throw new Error("receipt parent directory identity changed while writing");
    }

    await ops.rename(temporaryPath, destinationPath);
    created = false;
  } catch (error) {
    const closeError = await closeHandle(handle, error, ops.close);
    handle = null;
    try {
      await cleanupTemporary(ops, temporaryPath, created);
    } catch (cleanupError) {
      throw new AggregateError([closeError, cleanupError], "Unable to clean up temporary receipt");
    }
    throw closeError;
  }
}
