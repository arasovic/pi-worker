import process from "node:process";

import { inspectExternalTargets } from "./external-inspection.mjs";
import { runNativeCaptured } from "./native.mjs";

const CAPTURE_LIMIT = 1024 * 1024;
const NORMAL_STATUS_CODES = new Set([0, 3]);

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function parseNativeStatus(text) {
  const value = JSON.parse(text);
  if (!isObject(value) || value.schemaVersion !== 1 || !Array.isArray(value.verifiedTargets) ||
    !Array.isArray(value.trackedTargets)) {
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

function renderHuman(document) {
  const lines = [
    `status: ${document.status}`,
    `receipt-path: ${document.receiptPath}`,
  ];
  if (document.verifiedTargets.length > 0) {
    lines.push("verified-targets:", ...document.verifiedTargets.map((target) => `- ${target}`));
  }
  if (document.affectedTargets.length > 0) {
    lines.push("affected-targets:");
    for (const target of document.affectedTargets) {
      lines.push(`- ${target.path} (${target.state})`);
      for (const recovery of target.recovery) lines.push(`  - ${recovery}`);
    }
  }
  if (document.recovery.length > 0) {
    lines.push("recovery:", ...document.recovery.map((recovery) => `- ${recovery}`));
  }
  lines.push(`external-inspection: ${document.externalInspection.state}`);
  if (document.externalInspection.targets.length > 0) {
    lines.push("external-targets:");
    for (const target of document.externalInspection.targets) {
      const detail = {
        current: "current; externally managed; may be stale",
        legacy: "legacy; externally managed; older contract",
        unknown: "unknown; possibly newer version; inspect manually",
        none: "no recognized identity; inspect manually",
      }[target.identity];
      lines.push(`- ${target.path} (${detail})`);
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
