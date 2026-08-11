import { readFileSync } from "node:fs";
import { posix, win32 } from "node:path";

export const PINNED_SKILLS_VERSION = "1.5.22";
export const RULE_SCHEMA_VERSION = 1;
export const EXPECTED_AGENT_COUNT = 76;
export const EXPECTED_GLOBAL_TARGET_COUNT = 74;
export const EXPECTED_NO_GLOBAL_TARGET_COUNT = 2;

const RULE_KINDS = new Set([
  "home-relative",
  "config-home-relative",
  "environment-or-home",
  "first-existing-home-relative",
  "no-global-target",
]);

const ENVIRONMENT_VARIABLES = new Set([
  "CLAUDE_CONFIG_DIR",
  "CODEX_HOME",
  "HERMES_HOME",
  "GROK_HOME",
  "VIBE_HOME",
  "AUTOHAND_HOME",
]);

function invalid(message) {
  throw new Error(`Invalid skills target rules: ${message}`);
}

function requireString(value, name) {
  if (typeof value !== "string" || value.length === 0) invalid(`${name} must be a non-empty string`);
}

function requireRelativePath(value, name) {
  requireString(value, name);
  const segments = value.split("/");
  if (
    value.startsWith("/") ||
    value.includes("\\") ||
    value.includes("\0") ||
    value === "." ||
    segments.includes("") ||
    segments.includes(".") ||
    segments.includes("..") ||
    posix.normalize(value) !== value
  ) {
    invalid(`${name} must be a normalized relative POSIX path`);
  }
}

function validateRule(rule, index = "rule") {
  if (!rule || typeof rule !== "object" || Array.isArray(rule)) {
    invalid(`${index} must be an object`);
  }
  if (!RULE_KINDS.has(rule.kind)) invalid(`${index} has an unknown kind`);

  switch (rule.kind) {
    case "home-relative":
    case "config-home-relative":
      requireRelativePath(rule.path, `${index}.path`);
      break;
    case "environment-or-home":
      if (!ENVIRONMENT_VARIABLES.has(rule.variable)) {
        invalid(`${index}.variable is not a recognized environment variable`);
      }
      requireRelativePath(rule.fallback, `${index}.fallback`);
      requireRelativePath(rule.suffix, `${index}.suffix`);
      break;
    case "first-existing-home-relative":
      if (!Array.isArray(rule.candidates) || rule.candidates.length === 0) {
        invalid(`${index}.candidates must be a non-empty array`);
      }
      for (const candidate of rule.candidates) {
        requireRelativePath(candidate, `${index}.candidates entry`);
      }
      requireRelativePath(rule.fallback, `${index}.fallback`);
      requireRelativePath(rule.suffix, `${index}.suffix`);
      break;
    case "no-global-target":
      break;
    default:
      invalid(`${index} has an unsupported kind`);
  }
}

function validateDocument(document) {
  if (!document || typeof document !== "object" || Array.isArray(document)) {
    invalid("document must be an object");
  }
  if (document.schemaVersion !== RULE_SCHEMA_VERSION) invalid("schema version mismatch");
  if (document.skillsVersion !== PINNED_SKILLS_VERSION) invalid("skills version mismatch");
  if (!Array.isArray(document.agents)) invalid("agents must be an array");
  if (document.agentCount !== document.agents.length) invalid("agent count mismatch");

  const ids = new Set();
  let globalTargetCount = 0;
  let noGlobalTargetCount = 0;
  for (const [index, agent] of document.agents.entries()) {
    if (!agent || typeof agent !== "object" || Array.isArray(agent)) {
      invalid(`agent ${index} must be an object`);
    }
    requireString(agent.id, `agent ${index}.id`);
    if (ids.has(agent.id)) invalid(`duplicate agent id: ${agent.id}`);
    ids.add(agent.id);
    if (!agent.rule || typeof agent.rule !== "object" || Array.isArray(agent.rule)) {
      invalid(`agent ${agent.id} must have exactly one rule`);
    }
    validateRule(agent.rule, `agent ${agent.id}.rule`);
    if (agent.rule.kind === "no-global-target") noGlobalTargetCount += 1;
    else globalTargetCount += 1;
  }

  if (document.globalTargetCount !== globalTargetCount) invalid("global target count mismatch");
  if (document.noGlobalTargetCount !== noGlobalTargetCount) invalid("no-global target count mismatch");
  if (document.agentCount !== globalTargetCount + noGlobalTargetCount) invalid("total target count mismatch");
  if (document.agentCount !== EXPECTED_AGENT_COUNT) invalid("pinned agent count mismatch");
  if (document.globalTargetCount !== EXPECTED_GLOBAL_TARGET_COUNT) invalid("pinned global target count mismatch");
  if (document.noGlobalTargetCount !== EXPECTED_NO_GLOBAL_TARGET_COUNT) invalid("pinned no-global target count mismatch");

  return document;
}

export function loadRules(jsonPath) {
  let document;
  try {
    document = JSON.parse(readFileSync(jsonPath, "utf8"));
  } catch (error) {
    throw new Error(`Unable to load skills target rules: ${error.message}`, { cause: error });
  }
  return validateDocument(document);
}

function runtimeHome(runtime) {
  if (!runtime || typeof runtime.home !== "string" || runtime.home.length === 0) {
    throw new TypeError("skills target resolution requires a non-empty home path");
  }
  return runtime.home;
}

function relativePath(rule, property) {
  const value = rule[property];
  if (typeof value !== "string" || value.length === 0) {
    throw new TypeError(`skills target rule requires ${property}`);
  }
  return value;
}

export function resolveRule(rule, { env = {}, home, platform, exists } = {}) {
  const path = platform === "win32" ? win32 : posix;
  const baseHome = runtimeHome({ home });
  const environment = env && typeof env === "object" ? env : {};

  validateRule(rule);

  switch (rule.kind) {
    case "home-relative":
      return path.join(baseHome, relativePath(rule, "path"));
    case "config-home-relative": {
      const configHome = environment.XDG_CONFIG_HOME || path.join(baseHome, ".config");
      return path.join(configHome, relativePath(rule, "path"));
    }
    case "environment-or-home": {
      const variable = rule.variable;
      if (!ENVIRONMENT_VARIABLES.has(variable)) {
        throw new TypeError(`unrecognized environment variable in skills target rule: ${variable}`);
      }
      const configured = typeof environment[variable] === "string"
        ? environment[variable].trim()
        : "";
      const base = configured || path.join(baseHome, relativePath(rule, "fallback"));
      return path.join(base, relativePath(rule, "suffix"));
    }
    case "first-existing-home-relative": {
      if (typeof exists !== "function") {
        throw new TypeError("first-existing-home-relative requires an exists function");
      }
      for (const candidate of rule.candidates) {
        const candidatePath = path.join(baseHome, candidate);
        if (exists(candidatePath)) return path.join(candidatePath, relativePath(rule, "suffix"));
      }
      return path.join(baseHome, relativePath(rule, "fallback"), relativePath(rule, "suffix"));
    }
    case "no-global-target":
      return null;
    default:
      throw new TypeError(`unknown skills target rule kind: ${rule.kind}`);
  }
}

export function resolveAllTargets(document, runtime) {
  validateDocument(document);

  const targets = [];
  const resolved = new Set();
  for (const agent of document.agents) {
    if (!agent || typeof agent !== "object" || !agent.rule) {
      throw new TypeError("skills target document contains an invalid agent");
    }
    const target = resolveRule(agent.rule, runtime);
    const key = runtime?.platform === "win32" && target !== null ? target.toLowerCase() : target;
    if (target === null || resolved.has(key)) continue;
    resolved.add(key);
    targets.push(target);
  }
  return targets;
}
