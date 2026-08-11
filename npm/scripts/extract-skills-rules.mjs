#!/usr/bin/env node

import { createRequire } from "node:module";
import {
  closeSync,
  fstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  readSync,
  writeFileSync,
} from "node:fs";
import { dirname, posix, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  EXPECTED_AGENT_COUNT,
  EXPECTED_GLOBAL_TARGET_COUNT,
  EXPECTED_NO_GLOBAL_TARGET_COUNT,
  PINNED_SKILLS_VERSION,
  RULE_SCHEMA_VERSION,
} from "../lib/skill-rules.mjs";

const require = createRequire(import.meta.url);
const MAX_SOURCE_BYTES = 1024 * 1024;

const ENVIRONMENT_SPECS = Object.freeze({
  codexHome: { variable: "CODEX_HOME", fallback: ".codex" },
  claudeHome: { variable: "CLAUDE_CONFIG_DIR", fallback: ".claude" },
  vibeHome: { variable: "VIBE_HOME", fallback: ".vibe" },
  hermesHome: { variable: "HERMES_HOME", fallback: ".hermes" },
  autohandHome: { variable: "AUTOHAND_HOME", fallback: ".autohand" },
  grokHome: { variable: "GROK_HOME", fallback: ".grok" },
});

const OPENCLAW_RULE = Object.freeze({
  kind: "first-existing-home-relative",
  candidates: [".openclaw", ".clawdbot", ".moltbot"],
  fallback: ".openclaw",
  suffix: "skills",
});

function extractionError(message) {
  throw new Error(`Unable to extract pinned skills target rules: ${message}`);
}

function occurrences(source, needle) {
  const indexes = [];
  let from = 0;
  while (from <= source.length - needle.length) {
    const index = source.indexOf(needle, from);
    if (index < 0) break;
    indexes.push(index);
    from = index + needle.length;
  }
  return indexes;
}

function uniqueAnchor(source, anchor, name) {
  const indexes = occurrences(source, anchor);
  if (indexes.length === 0) extractionError(`missing ${name} anchor`);
  if (indexes.length !== 1) extractionError(`duplicate ${name} anchor`);
  return indexes[0];
}

function skipQuoted(source, start, limit = source.length) {
  const quote = source[start];
  for (let index = start + 1; index < limit; index += 1) {
    if (source[index] === "\\") {
      index += 1;
      continue;
    }
    if (source[index] === quote) return index + 1;
    if (source[index] === "\n" || source[index] === "\r") {
      extractionError("unterminated quoted source section");
    }
  }
  extractionError("unterminated quoted source section");
}

function skipTemplate(source, start, limit = source.length) {
  for (let index = start + 1; index < limit; index += 1) {
    if (source[index] === "\\") {
      index += 1;
      continue;
    }
    if (source[index] === "`") return index + 1;
  }
  extractionError("unterminated template source section");
}

function skipComment(source, start, limit = source.length) {
  if (source[start + 1] === "/") {
    const newline = source.indexOf("\n", start + 2);
    return newline < 0 || newline >= limit ? limit : newline + 1;
  }
  const close = source.indexOf("*/", start + 2);
  if (close < 0 || close + 2 > limit) extractionError("unterminated comment source section");
  return close + 2;
}

function scanBalanced(source, openIndex, limit = source.length) {
  const pairs = { "{": "}", "(": ")", "[": "]" };
  const stack = [source[openIndex]];
  if (!pairs[source[openIndex]]) extractionError("invalid delimiter anchor");

  for (let index = openIndex + 1; index < limit; index += 1) {
    const character = source[index];
    if (character === "\"" || character === "'") {
      index = skipQuoted(source, index, limit) - 1;
      continue;
    }
    if (character === "`") {
      index = skipTemplate(source, index, limit) - 1;
      continue;
    }
    if (character === "/" && (source[index + 1] === "/" || source[index + 1] === "*")) {
      index = skipComment(source, index, limit) - 1;
      continue;
    }
    if (pairs[character]) {
      stack.push(character);
      continue;
    }
    if (character === "}" || character === ")" || character === "]") {
      const open = stack.pop();
      if (!open || pairs[open] !== character) extractionError("mismatched delimiters in recognized section");
      if (stack.length === 0) return index;
    }
  }

  extractionError("unterminated recognized section");
}

function topLevelSegments(source, start, end) {
  const segments = [];
  const stack = [];
  let segmentStart = start;

  for (let index = start; index < end; index += 1) {
    const character = source[index];
    if (character === "\"" || character === "'") {
      index = skipQuoted(source, index, end) - 1;
      continue;
    }
    if (character === "`") {
      index = skipTemplate(source, index, end) - 1;
      continue;
    }
    if (character === "/" && (source[index + 1] === "/" || source[index + 1] === "*")) {
      index = skipComment(source, index, end) - 1;
      continue;
    }
    if (character === "{" || character === "(" || character === "[") {
      stack.push(character);
      continue;
    }
    if (character === "}" || character === ")" || character === "]") {
      const open = stack.pop();
      const pairs = { "{": "}", "(": ")", "[": "]" };
      if (!open || pairs[open] !== character) extractionError("mismatched delimiters in recognized section");
      continue;
    }
    if (character === "," && stack.length === 0) {
      if (source.slice(segmentStart, index).trim()) segments.push([segmentStart, index]);
      segmentStart = index + 1;
    }
  }

  if (stack.length !== 0) extractionError("unterminated recognized section");
  if (source.slice(segmentStart, end).trim()) segments.push([segmentStart, end]);
  return segments;
}

function topLevelColon(source, start, end) {
  const stack = [];
  const pairs = { "{": "}", "(": ")", "[": "]" };
  for (let index = start; index < end; index += 1) {
    const character = source[index];
    if (character === "\"" || character === "'") {
      index = skipQuoted(source, index, end) - 1;
      continue;
    }
    if (character === "`") {
      index = skipTemplate(source, index, end) - 1;
      continue;
    }
    if (character === "/" && (source[index + 1] === "/" || source[index + 1] === "*")) {
      index = skipComment(source, index, end) - 1;
      continue;
    }
    if (pairs[character]) {
      stack.push(character);
      continue;
    }
    if (character === "}" || character === ")" || character === "]") {
      const open = stack.pop();
      if (!open || pairs[open] !== character) extractionError("mismatched delimiters in recognized section");
      continue;
    }
    if (character === ":" && stack.length === 0) return index;
  }
  extractionError("missing property delimiter in recognized section");
}

function trimRange(source, start, end) {
  while (start < end && /\s/.test(source[start])) start += 1;
  while (end > start && /\s/.test(source[end - 1])) end -= 1;
  return [start, end];
}

function parseStringLiteral(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  if (trimmedStart >= trimmedEnd || source[trimmedStart] !== '"') {
    extractionError("unrecognized string literal source form");
  }
  const literalEnd = skipQuoted(source, trimmedStart, trimmedEnd);
  if (literalEnd !== trimmedEnd) extractionError("ambiguous string literal source form");
  let value;
  try {
    value = JSON.parse(source.slice(trimmedStart, literalEnd));
  } catch {
    extractionError("invalid string literal source form");
  }
  if (typeof value !== "string") extractionError("non-string source literal");
  return value;
}

function parseIdentifier(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  const value = source.slice(trimmedStart, trimmedEnd);
  if (!/^[A-Za-z_$][A-Za-z0-9_$]*$/.test(value)) {
    extractionError(`unrecognized identifier source form: ${value}`);
  }
  return value;
}

function parsePropertyKey(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  if (source[trimmedStart] === '"') return parseStringLiteral(source, trimmedStart, trimmedEnd);
  return parseIdentifier(source, trimmedStart, trimmedEnd);
}

function pathFromSegments(segments) {
  if (segments.length === 0 || segments.some((segment) => segment.length === 0)) {
    extractionError("empty target path source form");
  }
  const path = posix.join(...segments);
  if (!path || path === "." || path.startsWith("/") || path.split("/").includes("..")) {
    extractionError("unsafe target path source form");
  }
  return path;
}

function parseCallExpression(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  const text = source.slice(trimmedStart, trimmedEnd);
  if (!text.startsWith("join")) extractionError("unrecognized global target expression");
  const openIndex = trimmedStart + 4;
  if (source[openIndex] !== "(") extractionError("unrecognized join expression");
  const closeIndex = scanBalanced(source, openIndex, trimmedEnd);
  if (closeIndex !== trimmedEnd - 1) extractionError("ambiguous join expression");
  const argumentsList = topLevelSegments(source, openIndex + 1, closeIndex);
  if (argumentsList.length < 2) extractionError("empty join expression");
  const base = parseIdentifier(source, argumentsList[0][0], argumentsList[0][1]);
  const literals = argumentsList.slice(1).map(([argumentStart, argumentEnd]) => (
    parseStringLiteral(source, argumentStart, argumentEnd)
  ));

  if (base === "home") {
    return { kind: "home-relative", path: pathFromSegments(literals) };
  }
  if (base === "configHome") {
    return { kind: "config-home-relative", path: pathFromSegments(literals) };
  }
  if (Object.hasOwn(ENVIRONMENT_SPECS, base)) {
    if (literals.length !== 1 || literals[0] !== "skills") {
      extractionError(`unrecognized ${base} target expression`);
    }
    const specification = ENVIRONMENT_SPECS[base];
    return {
      kind: "environment-or-home",
      variable: specification.variable,
      fallback: specification.fallback,
      suffix: "skills",
    };
  }
  extractionError(`unrecognized join base: ${base}`);
}

function parseRuleExpression(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  const expression = source.slice(trimmedStart, trimmedEnd);
  if (expression === "void 0") return { kind: "no-global-target" };
  if (expression === "getOpenClawGlobalSkillsDir()") return { ...OPENCLAW_RULE };
  return parseCallExpression(source, trimmedStart, trimmedEnd);
}

function parseAgent(source, start, end) {
  const [memberStart, memberEnd] = trimRange(source, start, end);
  const colon = topLevelColon(source, memberStart, memberEnd);
  const id = parsePropertyKey(source, memberStart, colon);
  const [valueStart, valueEnd] = trimRange(source, colon + 1, memberEnd);
  if (source[valueStart] !== "{") extractionError(`agent ${id} is not an object`);
  const objectEnd = scanBalanced(source, valueStart, valueEnd);
  if (objectEnd !== valueEnd - 1) extractionError(`ambiguous agent ${id} object`);

  let name;
  let rule;
  for (const [propertyStart, propertyEnd] of topLevelSegments(source, valueStart + 1, objectEnd)) {
    const propertyColon = topLevelColon(source, propertyStart, propertyEnd);
    const property = parsePropertyKey(source, propertyStart, propertyColon);
    if (property === "name") {
      if (name !== undefined) extractionError(`duplicate name in agent ${id}`);
      name = parseStringLiteral(source, propertyColon + 1, propertyEnd);
    } else if (property === "globalSkillsDir") {
      if (rule !== undefined) extractionError(`duplicate globalSkillsDir in agent ${id}`);
      rule = parseRuleExpression(source, propertyColon + 1, propertyEnd);
    }
  }

  if (name === undefined) extractionError(`missing agent name for ${id}`);
  if (name !== id) extractionError(`agent key/name mismatch for ${id}`);
  if (rule === undefined) extractionError(`missing globalSkillsDir for ${id}`);
  return { id, rule };
}

function normalizeWhitespace(value) {
  return value.replace(/\s+/g, " ").trim();
}

function assertRecognizedAnchors(source, expectedVersion) {
  const homeAndConfigAnchor =
    'const home = homedir();\nconst configHome = xdgConfig ?? join(home, ".config");';
  const homeIndex = uniqueAnchor(source, homeAndConfigAnchor, "home/config");
  const configIndex = homeIndex + "const home = homedir();\n".length;
  const declarations = [
    'const codexHome = process.env.CODEX_HOME?.trim() || join(home, ".codex");',
    'const claudeHome = process.env.CLAUDE_CONFIG_DIR?.trim() || join(home, ".claude");',
    'const vibeHome = process.env.VIBE_HOME?.trim() || join(home, ".vibe");',
    'const hermesHome = process.env.HERMES_HOME?.trim() || join(home, ".hermes");',
    'const autohandHome = process.env.AUTOHAND_HOME?.trim() || join(home, ".autohand");',
    'const grokHome = process.env.GROK_HOME?.trim() || join(home, ".grok");',
  ];
  for (const declaration of declarations) uniqueAnchor(source, declaration, "environment-home");
  if (configIndex < homeIndex) extractionError("ambiguous home declaration order");

  const versionAnchor = uniqueAnchor(source, "var version$1 = ", "version");
  const versionStart = versionAnchor + "var version$1 = ".length;
  const versionEnd = skipQuoted(source, versionStart);
  const versionLiteral = parseStringLiteral(source, versionStart, versionEnd);
  if (source[versionEnd] !== ";") extractionError("ambiguous version source form");
  if (versionLiteral !== expectedVersion) extractionError(`skills version mismatch: ${versionLiteral}`);

  const openclawAnchorText = "function getOpenClawGlobalSkillsDir(homeDir = home, pathExists = existsSync) {";
  const openclawIndex = uniqueAnchor(source, openclawAnchorText, "OpenClaw resolver");
  const openclawOpen = openclawIndex + openclawAnchorText.length - 1;
  const openclawEnd = scanBalanced(source, openclawOpen);
  const openclawBody = normalizeWhitespace(source.slice(openclawOpen + 1, openclawEnd));
  const expectedOpenclawBody = normalizeWhitespace([
    'if (pathExists(join(homeDir, ".openclaw"))) return join(homeDir, ".openclaw/skills");',
    'if (pathExists(join(homeDir, ".clawdbot"))) return join(homeDir, ".clawdbot/skills");',
    'if (pathExists(join(homeDir, ".moltbot"))) return join(homeDir, ".moltbot/skills");',
    'return join(homeDir, ".openclaw/skills");',
  ].join(" "));
  if (openclawBody !== expectedOpenclawBody) extractionError("unrecognized OpenClaw resolver source form");

  return { homeIndex };
}

function extractDocument(source, expectedVersion = PINNED_SKILLS_VERSION) {
  if (typeof source !== "string") extractionError("bundle source must be text");
  if (Buffer.byteLength(source, "utf8") > MAX_SOURCE_BYTES) {
    extractionError("bundle source is oversized");
  }

  assertRecognizedAnchors(source, expectedVersion);
  const agentsAnchor = "const agents = {";
  const agentsIndex = uniqueAnchor(source, agentsAnchor, "agents");
  const prelude = source.slice(0, agentsIndex);
  const homeDeclarations = occurrences(prelude, "const home = homedir();");
  if (homeDeclarations.length !== 1) extractionError("duplicate or missing home anchor");
  const agentsOpen = agentsIndex + agentsAnchor.length - 1;
  const agentsEnd = scanBalanced(source, agentsOpen);
  let terminator = agentsEnd + 1;
  while (terminator < source.length && /\s/.test(source[terminator])) terminator += 1;
  if (source[terminator] !== ";") extractionError("unterminated agents section");

  const agentSegments = topLevelSegments(source, agentsOpen + 1, agentsEnd);
  if (agentSegments.length === 0) extractionError("empty agents result");
  const agents = agentSegments.map(([start, end]) => parseAgent(source, start, end));
  const ids = new Set();
  for (const agent of agents) {
    if (ids.has(agent.id)) extractionError(`duplicate agent id: ${agent.id}`);
    ids.add(agent.id);
  }

  const globalTargetCount = agents.filter((agent) => agent.rule.kind !== "no-global-target").length;
  const noGlobalTargetCount = agents.length - globalTargetCount;
  if (
    agents.length !== EXPECTED_AGENT_COUNT ||
    globalTargetCount !== EXPECTED_GLOBAL_TARGET_COUNT ||
    noGlobalTargetCount !== EXPECTED_NO_GLOBAL_TARGET_COUNT
  ) {
    extractionError(
      `rule count mismatch (agents=${agents.length}, global=${globalTargetCount}, no-global=${noGlobalTargetCount})`
    );
  }

  return {
    schemaVersion: RULE_SCHEMA_VERSION,
    skillsVersion: expectedVersion,
    agentCount: agents.length,
    globalTargetCount,
    noGlobalTargetCount,
    agents,
  };
}

function readBoundedText(path) {
  const descriptor = openSync(path, "r");
  try {
    const size = fstatSync(descriptor).size;
    if (size > MAX_SOURCE_BYTES) extractionError("bundle source is oversized");
    const bytes = Buffer.alloc(size);
    let offset = 0;
    while (offset < size) {
      const count = readSync(descriptor, bytes, offset, size - offset, offset);
      if (count === 0) extractionError("bundle source ended while reading");
      offset += count;
    }
    return bytes.toString("utf8");
  } finally {
    closeSync(descriptor);
  }
}

export function resolvePinnedSource() {
  const cliPath = require.resolve("skills/bin/cli.mjs");
  const distPath = resolve(dirname(cliPath), "..", "dist", "cli.mjs");
  return { cliPath, distPath };
}

export function extractRulesFromSource(source) {
  return extractDocument(source, PINNED_SKILLS_VERSION);
}

export function generateRulesDocument() {
  const { distPath } = resolvePinnedSource();
  return extractRulesFromSource(readBoundedText(distPath));
}

function serialized(document) {
  return `${JSON.stringify(document, null, 2)}\n`;
}

function runCommand(argv) {
  if (argv.length !== 2 || !["--write", "--check"].includes(argv[0]) || !argv[1]) {
    throw new Error("usage: extract-skills-rules.mjs --write|--check <path>");
  }
  const expected = serialized(generateRulesDocument());
  if (argv[0] === "--write") {
    mkdirSync(dirname(resolve(argv[1])), { recursive: true });
    writeFileSync(argv[1], expected, "utf8");
    return;
  }
  let actual;
  try {
    actual = readFileSync(argv[1], "utf8");
  } catch (error) {
    throw new Error(`unable to read generated rules: ${error.message}`, { cause: error });
  }
  if (actual !== expected) throw new Error("generated skills rules differ; run with --write");
}

const invokedPath = process.argv[1] ? resolve(process.argv[1]) : "";
if (invokedPath === fileURLToPath(import.meta.url)) {
  try {
    runCommand(process.argv.slice(2));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
