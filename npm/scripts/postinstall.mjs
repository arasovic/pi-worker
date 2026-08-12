import { installSkill } from "../lib/skill-install.mjs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import path from "node:path";
import process from "node:process";

const require = createRequire(import.meta.url);
const packageVersion = require("../../package.json").version;

function plainDiagnostic(outcome) {
  if (outcome === "installed") return "pi-worker: skill installed";
  if (outcome === "blocked") {
    return "pi-worker: skill installation blocked; run pi-worker skill status";
  }
  if (outcome === "skipped") {
    return "pi-worker: skill installation skipped; run pi-worker skill status";
  }
  return "pi-worker: skill installation failed; run pi-worker skill status";
}

export function postinstallDiagnostic({
  outcome,
  interactive,
  targetCount,
  version,
}) {
  if (!interactive) return plainDiagnostic(outcome);

  const normalized = ["installed", "blocked", "skipped"].includes(outcome)
    ? outcome
    : "failed";
  const color = normalized === "installed" ? 32 : normalized === "failed" ? 31 : 33;
  const count = normalized === "installed" && Number.isSafeInteger(targetCount) && targetCount >= 0
    ? ` · ${targetCount} ${targetCount === 1 ? "target" : "targets"}`
    : "";
  const next = normalized === "installed" ? "pi-worker doctor" : "pi-worker skill status";
  return `Pi Worker ${version}\n` +
    `  skill   \u001b[${color}m${normalized}\u001b[0m${count}\n` +
    `  next    ${next}`;
}

export async function runPostinstall({
  env = process.env,
  install = installSkill,
  isTTY = Boolean(process.stderr.isTTY),
  version = packageVersion,
  write = console.error,
} = {}) {
  let outcome = "failed";
  let targetCount;
  try {
    const result = await install();
    outcome = result?.outcome ?? "failed";
    targetCount = result?.targetCount;
  } catch {
    // Expected product failures are soft so the native CLI remains installed.
  }
  const interactive = isTTY && !Object.hasOwn(env, "NO_COLOR") && !env.CI;
  write(postinstallDiagnostic({ outcome, interactive, targetCount, version }));
  return outcome;
}

const invokedPath = process.argv[1] ? path.resolve(process.argv[1]) : "";
if (invokedPath === fileURLToPath(import.meta.url)) {
  await runPostinstall();
}
