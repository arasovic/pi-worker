import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const repository = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const readmePath = join(repository, "README.md");
const usagePath = join(repository, "docs", "v0-usage.md");
const readme = readFileSync(readmePath, "utf8");
const usage = readFileSync(usagePath, "utf8");
const packageManifest = JSON.parse(readFileSync(join(repository, "package.json"), "utf8"));

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
    .map(([, target]) => target.split("#", 1)[0])
    .filter((target) => target && !/^[a-z][a-z+.-]*:/i.test(target) && !target.startsWith("//"));
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
  assert.match(readme, /not published yet/i);
  assert.match(readme, /release links will be added when available/i);
  assert.doesNotMatch(readme, /branding\/publication gate/i);
  assert.match(readme, /durable receipt/);
  assert.match(readme, /identity\s+marker/);
  assert.match(readme, /receipt-untracked/);
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

test("README links resolve and the npm tarball has one root README", () => {
  for (const target of relativeLinks(readme)) {
    assert.equal(existsSync(resolve(repository, target)), true, `relative link resolves: ${target}`);
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
  assert.deepEqual(metadata.files.map(({ path }) => path).filter((path) => /README\.md$/i.test(path)), ["README.md"]);
  assert.equal(existsSync(join(repository, metadata.filename)), false, "dry run leaves no tarball");
});
