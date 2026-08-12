import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";
import path from "node:path";
import process from "node:process";

import { loadRules, resolveAllTargets } from "./skill-rules.mjs";
import { inspectSkillIdentity } from "./skill-tree.mjs";

const SKILL_NAME = "pi-worker";
const packageRoot = fileURLToPath(new URL("../..", import.meta.url));

function pathKey(value, platform) {
  const normalized = path.normalize(value);
  return platform === "win32" ? normalized.toLowerCase() : normalized;
}

function targetPaths(roots, home, cwd, platform) {
  const seen = new Set();
  const targets = [];
  for (const root of [...roots, path.join(home, ".agents", "skills")]) {
    if (typeof root !== "string") continue;
    const target = path.resolve(cwd, root, SKILL_NAME);
    const key = pathKey(target, platform);
    if (seen.has(key)) continue;
    seen.add(key);
    targets.push(target);
  }
  return targets.sort((left, right) => left < right ? -1 : left > right ? 1 : 0);
}

export async function inspectExternalTargets(options = {}) {
  const platform = options.platform ?? process.platform;
  const env = options.env ?? process.env;
  const home = options.home ?? env.HOME ?? env.USERPROFILE ?? homedir();
  const cwd = options.cwd ?? process.cwd();
  const inspect = options.inspectIdentity ?? inspectSkillIdentity;
  const resolveTargets = options.resolveTargets ?? resolveAllTargets;
  const excluded = new Set((options.excludePaths ?? []).map((value) => pathKey(value, platform)));

  try {
    const rules = options.rules ?? loadRules(
      options.rulesPath ?? path.join(packageRoot, "npm", "generated", "skills-rules.json"),
    );
    const roots = resolveTargets(rules, {
      env,
      home,
      cwd,
      platform,
      exists: (candidate) => existsSync(candidate),
    }) ?? [];
    const targets = [];
    for (const target of targetPaths(roots, home, cwd, platform)) {
      if (excluded.has(pathKey(target, platform))) continue;
      const identity = await inspect(target);
      if (identity === "absent") continue;
      if (!["current", "legacy", "unknown", "none"].includes(identity)) {
        throw new Error("external skill identity is invalid");
      }
      targets.push({ path: target, identity });
    }
    return { state: "performed", targets };
  } catch {
    return { state: "unavailable", targets: [] };
  }
}
