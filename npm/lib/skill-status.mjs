import process from "node:process";

import { inspectExternalTargets } from "./external-inspection.mjs";
import { runNativeCaptured } from "./native.mjs";

const CAPTURE_LIMIT = 1024 * 1024;
const NORMAL_STATUS_CODES = new Set([0, 3]);
const HUMAN_VALUE_LIMIT = 1024;
const STATUS_VALUES = new Set(["verified", "missing", "blocked", "drifted", "skipped", "failed", "stale"]);
const OPTIONAL_VERSION_FIELDS = new Set(["installerVersion", "programVersion"]);
const AFFECTED_STATE_VALUES = new Set(["unmanaged", "drifted", "conflicting"]);
const EXTERNAL_STATE_VALUES = new Set(["performed", "unavailable"]);
const EXTERNAL_IDENTITY_VALUES = new Set(["current", "legacy", "unknown", "none"]);
const SAFE_RECOVERY_COMMAND = "npm install -g --foreground-scripts pi-worker";

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasExactKeys(value, keys) {
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  return actual.length === expected.length && actual.every((key, index) => key === expected[index]);
}

function isStringArray(value) {
  return Array.isArray(value) && value.every((item) => typeof item === "string");
}

function hasDocumentKeys(value) {
  const required = [
    "schemaVersion", "receiptPath", "status", "verifiedTargets", "trackedTargets",
    "affectedTargets", "recovery", "externalInspection",
  ];
  const keys = Object.keys(value);
  return required.every((key) => keys.includes(key)) &&
    keys.every((key) => required.includes(key) || OPTIONAL_VERSION_FIELDS.has(key));
}

function isAffectedTarget(value) {
  return isObject(value) && hasExactKeys(value, ["path", "state", "recovery"]) &&
    typeof value.path === "string" && AFFECTED_STATE_VALUES.has(value.state) &&
    isStringArray(value.recovery);
}

function isExternalTarget(value) {
  return isObject(value) && hasExactKeys(value, ["path", "identity"]) &&
    typeof value.path === "string" && EXTERNAL_IDENTITY_VALUES.has(value.identity);
}

function isExternalInspection(value) {
  return isObject(value) && hasExactKeys(value, ["state", "targets"]) &&
    EXTERNAL_STATE_VALUES.has(value.state) && Array.isArray(value.targets) &&
    value.targets.every(isExternalTarget) &&
    (value.state !== "unavailable" || value.targets.length === 0);
}

function parseNativeStatus(text) {
  const value = JSON.parse(text);
  if (!isObject(value) || !hasDocumentKeys(value) || value.schemaVersion !== 1 ||
    typeof value.receiptPath !== "string" || !STATUS_VALUES.has(value.status) ||
    !isStringArray(value.verifiedTargets) || !isStringArray(value.trackedTargets) ||
    !Array.isArray(value.affectedTargets) || !value.affectedTargets.every(isAffectedTarget) ||
    !isStringArray(value.recovery) || !isExternalInspection(value.externalInspection) ||
    (value.installerVersion !== undefined && typeof value.installerVersion !== "string") ||
    (value.programVersion !== undefined && typeof value.programVersion !== "string")) {
    throw new Error("native skill status document is invalid");
  }
  return value;
}

function normalizedInspection(value) {
  if (!isObject(value) || !["performed", "unavailable"].includes(value.state) || !Array.isArray(value.targets)) {
    return { state: "unavailable", targets: [] };
  }
  if (value.state === "unavailable") return { state: "unavailable", targets: [] };
  const targets = [];
  for (const target of value.targets) {
    if (!isObject(target) || typeof target.path !== "string" ||
      !["current", "legacy", "unknown", "none"].includes(target.identity)) {
      return { state: "unavailable", targets: [] };
    }
    targets.push({ path: target.path, identity: target.identity });
  }
  return { state: "performed", targets };
}

function humanValue(value) {
  const flat = value.replace(/[\u0000-\u001f\u007f]/gu, " ");
  return flat.length <= HUMAN_VALUE_LIMIT ? flat : `${flat.slice(0, HUMAN_VALUE_LIMIT - 1)}…`;
}

function renderHuman(document) {
  const lines = [
    `status: ${humanValue(document.status)}`,
  ];
  if (document.status === "stale") {
    lines.push(`stale: installed by ${humanValue(document.installerVersion ?? "")}, ` +
      `running ${humanValue(document.programVersion ?? "")}; recovery: ${SAFE_RECOVERY_COMMAND}`);
  }
  lines.push(`receipt-path: ${humanValue(document.receiptPath)}`);
  if (document.verifiedTargets.length > 0) {
    lines.push("verified-targets:", ...document.verifiedTargets.map((target) => `- ${humanValue(target)}`));
  }
  if (document.affectedTargets.length > 0) {
    lines.push("affected-targets:");
    for (const target of document.affectedTargets) {
      lines.push(`- ${humanValue(target.path)} (${humanValue(target.state)})`);
      for (const recovery of target.recovery) lines.push(`  - ${humanValue(recovery)}`);
    }
  }
  if (document.recovery.length > 0) {
    lines.push("recovery:", ...document.recovery.map((recovery) => `- ${humanValue(recovery)}`));
  }
  lines.push(`external-inspection: ${humanValue(document.externalInspection.state)}`);
  if (document.externalInspection.targets.length > 0) {
    lines.push("external-targets:");
    for (const target of document.externalInspection.targets) {
      const detail = {
        current: "current; externally managed; may be stale",
        legacy: "legacy; externally managed; older contract",
        unknown: "unknown; possibly newer version; inspect manually",
        none: "no recognized identity; inspect manually",
      }[target.identity];
      lines.push(`- ${humanValue(target.path)} (${detail})`);
    }
  }
  return `${lines.join("\n")}\n`;
}

export async function runSkillStatus(options = {}) {
  const runCaptured = options.runCaptured ?? runNativeCaptured;
  const inspect = options.inspect ?? inspectExternalTargets;
  const writeStdout = options.writeStdout ?? ((value) => process.stdout.write(value));
  const writeStderr = options.writeStderr ?? ((value) => process.stderr.write(value));
  const result = await runCaptured(
    options.binary,
    ["skill", "status", "--json"],
    { maxOutputBytes: CAPTURE_LIMIT },
  );
  if (result.stderr) writeStderr(result.stderr);
  if (!NORMAL_STATUS_CODES.has(result.code)) {
    if (result.stdout) writeStdout(result.stdout);
    return result.code;
  }

  let document;
  try {
    document = parseNativeStatus(result.stdout);
  } catch {
    writeStderr("pi-worker: invalid native skill status document\n");
    return 9;
  }

  let external;
  try {
    external = normalizedInspection(await inspect({
      excludePaths: document.trackedTargets,
    }));
  } catch {
    external = { state: "unavailable", targets: [] };
  }
  if (external.state === "unavailable") {
    writeStderr("pi-worker: external skill inspection unavailable\n");
  }
  document.externalInspection = external;
  writeStdout(options.json ? `${JSON.stringify(document)}\n` : renderHuman(document));
  return result.code;
}
