import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const repository = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const readmePath = join(repository, "README.md");
const usagePath = join(repository, "docs", "v0-usage.md");
const contributingPath = join(repository, "CONTRIBUTING.md");
const securityPath = join(repository, "SECURITY.md");
const skillPath = join(repository, "skills", "pi-worker", "SKILL.md");
const readme = readFileSync(readmePath, "utf8");
const usage = readFileSync(usagePath, "utf8");
const contributing = existsSync(contributingPath) ? readFileSync(contributingPath, "utf8") : "";
const security = existsSync(securityPath) ? readFileSync(securityPath, "utf8") : "";
const skill = readFileSync(skillPath, "utf8");
const normalizedSecurity = security.replace(/\s+/g, " ");
const packageManifest = JSON.parse(readFileSync(join(repository, "package.json"), "utf8"));
const npmReadmeTargets = ["CONTRIBUTING.md", "SECURITY.md", "LICENSE", "THIRD_PARTY_NOTICES"];

const sections = [
  "What is it?",
  "Why does it exist?",
  "How do I use it?",
  "Requirements",
  "Install with npm",
  "First run",
  "Use from a coding agent",
  "Run independent tasks in parallel",
  "What gets installed",
  "Safety",
  "Install from GitHub Releases",
  "Troubleshooting",
  "Advanced documentation",
  "License",
];

function headingPositions(markdown) {
  return sections.map((section) => {
    const match = markdown.match(new RegExp(`^## ${section.replace(/[?]/g, "\\?")}\\s*$`, "m"));
    return match?.index ?? -1;
  });
}

function relativeLinks(markdown) {
  return [...markdown.matchAll(/\[[^\]]+\]\(([^)]+)\)/g)]
    .map(([, target]) => target.split("#", 1)[0].replace(/^\.\//, ""))
    .filter((target) => target && !/^[a-z][a-z+.-]*:/i.test(target) && !target.startsWith("//"));
}

function escapeRegex(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

test("README is the concise public entry point with the approved contract", () => {
  const positions = headingPositions(readme);
  assert.ok(positions.every((position) => position >= 0), "all required sections are present");
  assert.deepEqual([...positions].sort((a, b) => a - b), positions, "sections are in the approved order");
  assert.deepEqual(
    [...readme.matchAll(/^## (.+)$/gm)].map(([, heading]) => heading),
    sections,
    "README has exactly the approved section headings",
  );

  for (const exactText of [
    "npm install -g pi-worker",
    "npm install -g --foreground-scripts pi-worker",
    "pi-worker doctor",
    "pi-worker models",
    "pi-worker config set default-model provider/model",
    'pi-worker run --thinking high --task "Review this module and explain the main risks"',
    "Use pi-worker with provider/model at high effort to complete this task.",
    "pi-worker skill status",
    "pi-worker skill status --json",
    "pi-worker skill receipt-path",
    "npx --yes skills@1.5.22 list -g",
    "npx --yes skills@1.5.22 remove pi-worker -g -y",
  ]) {
    assert.ok(readme.includes(exactText), `README includes: ${exactText}`);
  }

  for (const check of ["pi-executable", "pi-version", "config", "model-catalog", "default-model"]) {
    assert.ok(readme.includes(`\`${check}\``), `README includes doctor check: ${check}`);
  }
  const doctorChecks = readme.slice(readme.indexOf("Its five checks"));
  assert.ok(doctorChecks.indexOf("pi-executable") < doctorChecks.indexOf("pi-version"));
  assert.ok(doctorChecks.indexOf("pi-version") < doctorChecks.indexOf("config"));
  assert.ok(doctorChecks.indexOf("config") < doctorChecks.indexOf("model-catalog"));
  assert.ok(doctorChecks.indexOf("model-catalog") < doctorChecks.indexOf("default-model"));

  for (const safetyStatement of [
    "current writable workspace",
    "parallel writes must target disjoint files",
    "bash has the current user's host permissions",
    "not a sandbox",
    "never silently changes",
    "same model",
    "reports the fallback",
  ]) {
    assert.match(readme, new RegExp(safetyStatement.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "i"));
  }
  assert.equal(packageManifest.private, true, "package remains private");
  assert.equal(packageManifest.engines.node, ">=22.20.0", "README requirement matches package lower bound");
  assert.match(readme, /not published to npm yet/i);
  assert.match(readme, /intended\s+post-publication install path/i);
  assert.match(readme, /not currently usable from the registry/i);
  assert.match(readme, /source-build instructions.*docs\/v0-usage\.md/s);
  assert.match(readme, /use\s+`\.\/bin\/pi-worker` after a source build/i);
  assert.match(readme, /four native binaries for macOS\/Linux arm64\/x64/i);
  assert.match(readme, /npm package supports only macOS and Linux on arm64 and x64/i);
  assert.match(readme, /Windows[\s\S]*build from source[\s\S]*compile-checked[\s\S]*not runtime-tested/i);
  assert.match(readme, /launcher selects the matching binary at runtime/i);
  assert.match(readme, /installation does not remove\s+the others/i);
  assert.match(readme, /canonical provider-neutral.*skill/s);
  assert.match(readme, /npm install attempts to install.*detected\s+agent targets/s);
  assert.match(readme, /pinned `skills@1\.5\.22`/);
  assert.match(readme, /installed,\s*blocked,\s*skipped,\s*or failed outcome.*durable receipt/s);
  assert.match(readme, /existing conflicts\s+may block,\s*skip,\s*or fail.*without overwriting/is);
  assert.match(readme, /Node\.js 22\.20\.0 or newer/);
  assert.match(usage, /Node\.js 22\.20\.0 or newer/);
  assert.match(usage, /source build/i);
  assert.match(usage, /`\.\/bin\/pi-worker`/);
  assert.match(usage, /human version output is `pi-worker dev`/i);
  assert.match(usage, /unsupported npm platform[\s\S]*skip before[\s\S]*receipt/i);
  assert.match(readme, /not published yet/i);
  assert.match(readme, /release links will be added when available/i);
  assert.doesNotMatch(readme, /branding\/publication gate/i);
  assert.match(readme, /durable receipt/);
  assert.match(readme, /identity\s+marker/);
  assert.match(readme, /externally managed/);
  assert.match(readme, /markerless|foreign|mixed/i);

  assert.doesNotMatch(readme, /<repository-url>|<owner>|<repo>|<package-name>/i);
  assert.doesNotMatch(readme, /\/(?:Users|home|tmp)\//);
  assert.doesNotMatch(readme, /(?:openai|anthropic|google|gemini|claude|gpt-[\w-]+)/i);
  assert.doesNotMatch(readme, /!\[/, "no badge wall");
  assert.doesNotMatch(readme, /(?:npm|package-manager) distribution is deferred|packaging is source-only/i);

  const firstTaskCommand = readme.indexOf('pi-worker run --thinking high --task "Review this module and explain the main risks"');
  const earlySafetyStart = readme.indexOf("> **Safety before running a worker task:**");
  assert.ok(earlySafetyStart >= 0 && earlySafetyStart < firstTaskCommand, "early safety callout precedes the first task command");
  const earlySafety = readme
    .slice(earlySafetyStart, firstTaskCommand)
    .replace(/^>\s?/gm, "")
    .replace(/\s+/g, " ");
  for (const safetyPhrase of [
    "modify the current writable workspace",
    "execute `bash` with the user's host permissions",
    "not a sandbox or worktree layer",
    "trusted workspace only",
    "parallel tasks must be disjoint",
  ]) {
    assert.match(earlySafety, new RegExp(safetyPhrase.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "i"));
  }

  const selectorInstruction = /Replace `provider\/model` with one exact selector\s+printed by `pi-worker models` before config set/;
  assert.match(readme, selectorInstruction, "README explains selector replacement");
  assert.ok(readme.search(selectorInstruction) < readme.indexOf("pi-worker config set default-model provider/model"));
});

test("installed skill states the worker authority boundary before delegation", () => {
  const normalizedSkill = skill.replace(/\s+/g, " ");
  assert.match(normalizedSkill, /current writable workspace/i);
  assert.match(normalizedSkill, /bash.*current user's host permissions/i);
  assert.match(normalizedSkill, /not a sandbox or worktree/i);
  assert.match(normalizedSkill, /Luna Max.*--thinking max/i);
  assert.doesNotMatch(normalizedSkill, /Spark Max/i);
});

test("README links resolve and the npm tarball has one root README", () => {
  for (const target of relativeLinks(readme)) {
    assert.equal(existsSync(resolve(repository, target)), true, `relative link resolves: ${target}`);
  }

  for (const target of npmReadmeTargets) {
    assert.ok(relativeLinks(readme).includes(target), `README links to ${target}`);
    assert.equal(existsSync(resolve(repository, target)), true, `required public document resolves: ${target}`);
  }

  const manifest = JSON.parse(readFileSync(join(repository, "package.json"), "utf8"));
  const readmeEntries = manifest.files.filter((entry) => /README\.md$/i.test(entry));
  assert.deepEqual(readmeEntries, ["README.md"], "package allowlist has one root README");

  const packed = spawnSync("npm", ["pack", "--dry-run", "--json", "--ignore-scripts"], {
    cwd: repository,
    encoding: "utf8",
  });
  assert.equal(packed.status, 0, packed.stderr);
  const metadata = JSON.parse(packed.stdout)[0];
  const packedPaths = new Set(metadata.files.map(({ path }) => path));
  assert.deepEqual(metadata.files.map(({ path }) => path).filter((path) => /README\.md$/i.test(path)), ["README.md"]);
  for (const target of npmReadmeTargets) {
    assert.ok(packedPaths.has(target), `npm tarball contains README target: ${target}`);
  }
  assert.equal(existsSync(join(repository, metadata.filename)), false, "dry run leaves no tarball");
});

test("contribution guidance covers the public workflow and local checks", () => {
  assert.match(contributing, /prerequisites/i);
  assert.match(contributing, /go\.mod|Go toolchain/i);
  assert.match(contributing, /Node(?:\.js)?\s*>=?\s*22\.20\.0/i);
  assert.match(contributing, /npm/i);
  assert.match(contributing, /Pi\s+(?:CLI\s+)?0\.84\.1.*(?:integration|dogfood)/is);
  assert.match(contributing, /fork/i);
  assert.match(contributing, /purpose[- ]named branch/i);
  assert.match(contributing, /focused changes/i);
  assert.match(contributing, /English.*(?:code|comments|docs|commit messages)/is);
  for (const check of ["gofmt", "go vet", "-race", "go build", "npm test", "npm run verify", "rules", "notices", "git diff --check"]) {
    assert.match(contributing, new RegExp(escapeRegex(check), "i"), `CONTRIBUTING.md includes ${check}`);
  }
  for (const sensitiveItem of ["dist", "npm/native", "tgz", "credentials", "Pi profiles", "provider config", "prompts", "workspace contents"]) {
    assert.match(contributing, new RegExp(`(?:do not|don't|never)[^\\n]*${escapeRegex(sensitiveItem)}`, "i"), `CONTRIBUTING.md protects ${sensitiveItem}`);
  }
  assert.match(contributing, /security reports follow SECURITY\.md/i);
  assert.doesNotMatch(contributing, /Co-Authored-By/i);
  assert.doesNotMatch(contributing, /(?:agent workflow|private plan|work log|review metadata|\/Users\/|\/home\/|\/tmp\/)/i);
});

test("security guidance states the current public reporting boundary", () => {
  for (const warning of ["credentials", "Pi profiles", "provider configuration", "prompts", "workspace contents", "public issues"]) {
    assert.match(normalizedSecurity, new RegExp(escapeRegex(warning), "i"), `SECURITY.md mentions ${warning}`);
  }
  assert.match(normalizedSecurity, /do not.*(?:post|disclose).*public issues/i);
  assert.match(normalizedSecurity, /no public security[- ]reporting channel is active/i);
  assert.match(normalizedSecurity, /GitHub private vulnerability reporting/i);
  assert.match(normalizedSecurity, /after.*public.*(?:repository|feature).*exists/i);
  assert.match(normalizedSecurity, /public release must not be made until.*channel is enabled/i);
  assert.match(normalizedSecurity, /v0\.1/i);
  assert.match(normalizedSecurity, /workers?.*execute.*bash.*current user.*permissions/i);
  assert.match(normalizedSecurity, /current writable workspace/i);
  assert.match(normalizedSecurity, /not a sandbox/i);
  assert.match(normalizedSecurity, /do not disclose publicly/i);
  assert.doesNotMatch(security, /https?:\/\/|mailto:|[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}/i);

  const publicDocs = `${readme}\n${contributing}\n${security}`;
  assert.doesNotMatch(publicDocs, /https?:\/\/|mailto:|[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}/i);
  assert.doesNotMatch(publicDocs, /(?:\.github\/workflows|private plan|work log|review metadata|\/Users\/|\/home\/|\/tmp\/)/i);
});
