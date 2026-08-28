import {
  closeSync,
  fstatSync,
  lstatSync,
  openSync,
  readSync,
  readFileSync,
} from "node:fs";
import { posix, win32 } from "node:path";

export const PINNED_SKILLS_VERSION = "1.5.23";
export const RULE_SCHEMA_VERSION = 3;
const MAX_EVE_PACKAGE_BYTES = 1024 * 1024;
export const EXPECTED_AGENT_COUNT = 77;
export const EXPECTED_GLOBAL_TARGET_COUNT = 75;
export const EXPECTED_NO_GLOBAL_TARGET_COUNT = 2;

const RULE_KINDS = new Set([
  "home-relative",
  "config-home-relative",
  "environment-or-home",
  "first-existing-home-relative",
  "no-global-target",
]);
const DETECTOR_KINDS = new Set(["any-existing", "eve-project", "never"]);
const DETECTOR_PATH_KINDS = new Set([
  "home-relative",
  "config-home-relative",
  "cwd-relative",
  "absolute",
  "environment-relative",
  "environment-or-home",
]);

const ENVIRONMENT_VARIABLES = new Set([
  "APPDATA",
  "CLAUDE_CONFIG_DIR",
  "CODEX_HOME",
  "FLATPAK_XDG_CONFIG_HOME",
  "HERMES_HOME",
  "GROK_HOME",
  "VIBE_HOME",
  "AUTOHAND_HOME",
]);
const ENVIRONMENT_HOME_VARIABLES = new Set([
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

function requireOnly(value, allowed, name) {
  for (const key of Object.keys(value)) if (!allowed.includes(key)) invalid(`${name} has an unknown field: ${key}`);
}

function requireRelativePath(value, name, { allowEmpty = false } = {}) {
  if (allowEmpty && value === "") return;
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

function validatePathExpression(expression, index = "path") {
  if (!expression || typeof expression !== "object" || Array.isArray(expression)) {
    invalid(`${index} must be an object`);
  }
  if (!DETECTOR_PATH_KINDS.has(expression.kind)) invalid(`${index} has an unknown kind`);
  const fields = {
    "home-relative": ["kind", "path"],
    "config-home-relative": ["kind", "path"],
    "cwd-relative": ["kind", "path"],
    absolute: ["kind", "path"],
    "environment-relative": ["kind", "variable", "suffix"],
    "environment-or-home": ["kind", "variable", "fallback"],
  }[expression.kind];
  requireOnly(expression, fields, index);
  switch (expression.kind) {
    case "home-relative":
    case "config-home-relative":
    case "cwd-relative":
      requireRelativePath(expression.path, `${index}.path`);
      break;
    case "absolute":
      requireString(expression.path, `${index}.path`);
      if (!expression.path.startsWith("/") || expression.path.includes("\\") || expression.path.includes("\0")) {
        invalid(`${index}.path must be an absolute POSIX path`);
      }
      break;
    case "environment-relative":
      if (!ENVIRONMENT_VARIABLES.has(expression.variable)) {
        invalid(`${index}.variable is not a recognized environment variable`);
      }
      requireRelativePath(expression.suffix, `${index}.suffix`);
      break;
    case "environment-or-home":
      if (!ENVIRONMENT_HOME_VARIABLES.has(expression.variable)) {
        invalid(`${index}.variable is not a recognized environment-home variable`);
      }
      requireRelativePath(expression.fallback, `${index}.fallback`);
      break;
    default:
      invalid(`${index} has an unsupported kind`);
  }
}

function validateDetector(detector, index = "detector") {
  if (!detector || typeof detector !== "object" || Array.isArray(detector)) {
    invalid(`${index} must be an object`);
  }
  if (!DETECTOR_KINDS.has(detector.kind)) invalid(`${index} has an unknown kind`);
  const fields = {
    "any-existing": ["kind", "paths"],
    "eve-project": ["kind", "agentPath", "packageJsonPath", "dependency"],
    never: ["kind"],
  }[detector.kind];
  requireOnly(detector, fields, index);
  switch (detector.kind) {
    case "any-existing":
      if (!Array.isArray(detector.paths) || detector.paths.length === 0) {
        invalid(`${index}.paths must be a non-empty array`);
      }
      detector.paths.forEach((pathExpression, pathIndex) => {
        validatePathExpression(pathExpression, `${index}.paths[${pathIndex}]`);
      });
      break;
    case "eve-project":
      validatePathExpression(detector.agentPath, `${index}.agentPath`);
      validatePathExpression(detector.packageJsonPath, `${index}.packageJsonPath`);
      if (detector.agentPath.kind !== "cwd-relative" || detector.agentPath.path !== "agent" ||
        detector.packageJsonPath.kind !== "cwd-relative" || detector.packageJsonPath.path !== "package.json" ||
        detector.dependency !== "eve") {
        invalid(`${index} is not the pinned Eve detector`);
      }
      break;
    case "never":
      break;
    default:
      invalid(`${index} has an unsupported kind`);
  }
}

function validateRule(rule, index = "rule") {
  if (!rule || typeof rule !== "object" || Array.isArray(rule)) {
    invalid(`${index} must be an object`);
  }
  if (!RULE_KINDS.has(rule.kind)) invalid(`${index} has an unknown kind`);
  const fields = {
    "home-relative": ["kind", "path"],
    "config-home-relative": ["kind", "path"],
    "environment-or-home": ["kind", "variable", "fallback", "suffix"],
    "first-existing-home-relative": ["kind", "candidates", "fallback", "suffix"],
    "no-global-target": ["kind"],
  }[rule.kind];
  requireOnly(rule, fields, index);

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

function validateDocument(document, enforcePinned = true) {
  if (!document || typeof document !== "object" || Array.isArray(document)) {
    invalid("document must be an object");
  }
  requireOnly(document, [
    "schemaVersion",
    "skillsVersion",
    "agentCount",
    "globalTargetCount",
    "noGlobalTargetCount",
    "agents",
  ], "document");
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
    requireOnly(agent, ["id", "usesUniversalTarget", "rule", "detector"], `agent ${index}`);
    requireString(agent.id, `agent ${index}.id`);
    if (typeof agent.usesUniversalTarget !== "boolean") {
      invalid(`agent ${agent.id} must declare universal target behavior`);
    }
    if (ids.has(agent.id)) invalid(`duplicate agent id: ${agent.id}`);
    ids.add(agent.id);
    if (!agent.rule || typeof agent.rule !== "object" || Array.isArray(agent.rule)) {
      invalid(`agent ${agent.id} must have exactly one rule`);
    }
    validateRule(agent.rule, `agent ${agent.id}.rule`);
    if (!Object.hasOwn(agent, "detector")) invalid(`agent ${agent.id} must have exactly one detector`);
    validateDetector(agent.detector, `agent ${agent.id}.detector`);
    if (agent.rule.kind === "no-global-target") noGlobalTargetCount += 1;
    else globalTargetCount += 1;
  }

  if (document.globalTargetCount !== globalTargetCount) invalid("global target count mismatch");
  if (document.noGlobalTargetCount !== noGlobalTargetCount) invalid("no-global target count mismatch");
  if (document.agentCount !== globalTargetCount + noGlobalTargetCount) invalid("total target count mismatch");
  if (enforcePinned) {
    if (document.agentCount !== EXPECTED_AGENT_COUNT) invalid("pinned agent count mismatch");
    if (document.globalTargetCount !== EXPECTED_GLOBAL_TARGET_COUNT) invalid("pinned global target count mismatch");
    if (document.noGlobalTargetCount !== EXPECTED_NO_GLOBAL_TARGET_COUNT) invalid("pinned no-global target count mismatch");
  }

  return document;
}

function runtimePath(runtime) {
  return runtime?.platform === "win32" ? win32 : posix;
}

function runtimeCwd(runtime) {
  if (runtime?.cwd === undefined) return process.cwd();
  if (typeof runtime.cwd !== "string" || runtime.cwd.length === 0) {
    throw new TypeError("skills detector evaluation requires a non-empty cwd path");
  }
  return runtime.cwd;
}

function environmentValue(environment, variable) {
  return typeof environment[variable] === "string" ? environment[variable].trim() : "";
}

function resolveDetectorPath(expression, runtime) {
  validatePathExpression(expression);
  const path = runtimePath(runtime);
  const home = runtimeHome(runtime);
  const cwd = runtimeCwd(runtime);
  const environment = runtime?.env && typeof runtime.env === "object" ? runtime.env : {};
  const fromCwd = (candidate) => path.isAbsolute(candidate) ? candidate : path.resolve(cwd, candidate);
  switch (expression.kind) {
    case "home-relative": return fromCwd(path.join(home, expression.path));
    case "config-home-relative": {
      const configHome = environment.XDG_CONFIG_HOME || path.join(home, ".config");
      return fromCwd(path.join(configHome, expression.path));
    }
    case "cwd-relative": return fromCwd(path.join(cwd, expression.path));
    case "absolute": return expression.path;
    case "environment-relative": {
      const configured = environmentValue(environment, expression.variable);
      return configured ? fromCwd(path.join(configured, expression.suffix)) : null;
    }
    case "environment-or-home": {
      const configured = environmentValue(environment, expression.variable);
      return fromCwd(configured || path.join(home, expression.fallback));
    }
    default: throw new TypeError(`unknown detector path kind: ${expression.kind}`);
  }
}

function readBoundedJsonDependency(packagePath, dependency) {
  let descriptor;
  try {
    const stat = lstatSync(packagePath);
    if (!stat.isFile() || stat.size > MAX_EVE_PACKAGE_BYTES) return false;
    descriptor = openSync(packagePath, "r");
    const opened = fstatSync(descriptor);
    if (!opened.isFile() || opened.size > MAX_EVE_PACKAGE_BYTES) return false;
    const bytes = Buffer.alloc(opened.size);
    let offset = 0;
    while (offset < opened.size) {
      const count = readSync(descriptor, bytes, offset, opened.size - offset, offset);
      if (count <= 0) return false;
      offset += count;
    }
    const value = JSON.parse(bytes.toString("utf8"));
    if (!value || typeof value !== "object" || Array.isArray(value)) return false;
    const hasDependency = (section) => section && typeof section === "object" && !Array.isArray(section) &&
      Object.prototype.hasOwnProperty.call(section, dependency) && Boolean(section[dependency]);
    return hasDependency(value.dependencies) || hasDependency(value.devDependencies);
  } catch {
    return false;
  } finally {
    if (descriptor !== undefined) {
      try { closeSync(descriptor); } catch { /* best effort */ }
    }
  }
}

export function evaluateDetector(detector, runtime = {}) {
  validateDetector(detector);
  const exists = runtime.exists;
  if (typeof exists !== "function") throw new TypeError("skills detector evaluation requires an exists function");
  switch (detector.kind) {
    case "never": return false;
    case "any-existing":
      return detector.paths.some((expression) => {
        const resolved = resolveDetectorPath(expression, runtime);
        return resolved !== null && Boolean(exists(resolved));
      });
    case "eve-project": {
      const agentPath = resolveDetectorPath(detector.agentPath, runtime);
      if (!exists(agentPath)) return false;
      const packagePath = resolveDetectorPath(detector.packageJsonPath, runtime);
      return readBoundedJsonDependency(packagePath, detector.dependency);
    }
    default: throw new TypeError(`unknown skills detector kind: ${detector.kind}`);
  }
}

export function detectInstalledAgents(document, runtime) {
  validateDocument(document, false);
  return document.agents.filter((agent) => evaluateDetector(agent.detector, runtime)).map((agent) => agent.id);
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

export function resolveAgentTarget(agent, runtime) {
  if (!agent || typeof agent !== "object" || Array.isArray(agent)) {
    throw new TypeError("skills target resolution requires an agent");
  }
  if (typeof agent.usesUniversalTarget !== "boolean") {
    throw new TypeError("skills target resolution requires universal target behavior");
  }
  if (agent.usesUniversalTarget) {
    const path = runtimePath(runtime);
    return path.join(runtimeHome(runtime), ".agents", "skills");
  }
  return resolveRule(agent.rule, runtime);
}

export function resolveAllTargets(document, runtime) {
  validateDocument(document);

  const targets = [];
  const resolved = new Set();
  for (const agent of document.agents) {
    if (!agent || typeof agent !== "object" || !agent.rule) {
      throw new TypeError("skills target document contains an invalid agent");
    }
    const target = resolveAgentTarget(agent, runtime);
    const key = runtime?.platform === "win32" && target !== null ? target.toLowerCase() : target;
    if (target === null || resolved.has(key)) continue;
    resolved.add(key);
    targets.push(target);
  }
  return targets;
}
