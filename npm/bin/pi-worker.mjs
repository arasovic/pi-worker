#!/usr/bin/env node
import { fileURLToPath } from "node:url";
import process from "node:process";

import {
  UnsupportedPlatformError,
  nativePath,
  nativeTarget,
  runNative,
} from "../lib/native.mjs";

function unsupportedPlatformDiagnostic(error) {
  const message = typeof error?.message === "string" ? error.message : "";
  return message
    .replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(/[^\x20-\x7e]+/g, " ")
    .replace(/\s+/g, " ")
    .trim()
    .slice(0, 200) || "Unsupported platform/architecture";
}

try {
  const packageRoot = fileURLToPath(new URL("../..", import.meta.url));
  const target = nativeTarget();
  const binary = nativePath(packageRoot, target.platform, target.arch);
  const args = process.argv.slice(2);

  if (
    args.length >= 2 && args.length <= 3 &&
    args[0] === "skill" && args[1] === "status" &&
    (args.length === 2 || args[2] === "--json")
  ) {
    const { runSkillStatus } = await import("../lib/skill-status.mjs");
    process.exitCode = await runSkillStatus({ binary, json: args[2] === "--json" });
  } else {
    process.exitCode = await runNative(binary, args);
  }
} catch (error) {
  if (!(error instanceof UnsupportedPlatformError)) throw error;
  process.stderr.write(`${unsupportedPlatformDiagnostic(error)}\n`);
  process.exitCode = 1;
}
