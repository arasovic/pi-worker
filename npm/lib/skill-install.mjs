import { createHash } from "node:crypto";
import { createRequire } from "node:module";
import { spawn as childProcessSpawn } from "node:child_process";
import { constants, existsSync, readFileSync } from "node:fs";
import {
  lstat as lstatAsync,
  open as openAsync,
  readlink as readlinkAsync,
  realpath as realpathAsync,
} from "node:fs/promises";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";
import path from "node:path";

import { nativePath, nativeTarget } from "./native.mjs";
import {
  receiptPathFromNative,
  validateReceipt,
  writeReceipt,
} from "./skill-receipt.mjs";
import {
  classifyTarget,
  hashSkillTree,
  IDENTITY_CONTENT,
  IDENTITY_FILE,
  inspectSkillIdentity,
} from "./skill-tree.mjs";
import {
  detectInstalledAgents,
  loadRules,
  PINNED_SKILLS_VERSION,
  resolveAgentTarget,
  resolveAllTargets,
} from "./skill-rules.mjs";

const SKILL_NAME = "pi-worker";
const OUTPUT_LIMIT = 64 * 1024;
const DEFAULT_TIMEOUT_MS = 30 * 1000;
const GLOBAL_REMOVE = "npx --yes skills@1.5.22 remove pi-worker -g -y";
const GLOBAL_RETRY = "npm install -g --foreground-scripts pi-worker";
const MAX_RECEIPT_BYTES = 1024 * 1024;
const CHILD_GRACE_MS = 100;

const packageRootFromUrl = fileURLToPath(new URL("../..", import.meta.url));

function packageVersion(root) {
  try {
    const packageJson = JSON.parse(readFileSync(path.join(root, "package.json"), "utf8"));
    return typeof packageJson.version === "string" && packageJson.version.length > 0
      ? packageJson.version
      : "dev";
  } catch {
    return "dev";
  }
}

function result(outcome, diagnostic) {
  return { outcome, diagnostic };
}

function isAbsolute(value) {
  return typeof value === "string" && value.length > 0 && path.isAbsolute(value) && !value.includes("\0");
}

function sortPaths(paths) {
  return [...paths].sort((left, right) => left < right ? -1 : left > right ? 1 : 0);
}

function pathKey(value, platform) {
  const normalized = path.normalize(value);
  return platform === "win32" ? normalized.toLowerCase() : normalized;
}

function samePath(left, right, platform) {
  return pathKey(left, platform) === pathKey(right, platform);
}

function receiptTracksPath(receipt, targetPath, platform) {
  if (!receipt) return false;
  return receipt.targets.some((target) => {
    if (target.kind === "symlink") {
      return target.files.some((file) => samePath(path.join(target.path, file.path), targetPath, platform));
    }
    return samePath(target.path, targetPath, platform);
  });
}

function treeFiles(tree) {
  const source = Array.isArray(tree)
    ? tree
    : (tree?.files ?? tree?.manifest ?? tree?.hashes ?? []);
  return source.map((file) => ({
    path: file.path,
    sha256: file.sha256.toLowerCase(),
  })).sort((left, right) => left.path < right.path ? -1 : left.path > right.path ? 1 : 0);
}

function treesEqual(left, right) {
  return JSON.stringify(treeFiles(left)) === JSON.stringify(treeFiles(right));
}

function temporaryReceipt(version) {
  return {
    schemaVersion: 1,
    installerVersion: version,
    skillsVersion: PINNED_SKILLS_VERSION,
    outcome: "skipped",
    targets: [],
    affectedTargets: [],
    recovery: [],
  };
}

function failedReceipt(version, targets = []) {
  return {
    schemaVersion: 1,
    installerVersion: version,
    skillsVersion: PINNED_SKILLS_VERSION,
    outcome: "failed",
    targets: [...targets],
    affectedTargets: [],
    recovery: [GLOBAL_RETRY],
  };
}

async function persist(writer, receiptPath, document, options) {
  await writer(receiptPath, document, options);
}

function sameFileIdentity(left, right) {
  return typeof left?.dev === "number" && typeof left?.ino === "number" &&
    typeof right?.dev === "number" && typeof right?.ino === "number" &&
    left.dev === right.dev && left.ino === right.ino;
}

async function readPriorReceipt(receiptPath) {
  let handle;
  try {
    const before = await lstatAsync(receiptPath);
    if (before.isSymbolicLink() || !before.isFile() ||
      !Number.isSafeInteger(before.size) || before.size < 0 || before.size > MAX_RECEIPT_BYTES) {
      return null;
    }
    let flags = constants.O_RDONLY;
    if (typeof constants.O_NOFOLLOW === "number") flags |= constants.O_NOFOLLOW;
    handle = await openAsync(receiptPath, flags);
    const opened = await handle.stat();
    if (!opened.isFile() || !sameFileIdentity(before, opened) ||
      opened.size !== before.size || opened.size > MAX_RECEIPT_BYTES) return null;

    const chunks = [];
    let offset = 0;
    while (offset < opened.size) {
      const length = Math.min(64 * 1024, opened.size - offset);
      const buffer = Buffer.alloc(length);
      const read = await handle.read(buffer, 0, length, offset);
      if (read.bytesRead <= 0) return null;
      chunks.push(read.bytesRead === length ? buffer : buffer.subarray(0, read.bytesRead));
      offset += read.bytesRead;
    }
    const afterHandle = await handle.stat();
    const afterPath = await lstatAsync(receiptPath);
    if (!afterHandle.isFile() || !sameFileIdentity(opened, afterHandle) ||
      afterHandle.size !== opened.size || afterPath.isSymbolicLink() ||
      !afterPath.isFile() || !sameFileIdentity(before, afterPath) ||
      afterPath.size !== before.size) return null;

    // JSON.parse rejects malformed and multiple JSON documents while allowing
    // ordinary trailing whitespace. The parsed value is copied by validation.
    return validateReceipt(JSON.parse(Buffer.concat(chunks, opened.size).toString("utf8")));
  } catch {
    return null;
  } finally {
    if (handle) {
      try { await handle.close(); } catch { /* no ownership evidence */ }
    }
  }
}

function affectedRecovery(targetPath, state) {
  const inspect = `Inspect and back up ${targetPath} before retrying.`;
  if (state === "conflicting") {
    return [inspect, `Move ${targetPath} only after inspecting and backing it up, then retry.`];
  }
  return [inspect];
}

function blockedReceipt(version, prior, affected) {
  const states = affected.map(({ path: targetPath, state }) => ({
    path: targetPath,
    state,
    recovery: affectedRecovery(targetPath, state),
  }));
  const hasConflict = states.some((entry) => entry.state === "conflicting");
  const allNonConflict = states.length > 0 && states.every((entry) => (
    entry.state === "unmanaged" || entry.state === "drifted"
  ));
  return {
    schemaVersion: 1,
    installerVersion: version,
    skillsVersion: PINNED_SKILLS_VERSION,
    outcome: "blocked",
    targets: prior?.targets ?? [],
    affectedTargets: states.sort((left, right) => left.path < right.path ? -1 : left.path > right.path ? 1 : 0),
    recovery: !hasConflict && allNonConflict ? [GLOBAL_REMOVE, GLOBAL_RETRY] : [],
  };
}

function targetsFromResolved(resolved, cwd, platform) {
  const candidates = resolved.filter((target) => typeof target === "string")
    .map((target) => path.resolve(cwd, target, SKILL_NAME));
  const seen = new Set();
  const targets = [];
  for (const target of candidates) {
    const key = pathKey(target, platform);
    if (!seen.has(key)) {
      seen.add(key);
      targets.push(target);
    }
  }
  return sortPaths(targets);
}

function targetList({ home, cwd, rules, runtime, resolveTargets, platform }) {
  const resolved = resolveTargets(rules, runtime) ?? [];
  return targetsFromResolved([...resolved, path.join(home, ".agents", "skills")], cwd, platform);
}

function requiredTargetList({ rules, agentIds, runtime, cwd, platform }) {
  const agentsById = new Map(rules.agents.map((agent) => [agent.id, agent]));
  const resolved = agentIds.map((id) => {
    const agent = agentsById.get(id);
    if (!agent) throw new Error("detected agent is missing from the rules");
    return resolveAgentTarget(agent, runtime);
  });
  return targetsFromResolved(resolved, cwd, platform);
}

function captureChild(spawn, binary, args, options, timeoutMs) {
  return new Promise((resolve) => {
    let child;
    let settled = false;
    let stopping = false;
    let stopReason = "process failed";
    let timer = null;
    let graceTimer = null;
    let fallbackTimer = null;
    let stdoutBytes = 0;
    let stderrBytes = 0;
    const stdout = [];
    const stderr = [];
    const listeners = [];
    const streamCleanups = [];

    const cleanup = () => {
      if (timer !== null) clearTimeout(timer);
      if (graceTimer !== null) clearTimeout(graceTimer);
      if (fallbackTimer !== null) clearTimeout(fallbackTimer);
      timer = graceTimer = fallbackTimer = null;
      for (const { event, listener } of listeners) child?.removeListener?.(event, listener);
      for (const cleanupStream of streamCleanups) cleanupStream();
    };
    const finish = (value) => {
      if (settled) return;
      settled = true;
      cleanup();
      resolve({ ...value, stdout: Buffer.concat(stdout), stderr: Buffer.concat(stderr) });
    };
    const signalChild = (signal) => {
      try {
        // A detached Unix child is preferably treated as a process group, but
        // retain child.kill for portable implementations and test doubles.
        let groupSignalled = false;
        if (process.platform !== "win32" && Number.isInteger(child?.pid) && child.pid > 0) {
          try {
            process.kill(-child.pid, signal);
            groupSignalled = true;
          } catch {
            // Fall back to the child handle below.
          }
        }
        if (!groupSignalled && typeof child?.kill === "function") child.kill(signal);
      } catch {
        // Continue to the bounded escalation/fallback path.
      }
    };
    const beginStop = (reason) => {
      if (stopping || settled) return;
      stopping = true;
      stopReason = reason;
      signalChild("SIGTERM");
      graceTimer = setTimeout(() => {
        signalChild("SIGKILL");
        fallbackTimer = setTimeout(() => {
          // Real child processes should emit close after SIGKILL. This final
          // bounded path prevents a broken child test double from hanging the
          // installer and never includes its output.
          finish({ ok: false, reason: `${reason}; child cleanup incomplete` });
        }, CHILD_GRACE_MS);
      }, CHILD_GRACE_MS);
    };
    const append = (streamName, chunk) => {
      if (stopping || settled) return;
      const data = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk instanceof Uint8Array ? chunk : String(chunk));
      const total = streamName === "stdout" ? stdoutBytes : stderrBytes;
      if (data.length > OUTPUT_LIMIT - total) {
        beginStop(`${streamName} output exceeded the limit`);
        return;
      }
      if (streamName === "stdout") {
        stdoutBytes += data.length;
        stdout.push(data);
      } else {
        stderrBytes += data.length;
        stderr.push(data);
      }
    };
    const attachStream = (stream, name, chunks) => {
      if (!stream?.on) return;
      const listener = (chunk) => append(name, chunk);
      stream.on("data", listener);
      streamCleanups.push(() => stream.removeListener?.("data", listener));
      // Keep the local chunks reference explicit so the bounded capture is
      // visibly in-memory and never routed to a log or file.
      void chunks;
    };

    try {
      child = spawn(binary, args, options);
    } catch {
      finish({ ok: false, reason: "process could not be started" });
      return;
    }
    if (!child || typeof child.on !== "function") {
      finish({ ok: false, reason: "process did not provide a child process" });
      return;
    }

    const onError = () => finish({ ok: false, reason: "process could not be started" });
    const onClose = (code, signal) => {
      if (stopping) finish({ ok: false, reason: stopReason });
      else if (signal) finish({ ok: false, reason: "process terminated" });
      else if (code !== 0) finish({ ok: false, reason: "process exited unsuccessfully" });
      else finish({ ok: true });
    };
    const addChildListener = (event, listener) => {
      if (typeof child.once === "function") child.once(event, listener);
      else child.on(event, listener);
    };
    addChildListener("error", onError);
    addChildListener("close", onClose);
    listeners.push({ event: "error", listener: onError }, { event: "close", listener: onClose });
    attachStream(child.stdout, "stdout", stdout);
    attachStream(child.stderr, "stderr", stderr);
    timer = setTimeout(() => beginStop("process timed out"), timeoutMs);
  });
}

async function inspectInstalledTargets(
  targets,
  requiredTargets,
  canonical,
  bundledTree,
  platform,
  hashTree,
  priorReceipt,
  initialStates,
) {
  const records = [];
  const symlinkRecords = new Map();
  const requiredKeys = new Set(requiredTargets.map((target) => pathKey(target, platform)));
  const missingRequired = [];
  let postconditionFailed = false;
  let canonicalReal;
  let canonicalVerified = false;

  try {
    const info = await lstatAsync(canonical);
    if (!info.isDirectory() || info.isSymbolicLink()) throw new Error("canonical target is unsafe");
    const canonicalTree = await hashTree(canonical);
    const marker = treeFiles(canonicalTree).find((file) => file.path === IDENTITY_FILE);
    const markerDigest = createHash("sha256").update(Buffer.from(IDENTITY_CONTENT, "utf8")).digest("hex");
    if (!marker || marker.sha256 !== markerDigest) throw new Error("canonical identity marker is invalid");
    if (!treesEqual(canonicalTree, bundledTree)) throw new Error("canonical target does not match the bundle");
    canonicalReal = await realpathAsync(canonical);
    canonicalVerified = true;
    records.push({ path: canonical, kind: "canonical", files: treeFiles(bundledTree) });
  } catch {
    postconditionFailed = true;
  }

  const priorCopy = (target) => priorReceipt?.targets.find((candidate) =>
    candidate.kind === "copy" && samePath(candidate.path, target, platform));
  for (const target of targets) {
    let info;
    try {
      info = await lstatAsync(target);
    } catch (error) {
      if (error?.code === "ENOENT") {
        if (requiredKeys.has(pathKey(target, platform))) missingRequired.push(target);
        if (requiredKeys.has(pathKey(target, platform)) || initialStates.get(target)?.state === "owned") {
          postconditionFailed = true;
        }
        continue;
      }
      postconditionFailed = true;
      continue;
    }

    if (samePath(target, canonical, platform)) continue;
    if (info.isSymbolicLink()) {
      let destination;
      let resolved;
      try {
        if (!canonicalVerified) throw new Error("canonical target is unavailable");
        destination = await readlinkAsync(target, { encoding: "buffer" });
        resolved = await realpathAsync(target);
        if (!samePath(resolved, canonicalReal, platform)) throw new Error("symlink does not resolve to canonical");
        if (!treesEqual(await hashTree(resolved), bundledTree)) throw new Error("symlink destination drifted");
      } catch {
        postconditionFailed = true;
        continue;
      }
      const parent = path.dirname(target);
      const files = symlinkRecords.get(parent) ?? [];
      files.push({ path: path.basename(target), sha256: createHash("sha256").update(destination).digest("hex") });
      symlinkRecords.set(parent, files);
      continue;
    }
    if (!info.isDirectory()) {
      postconditionFailed = true;
      continue;
    }
    let currentTree;
    try {
      currentTree = await hashTree(target);
    } catch {
      postconditionFailed = true;
      continue;
    }
    let files;
    if (treesEqual(currentTree, bundledTree)) {
      files = treeFiles(bundledTree);
    } else {
      const previous = initialStates.get(target)?.state === "owned" ? priorCopy(target) : null;
      if (!previous || !treesEqual(currentTree, { files: previous.files })) {
        postconditionFailed = true;
        continue;
      }
      files = treeFiles(previous.files);
    }
    records.push({ path: target, kind: "copy", files });
  }

  for (const [parent, files] of symlinkRecords) {
    records.push({ path: parent, kind: "symlink", files: files.sort((left, right) => left.path < right.path ? -1 : left.path > right.path ? 1 : 0) });
  }
  records.sort((left, right) => left.path < right.path ? -1 : left.path > right.path ? 1 : 0);
  return { records, missingRequired, postconditionFailed };
}

async function expectedTargetKind(target, canonical, platform) {
  if (samePath(target, canonical, platform)) return "canonical";
  try {
    const info = await lstatAsync(target);
    if (info.isSymbolicLink()) return "symlink";
    if (info.isDirectory()) return "copy";
    return undefined;
  } catch {
    return undefined;
  }
}

function stripAnsi(value) {
  return value.replace(/\u001B\[[0-?]*[ -/]*[@-~]/g, "");
}

function hasInvalidFailureProse(childResult) {
  const text = stripAnsi(Buffer.concat([childResult.stdout, childResult.stderr]).toString("utf8"));
  const aggregate = text.match(/Failed\s+to\s+install\s+(\d+)/i);
  if (!aggregate) return false;
  const details = [...text.matchAll(/^\s*✗\s+pi-worker\s+→\s+([^:\r\n]+):/gmu)]
    .map((match) => match[1].trim());
  const allowed = new Set(["PromptScript", "Eve"]);
  return Number(aggregate[1]) !== details.length || details.some((name) => !allowed.has(name));
}

async function verifyReceiptTargets(receipt, targets, classify) {
  const verified = [];
  for (const target of targets) {
    let verifiedTarget = true;
    const actualPaths = target.kind === "symlink"
      ? target.files.map((file) => path.join(target.path, file.path))
      : [target.path];
    for (const targetPath of actualPaths) {
      try {
        if (await classify({
          target: { path: targetPath, expectedKind: target.kind },
          bundledTree: receipt.__bundledTree,
          receipt: receipt.document,
        }) !== "owned") {
          verifiedTarget = false;
        }
      } catch {
        verifiedTarget = false;
      }
    }
    if (verifiedTarget) verified.push(target);
  }
  return verified;
}

/** Install the bundled pi-worker skill conservatively and record its topology. */
export async function installSkill(options = {}) {
  const platform = options.platform ?? process.platform;
  const env = options.env ?? process.env;
  const home = options.home ?? env.HOME ?? env.USERPROFILE ?? homedir();
  const cwd = options.cwd ?? process.cwd();
  const packageRoot = options.packageRoot ?? packageRootFromUrl;
  const version = options.version ?? packageVersion(packageRoot);
  const bundledSkill = options.bundledSkill ?? path.join(packageRoot, "skills", SKILL_NAME);
  const resolver = options.receiptPathFromNative ?? receiptPathFromNative;
  const writer = options.writeReceipt ?? writeReceipt;
  const classify = options.classifyTarget ?? classifyTarget;
  const hash = options.hashSkillTree ?? hashSkillTree;
  const inspectIdentity = options.inspectSkillIdentity ?? inspectSkillIdentity;
  const load = options.loadRules ?? loadRules;
  const resolveTargets = options.resolveAllTargets ?? resolveAllTargets;
  const spawn = options.spawn ?? childProcessSpawn;

  let binary = options.binary;
  try {
    if (!binary) {
      const selected = options.nativeTarget ?? nativeTarget(platform, options.arch ?? process.arch);
      binary = nativePath(packageRoot, selected.platform, selected.arch);
    }
  } catch {
    return result("skipped", "Unable to prepare the native skill installer.");
  }

  let receiptPath;
  try {
    const resolverOptions = { binary };
    if (options.nativeSpawn) resolverOptions.spawn = options.nativeSpawn;
    if (options.nativeTimeoutMs !== undefined) resolverOptions.timeoutMs = options.nativeTimeoutMs;
    receiptPath = await resolver(resolverOptions);
    if (!isAbsolute(receiptPath)) throw new Error("invalid receipt path");
  } catch {
    return result("skipped", "Unable to resolve the skill installation receipt.");
  }

  const priorReceipt = await readPriorReceipt(receiptPath);
  try {
    await persist(writer, receiptPath, temporaryReceipt(version), options.receiptWriteOptions);
  } catch {
    return result("skipped", "Unable to prepare the skill installation receipt.");
  }

  const failAfterGuard = async (diagnostic = "Skill installation failed.", targets = []) => {
    try {
      await persist(writer, receiptPath, failedReceipt(version, targets), options.receiptWriteOptions);
    } catch {
      // The diagnostic remains deliberately generic if persistence also fails.
    }
    return result("failed", diagnostic);
  };

  let rules;
  let bundledTree;
  let targets;
  const runtime = {
    env,
    home,
    platform,
    exists: (candidate) => existsSync(candidate),
  };
  try {
    rules = load(options.rulesPath ?? path.join(packageRoot, "npm", "generated", "skills-rules.json"));
    targets = targetList({ home, cwd, rules, runtime, resolveTargets, platform });
    bundledTree = await hash(bundledSkill);
  } catch {
    return failAfterGuard("Unable to inspect the bundled skill.");
  }

  const canonical = path.join(home, ".agents", "skills", SKILL_NAME);
  const states = [];
  const initialStates = new Map();
  try {
    for (const target of targets) {
      const expectedKind = await expectedTargetKind(target, canonical, platform);
      let state = await classify({
        target: { path: target, expectedKind },
        bundledTree,
        receipt: priorReceipt,
      });
      if (state !== "owned" && !receiptTracksPath(priorReceipt, target, platform)) {
        try {
          const identity = await inspectIdentity(target);
          if (identity === "current" || identity === "legacy") {
            state = `external-${identity}`;
          }
        } catch {
          // The conservative classifier already marks unreadable or unsafe
          // content as conflicting; identity inspection cannot weaken it.
        }
      }
      const entry = { path: target, state, expectedKind };
      states.push(entry);
      initialStates.set(target, entry);
    }
  } catch {
    return failAfterGuard("Unable to inspect existing skill targets.");
  }
  const persistBlocked = async (currentStates) => {
    const affected = currentStates.filter(({ state }) => (
      state !== "absent" && state !== "owned" && !state.startsWith("external-")
    ));
    if (affected.length === 0) return failAfterGuard("Skill installation preflight changed.");
    const document = blockedReceipt(version, priorReceipt, affected);
    try {
      await persist(writer, receiptPath, document, options.receiptWriteOptions);
    } catch {
      return failAfterGuard("Unable to record the blocked skill installation.");
    }
    return { ...result("blocked", "Skill installation is blocked."), affectedTargets: document.affectedTargets };
  };
  if (states.some(({ state }) => (
    state !== "absent" && state !== "owned" && !state.startsWith("external-")
  ))) {
    return persistBlocked(states);
  }
  if (states.some(({ state }) => state.startsWith("external-"))) {
    try {
      await persist(
        writer,
        receiptPath,
        priorReceipt ?? temporaryReceipt(version),
        options.receiptWriteOptions,
      );
    } catch {
      return failAfterGuard("Unable to preserve external skill ownership.");
    }
    return result("skipped", "Recognized external skill preserved.");
  }

  let cli;
  try {
    const resolveCLI = options.resolveCLI ?? (() => createRequire(import.meta.url).resolve("skills/bin/cli.mjs"));
    cli = options.cli ?? resolveCLI();
  } catch {
    return failAfterGuard("Unable to resolve the package-local skills CLI.");
  }

  // Recheck both topology expectations and classifications at the last safe
  // point before spawning. No target mutation occurs on a mismatch.
  const rechecked = [];
  try {
    for (const target of targets) {
      const expectedKind = initialStates.get(target)?.expectedKind;
      const state = await classify({
        target: { path: target, expectedKind },
        bundledTree,
        receipt: priorReceipt,
      });
      rechecked.push({ path: target, state, expectedKind });
    }
  } catch {
    return failAfterGuard("Unable to recheck existing skill targets.");
  }
  const changed = states.length !== rechecked.length || states.some((entry, index) => (
    entry.path !== rechecked[index].path || entry.state !== rechecked[index].state ||
    entry.expectedKind !== rechecked[index].expectedKind
  ));
  if (changed) return persistBlocked(rechecked);

  let agentIds;
  let requiredTargets;
  try {
    const detected = detectInstalledAgents(rules, {
      env,
      home,
      cwd,
      platform,
      exists: (candidate) => existsSync(candidate),
    });
    const agentsById = new Map(rules.agents.map((agent) => [agent.id, agent]));
    const detectedGlobalAgentIds = detected.filter((id) => agentsById.get(id)?.rule.kind !== "no-global-target");
    requiredTargets = requiredTargetList({
      rules,
      agentIds: detectedGlobalAgentIds,
      runtime,
      cwd,
      platform,
    });
    const preflightKeys = new Set(targets.map((target) => pathKey(target, platform)));
    if (requiredTargets.some((target) => !preflightKeys.has(pathKey(target, platform)))) {
      throw new Error("detected target is outside the conservative inventory");
    }
    agentIds = detectedGlobalAgentIds.length === 0 ? ["universal"] : detectedGlobalAgentIds;
  } catch {
    return failAfterGuard("Unable to inspect the bundled skill.");
  }

  const childResult = await captureChild(
    spawn,
    process.execPath,
    [cli, "add", bundledSkill, "--skill", SKILL_NAME, "--global", "--yes", "--agent", ...agentIds],
    {
      shell: false,
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
      cwd,
      ...(platform !== "win32" ? { detached: true } : {}),
      env: { ...env, DO_NOT_TRACK: "1" },
    },
    options.timeoutMs ?? DEFAULT_TIMEOUT_MS,
  );

  let inspection;
  try {
    inspection = await inspectInstalledTargets(
      targets,
      requiredTargets,
      canonical,
      bundledTree,
      platform,
      hash,
      priorReceipt,
      initialStates,
    );
  } catch {
    inspection = { records: [], missingRequired: [], postconditionFailed: true };
  }

  let verifiedTargets = inspection.records;
  let ownershipFailed = false;
  if (verifiedTargets.length > 0) {
    const candidate = {
      schemaVersion: 1,
      installerVersion: version,
      skillsVersion: PINNED_SKILLS_VERSION,
      outcome: "installed",
      targets: verifiedTargets,
      affectedTargets: [],
      recovery: [],
    };
    try {
      verifiedTargets = await verifyReceiptTargets(
        { document: candidate, __bundledTree: bundledTree },
        verifiedTargets,
        classify,
      );
      ownershipFailed = verifiedTargets.length !== candidate.targets.length;
    } catch {
      verifiedTargets = [];
      ownershipFailed = true;
    }
  }

  const childFailed = !childResult.ok || hasInvalidFailureProse(childResult);
  if (inspection.postconditionFailed || inspection.missingRequired.length > 0 || ownershipFailed || childFailed) {
    if (!childResult.ok) {
      return failAfterGuard(`Skill installation failed: ${childResult.reason}.`, verifiedTargets);
    }
    return failAfterGuard("Skill installation failed.", verifiedTargets);
  }

  const document = {
    schemaVersion: 1,
    installerVersion: version,
    skillsVersion: PINNED_SKILLS_VERSION,
    outcome: "installed",
    targets: verifiedTargets,
    affectedTargets: [],
    recovery: [],
  };
  try {
    await persist(writer, receiptPath, document, options.receiptWriteOptions);
    return {
      ...result("installed", "Skill installed."),
      targetCount: verifiedTargets.length,
    };
  } catch {
    return failAfterGuard("Unable to record the installed skill.", verifiedTargets);
  }
}
