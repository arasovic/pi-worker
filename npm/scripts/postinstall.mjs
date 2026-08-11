import { installSkill } from "../lib/skill-install.mjs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import process from "node:process";

export function postinstallDiagnostic(outcome) {
  if (outcome === "installed") return "pi-worker: skill installed";
  if (outcome === "blocked") {
    return "pi-worker: skill installation blocked; run pi-worker skill status";
  }
  if (outcome === "skipped") {
    return "pi-worker: skill installation skipped; run pi-worker skill status";
  }
  return "pi-worker: skill installation failed; run pi-worker skill status";
}

export async function runPostinstall({ install = installSkill, write = console.error } = {}) {
  let outcome = "failed";
  try {
    const result = await install();
    outcome = result?.outcome ?? "failed";
  } catch {
    // Expected product failures are soft so the native CLI remains installed.
  }
  write(postinstallDiagnostic(outcome));
  return outcome;
}

const invokedPath = process.argv[1] ? path.resolve(process.argv[1]) : "";
if (invokedPath === fileURLToPath(import.meta.url)) {
  await runPostinstall();
}
