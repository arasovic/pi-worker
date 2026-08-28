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

const DETECTOR_ENVIRONMENT_SPECS = Object.freeze({
  codexHome: { variable: "CODEX_HOME", fallback: ".codex" },
  claudeHome: { variable: "CLAUDE_CONFIG_DIR", fallback: ".claude" },
  vibeHome: { variable: "VIBE_HOME", fallback: ".vibe" },
  hermesHome: { variable: "HERMES_HOME", fallback: ".hermes" },
  autohandHome: { variable: "AUTOHAND_HOME", fallback: ".autohand" },
  grokHome: { variable: "GROK_HOME", fallback: ".grok" },
  zedAppDataHome: { variable: "APPDATA" },
  zedFlatpakConfigHome: { variable: "FLATPAK_XDG_CONFIG_HOME" },
});

const DETECTOR_HELPERS = Object.freeze({
  isZCodeInstalled: Object.freeze([
    { kind: "home-relative", path: ".zcode" },
    { kind: "absolute", path: "/Applications/ZCode.app" },
  ]),
  isKimchiInstalled: Object.freeze([
    { kind: "home-relative", path: ".config/kimchi" },
  ]),
  isMiniMaxCodeInstalled: Object.freeze([
    { kind: "home-relative", path: ".minimax" },
    { kind: "absolute", path: "/Applications/MiniMax Code.app" },
  ]),
  isPositAssistantInstalled: Object.freeze([
    { kind: "home-relative", path: ".posit/assistant" },
    { kind: "home-relative", path: ".positai" },
  ]),
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

function splitTopLevelOperator(source, start, end, operator) {
  const matches = [];
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
      if (!open || pairs[open] !== character) extractionError("mismatched detector delimiters");
      continue;
    }
    if (stack.length === 0 && source.startsWith(operator, index)) {
      matches.push(index);
      index += operator.length - 1;
    }
  }
  if (stack.length !== 0) extractionError("unterminated detector expression");
  return matches;
}

function detectorPathExpression(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  const expression = source.slice(trimmedStart, trimmedEnd);
  if (expression.startsWith("join")) {
    const openIndex = trimmedStart + 4;
    if (source[openIndex] !== "(") extractionError("unrecognized detector join expression");
    const closeIndex = scanBalanced(source, openIndex, trimmedEnd);
    if (closeIndex !== trimmedEnd - 1) extractionError("ambiguous detector join expression");
    const argumentsList = topLevelSegments(source, openIndex + 1, closeIndex);
    if (argumentsList.length < 2) extractionError("empty detector join expression");
    const baseText = source.slice(...trimRange(source, argumentsList[0][0], argumentsList[0][1]));
    const base = baseText === "process.cwd()"
      ? "cwd"
      : parseIdentifier(source, argumentsList[0][0], argumentsList[0][1]);
    const literals = argumentsList.slice(1).map(([argumentStart, argumentEnd]) => (
      parseStringLiteral(source, argumentStart, argumentEnd)
    ));
    if (base === "home") return { kind: "home-relative", path: pathFromSegments(literals) };
    if (base === "configHome") return { kind: "config-home-relative", path: pathFromSegments(literals) };
    if (base === "cwd") return { kind: "cwd-relative", path: pathFromSegments(literals) };
    if (base === "process") extractionError("unrecognized detector process path expression");
    if (Object.hasOwn(DETECTOR_ENVIRONMENT_SPECS, base)) {
      const specification = DETECTOR_ENVIRONMENT_SPECS[base];
      if (!specification.variable || literals.length !== 1) extractionError("unrecognized detector environment path");
      return { kind: "environment-relative", variable: specification.variable, suffix: pathFromSegments(literals) };
    }
    extractionError(`unrecognized detector join base: ${base}`);
  }
  if (expression === "process.cwd()") extractionError("cwd detector must include a relative suffix");
  if (Object.hasOwn(DETECTOR_ENVIRONMENT_SPECS, expression)) {
    const specification = DETECTOR_ENVIRONMENT_SPECS[expression];
    if (!specification.fallback) {
      return { kind: "environment-relative", variable: specification.variable, suffix: "" };
    }
    return {
      kind: "environment-or-home",
      variable: specification.variable,
      fallback: specification.fallback,
    };
  }
  const literal = parseStringLiteral(source, trimmedStart, trimmedEnd);
  if (!literal.startsWith("/")) extractionError("detector absolute path must be absolute");
  return { kind: "absolute", path: literal };
}

function parseExistsDetector(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  if (!source.slice(trimmedStart, trimmedEnd).startsWith("existsSync")) {
    extractionError("unrecognized detector predicate");
  }
  const openIndex = trimmedStart + "existsSync".length;
  if (source[openIndex] !== "(") extractionError("unrecognized existsSync detector");
  const closeIndex = scanBalanced(source, openIndex, trimmedEnd);
  if (closeIndex !== trimmedEnd - 1) extractionError("ambiguous existsSync detector");
  return detectorPathExpression(source, openIndex + 1, closeIndex);
}

function parseDependencyDetector(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  const text = source.slice(trimmedStart, trimmedEnd);
  const prefix = "packageJsonHasDependency(";
  if (!text.startsWith(prefix)) extractionError("unrecognized Eve detector predicate");
  const openIndex = trimmedStart + prefix.length - 1;
  const closeIndex = scanBalanced(source, openIndex, trimmedEnd);
  if (closeIndex !== trimmedEnd - 1) extractionError("ambiguous Eve detector predicate");
  const argumentsList = topLevelSegments(source, openIndex + 1, closeIndex);
  if (argumentsList.length !== 2) extractionError("invalid Eve dependency detector");
  const packagePath = detectorPathExpression(source, argumentsList[0][0], argumentsList[0][1]);
  const dependency = parseStringLiteral(source, argumentsList[1][0], argumentsList[1][1]);
  if (packagePath.kind !== "cwd-relative" || packagePath.path !== "package.json" || dependency !== "eve") {
    extractionError("unrecognized Eve dependency detector");
  }
  return true;
}

function parseDetectorExpression(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  const expression = source.slice(trimmedStart, trimmedEnd);
  if (expression === "false") return { kind: "never" };
  if (expression.endsWith("()") && Object.hasOwn(DETECTOR_HELPERS, expression.slice(0, -2))) {
    const paths = DETECTOR_HELPERS[expression.slice(0, -2)];
    return { kind: "any-existing", paths: paths.map((path) => ({ ...path })) };
  }
  const orOperators = splitTopLevelOperator(source, trimmedStart, trimmedEnd, "||");
  if (orOperators.length > 0) {
    const paths = [];
    let partStart = trimmedStart;
    for (const operatorIndex of [...orOperators, trimmedEnd]) {
      const partEnd = operatorIndex;
      const andOperators = splitTopLevelOperator(source, partStart, partEnd, "&&");
      if (andOperators.length === 0) {
        paths.push(parseExistsDetector(source, partStart, partEnd));
      } else {
        if (andOperators.length !== 1) extractionError("unrecognized detector helper conjunction");
        const leftEnd = andOperators[0];
        const left = source.slice(...trimRange(source, partStart, leftEnd));
        if (!left.startsWith("!!")) extractionError("unrecognized detector helper guard");
        const variable = parseIdentifier(left, 2, left.length);
        const pathExpression = parseExistsDetector(source, leftEnd + 2, partEnd);
        if (pathExpression.kind !== "environment-relative" || pathExpression.variable !== DETECTOR_ENVIRONMENT_SPECS[variable]?.variable) {
          extractionError("unrecognized detector helper path");
        }
        paths.push(pathExpression);
      }
      partStart = operatorIndex + 2;
    }
    return { kind: "any-existing", paths };
  }
  const andOperators = splitTopLevelOperator(source, trimmedStart, trimmedEnd, "&&");
  if (andOperators.length > 0) {
    if (andOperators.length !== 1) extractionError("unrecognized detector conjunction");
    const leftEnd = andOperators[0];
    const rightStart = leftEnd + 2;
    const agentPath = parseExistsDetector(source, trimmedStart, leftEnd);
    const dependency = parseDependencyDetector(source, rightStart, trimmedEnd);
    if (dependency !== true || agentPath.kind !== "cwd-relative" || agentPath.path !== "agent") {
      extractionError("unrecognized detector conjunction");
    }
    return {
      kind: "eve-project",
      agentPath,
      packageJsonPath: { kind: "cwd-relative", path: "package.json" },
      dependency: "eve",
    };
  }
  return { kind: "any-existing", paths: [parseExistsDetector(source, trimmedStart, trimmedEnd)] };
}

function parseDetectorFunction(source, start, end) {
  const [trimmedStart, trimmedEnd] = trimRange(source, start, end);
  const prefix = "async () =>";
  if (!source.slice(trimmedStart, trimmedEnd).startsWith(prefix)) extractionError("unrecognized detector function");
  const bodyStart = trimmedStart + prefix.length;
  const [bodyTrimmedStart, bodyTrimmedEnd] = trimRange(source, bodyStart, trimmedEnd);
  if (source[bodyTrimmedStart] === "{") {
    const bodyEnd = scanBalanced(source, bodyTrimmedStart, trimmedEnd);
    if (bodyEnd !== trimmedEnd - 1) extractionError("ambiguous detector function");
    const bodyStartText = bodyTrimmedStart + 1;
    const cwdPrelude = "const cwd = process.cwd();";
    let statementStart = bodyStartText;
    const [preludeStart, preludeEnd] = trimRange(source, statementStart, bodyEnd);
    if (source.slice(preludeStart, preludeEnd).startsWith(cwdPrelude)) statementStart = preludeStart + cwdPrelude.length;
    const returnPrefix = "return ";
    [statementStart] = trimRange(source, statementStart, bodyEnd);
    const [returnStart, statementEnd] = trimRange(source, statementStart, bodyEnd);
    if (!source.slice(returnStart, statementEnd).startsWith(returnPrefix) || source[statementEnd - 1] !== ";") {
      extractionError("unrecognized detector return form");
    }
    return parseDetectorExpression(source, returnStart + returnPrefix.length, statementEnd - 1);
  }
  return parseDetectorExpression(source, bodyTrimmedStart, bodyTrimmedEnd);
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
  let skillsDir;
  let rule;
  let detector;
  for (const [propertyStart, propertyEnd] of topLevelSegments(source, valueStart + 1, objectEnd)) {
    const propertyColon = topLevelColon(source, propertyStart, propertyEnd);
    const property = parsePropertyKey(source, propertyStart, propertyColon);
    if (property === "name") {
      if (name !== undefined) extractionError(`duplicate name in agent ${id}`);
      name = parseStringLiteral(source, propertyColon + 1, propertyEnd);
    } else if (property === "skillsDir") {
      if (skillsDir !== undefined) extractionError(`duplicate skillsDir in agent ${id}`);
      skillsDir = parseStringLiteral(source, propertyColon + 1, propertyEnd);
    } else if (property === "globalSkillsDir") {
      if (rule !== undefined) extractionError(`duplicate globalSkillsDir in agent ${id}`);
      rule = parseRuleExpression(source, propertyColon + 1, propertyEnd);
    } else if (property === "detectInstalled") {
      if (detector !== undefined) extractionError(`duplicate detector in agent ${id}`);
      detector = parseDetectorFunction(source, propertyColon + 1, propertyEnd);
    }
  }

  if (name === undefined) extractionError(`missing agent name for ${id}`);
  if (name !== id) extractionError(`agent key/name mismatch for ${id}`);
  if (skillsDir === undefined) extractionError(`missing skillsDir for ${id}`);
  if (rule === undefined) extractionError(`missing globalSkillsDir for ${id}`);
  if (detector === undefined) extractionError(`missing detector for ${id}`);
  return { id, usesUniversalTarget: skillsDir === ".agents/skills", rule, detector };
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
    'const zedAppDataHome = process.env.APPDATA?.trim();',
    'const zedFlatpakConfigHome = process.env.FLATPAK_XDG_CONFIG_HOME?.trim();',
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
  const helperAnchors = [
    ["ZCode helper", "function isZCodeInstalled(homeDir = home, pathExists = existsSync) {", [
      'return pathExists(join(homeDir, ".zcode")) || pathExists("/Applications/ZCode.app");',
    ]],
    ["Kimchi helper", "function isKimchiInstalled(homeDir = home, pathExists = existsSync) {", [
      'return pathExists(join(homeDir, ".config", "kimchi"));',
    ]],
    ["MiniMax Code helper", "function isMiniMaxCodeInstalled(homeDir = home, pathExists = existsSync) {", [
      'return pathExists(join(homeDir, ".minimax")) || pathExists("/Applications/MiniMax Code.app");',
    ]],
    ["Posit Assistant helper", "function isPositAssistantInstalled(homeDir = home, pathExists = existsSync) {", [
      'return pathExists(join(homeDir, ".posit/assistant")) || pathExists(join(homeDir, ".positai"));',
    ]],
  ];
  for (const [name, anchor, expectedLines] of helperAnchors) {
    const helperIndex = uniqueAnchor(source, anchor, name);
    const helperOpen = helperIndex + anchor.length - 1;
    const helperEnd = scanBalanced(source, helperOpen);
    const helperBody = normalizeWhitespace(source.slice(helperOpen + 1, helperEnd));
    if (helperBody !== normalizeWhitespace(expectedLines.join(" "))) {
      extractionError(`unrecognized ${name} source form`);
    }
  }

  const openclawBody = normalizeWhitespace(source.slice(openclawOpen + 1, openclawEnd));
  const expectedOpenclawBody = normalizeWhitespace([
    'if (pathExists(join(homeDir, ".openclaw"))) return join(homeDir, ".openclaw/skills");',
    'if (pathExists(join(homeDir, ".clawdbot"))) return join(homeDir, ".clawdbot/skills");',
    'if (pathExists(join(homeDir, ".moltbot"))) return join(homeDir, ".moltbot/skills");',
    'return join(homeDir, ".openclaw/skills");',
  ].join(" "));
  if (openclawBody !== expectedOpenclawBody) extractionError("unrecognized OpenClaw resolver source form");

  const detectorAnchors = [
    'return existsSync(join(home, ".openclaw")) || existsSync(join(home, ".clawdbot")) || existsSync(join(home, ".moltbot"));',
    'return existsSync(codexHome) || existsSync("/etc/codex");',
    'return existsSync(join(process.cwd(), ".promptscript")) || existsSync(join(process.cwd(), "promptscript.yaml"));',
    'return existsSync(join(configHome, "zed")) || !!zedAppDataHome && existsSync(join(zedAppDataHome, "Zed")) || !!zedFlatpakConfigHome && existsSync(join(zedFlatpakConfigHome, "zed"));',
    'return existsSync(join(cwd, "agent")) && packageJsonHasDependency(join(cwd, "package.json"), "eve");',
  ];
  for (const [index, anchor] of detectorAnchors.entries()) uniqueAnchor(source, anchor, `detector ${index + 1}`);
  uniqueAnchor(
    source,
    'return agents[type].skillsDir === ".agents/skills";',
    "universal target behavior"
  );

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
  if (agents.some((agent) => !agent.detector)) extractionError("partial detector extraction");
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
