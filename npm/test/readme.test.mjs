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
const piPin = JSON.parse(readFileSync(join(repository, "compat", "pi", "package.json"), "utf8"));
const piVersion = piPin.dependencies["@earendil-works/pi-coding-agent"];
const npmReadmeTargets = ["CONTRIBUTING.md", "SECURITY.md", "LICENSE", "THIRD_PARTY_NOTICES"];

const sections = [
  "How it works",
  "Quick start",
  "Documentation",
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

function imageTargets(markdown) {
  return [...markdown.matchAll(/!\[[^\]]*\]\(([^)]+)\)|<img\s+[^>]*src="([^"]+)"/g)].map(([, markdownTarget, htmlTarget]) =>
    (markdownTarget ?? htmlTarget).replaceAll("&amp;", "&"),
  );
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
    "pi-worker models",
    "pi-worker doctor",
    "pi-worker config set default-model provider/model",
    'pi-worker run --thinking high --task "Review this module and explain the main risks"',
    "Use pi-worker with provider/model at high effort to complete this task.",
    "go install github.com/arasovic/pi-worker/cmd/pi-worker@latest",
    "pi-worker skill status",
  ]) {
    assert.ok(readme.includes(exactText), `README includes: ${exactText}`);
  }

  assert.match(readme, /not a sandbox/i);
  assert.match(readme, /never silently changes/i);
  assert.equal(packageManifest.name, "pi-worker");
  assert.match(
    packageManifest.version,
    /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/,
    "package version is a stable semver: three dot-separated numeric components, no prerelease or build suffix",
  );
  assert.notEqual(packageManifest.private, true, "package is publishable");
  assert.equal(packageManifest.license, "MIT");
  assert.deepEqual(packageManifest.repository, {
    type: "git",
    url: "git+https://github.com/arasovic/pi-worker.git",
  });
  assert.equal(packageManifest.homepage, "https://github.com/arasovic/pi-worker#readme");
  assert.deepEqual(packageManifest.bugs, { url: "https://github.com/arasovic/pi-worker/issues" });
  assert.equal(packageManifest.engines.node, ">=22.20.0", "README requirement matches package lower bound");
  assert.doesNotMatch(readme, /not published|not currently usable|intended\s+post-publication/i);
  assert.match(readme, /source builds.*docs\/v0-usage\.md/is);
  assert.match(readme, /Node\.js 22\.20\+/);
  assert.match(usage, /Node\.js 22\.20\.0 or newer/);
  assert.match(usage, /source build/i);
  assert.match(usage, /`\.\/bin\/pi-worker`/);
  assert.match(usage, /human version output is `pi-worker dev`/i);
  assert.match(usage, /unsupported npm platform[\s\S]*skip before[\s\S]*receipt/i);
  assert.ok(
    usage.includes("go install github.com/arasovic/pi-worker/cmd/pi-worker@latest"),
    "docs/v0-usage.md installs the module at @latest",
  );
  assert.doesNotMatch(usage, /cmd\/pi-worker@v\d/, "docs/v0-usage.md pins no release version");
  assert.doesNotMatch(readme, /branding\/publication gate/i);

  assert.doesNotMatch(readme, /<repository-url>|<owner>|<repo>|<package-name>/i);
  assert.doesNotMatch(readme, /\/(?:Users|home|tmp)\//);
  assert.doesNotMatch(readme, /(?:openai|anthropic|google|gemini|gpt-[\w-]+|claude(?!\s+code))/i);
  assert.deepEqual(
    imageTargets(readme),
    [
      "https://raw.githubusercontent.com/arasovic/pi-worker/main/assets/brand/github-social-preview.png",
      "https://github.com/arasovic/pi-worker/actions/workflows/ci.yml/badge.svg",
      "https://img.shields.io/npm/v/pi-worker.svg",
      "https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white",
      "https://img.shields.io/badge/MIT-green.svg",
    ],
    "README has the approved badges and project image",
  );
  assert.doesNotMatch(readme, /(?:npm|package-manager) distribution is deferred|packaging is source-only/i);

  const safetyStart = readme.indexOf("> **Safety:**");
  assert.ok(safetyStart >= 0, "README has a safety callout");
  const safetyEnd = readme.indexOf("\n\n", safetyStart);
  const safetyCallout = readme.slice(safetyStart, safetyEnd).replace(/^>\s?/gm, "").replace(/\s+/g, " ");
  for (const safetyPhrase of [
    "not a sandbox",
    "edit the current workspace",
    "current user's permissions",
    "disjoint files",
  ]) {
    assert.match(safetyCallout, new RegExp(safetyPhrase.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "i"));
  }

  for (const step of ["You keep your agent", "Small tasks go to cheaper models", "Results come back in order", "Your agent already knows how"]) {
    assert.ok(readme.includes(`- **${step}.**`), `README explains: ${step}`);
  }
});

test("installed skill states the worker authority boundary before delegation", () => {
  const normalizedSkill = skill.replace(/\s+/g, " ");
  assert.match(normalizedSkill, /current writable workspace/i);
  assert.match(normalizedSkill, /bash.*current user's host permissions/i);
  assert.match(normalizedSkill, /not a sandbox/i);
  assert.match(normalizedSkill, /separate working directory, not containment/i);
  assert.match(normalizedSkill, /cleanup is best-effort lifecycle recovery/i);
  assert.match(normalizedSkill, /not a sandbox or a no-escape guarantee/i);
  assert.match(normalizedSkill, /deliberately daemonized or reparented Unix descendants/i);
  assert.match(normalizedSkill, /processes spawned during teardown/i);
  assert.match(normalizedSkill, /Windows pre-assignment window can escape/i);
  assert.match(normalizedSkill, /exit of 7 or 8 means it was cut short/i);
  assert.match(normalizedSkill, /without a document, report interruption and stop/i);
  assert.match(normalizedSkill, /whatever the outcome, read and report/i);
  assert.match(normalizedSkill, /`3` `workers-unavailable`/);
  assert.match(normalizedSkill, /`5` `task-failed` or `partial`/);
  assert.match(normalizedSkill, /`6` `verification-failed`/);
  assert.match(normalizedSkill, /`runs wait` whose own `--timeout` runs out also exits 7/);
  assert.match(normalizedSkill, /a failed run's `changes` still lists what the workers wrote; nothing is rolled back/i);
  assert.match(normalizedSkill, /--background/);
  assert.match(normalizedSkill, /runs wait <id> --timeout <slice> --json/i);
  assert.match(normalizedSkill, /a wait that runs out leaves the run going/i);
  assert.match(normalizedSkill, /runs cancel <id> --json/i);
  assert.doesNotMatch(
    normalizedSkill,
    /(?:--background|slices?|threshold|host|command)[^.]{0,40}\d+\s*minutes?|\d+\s*minutes?[^.]{0,40}(?:--background|slices?|threshold|host|command)/i,
    "skill does not express the background trigger as a number of minutes"
  );
  assert.doesNotMatch(normalizedSkill, /Do not treat empty output as any kind of success/i);
  for (const field of [
    "model",
    "thinkingLevel",
    "status",
    "explanation",
    "partialExplanation",
    "error",
    "changes",
    "writes",
    "verification",
  ]) {
    assert.match(normalizedSkill, new RegExp("`" + field + "`"), `skill names ${field}`);
  }
  assert.match(normalizedSkill, /parent-started side jobs must self-terminate/i);
  assert.doesNotMatch(normalizedSkill, /worker's `failure`/i);
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
  assert.match(contributing, new RegExp(`Pi\\s+(?:CLI\\s+)?${escapeRegex(piVersion)}.*(?:integration|dogfood)`, "is"));
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
  assert.match(normalizedSecurity, /GitHub private vulnerability reporting/i);
  assert.match(security, /https:\/\/github\.com\/arasovic\/pi-worker\/security\/advisories\/new/);
  assert.match(normalizedSecurity, /if.*channel.*unavailable.*do not.*public/i);
  assert.match(
    normalizedSecurity,
    /(?:supported release[^.]*latest published|latest published[^.]*supported release)/i,
    "SECURITY.md states that the supported release is the latest published version",
  );
  assert.match(normalizedSecurity, /workers?.*execute.*bash.*current user.*permissions/i);
  assert.match(normalizedSecurity, /current writable workspace/i);
  assert.match(normalizedSecurity, /not a sandbox/i);
  assert.match(normalizedSecurity, /do not disclose publicly/i);
  assert.doesNotMatch(security, /mailto:|[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}/i);

  const publicDocs = `${readme}\n${contributing}\n${security}`;
  for (const [, rawUrl] of publicDocs.matchAll(/(https?:\/\/[^)"'\s>]+)/g)) {
    const url = rawUrl.replaceAll("&amp;", "&");
    assert.match(
      url,
      /^(?:https:\/\/github\.com\/arasovic\/pi-worker\/(?:actions\/workflows\/ci\.yml(?:\/badge\.svg)?|releases|security\/advisories\/new)$|https:\/\/www\.npmjs\.com\/package\/pi-worker$|https:\/\/img\.shields\.io\/(?:npm\/v\/pi-worker\.svg|badge\/(?:Go-1\.25%2B-00ADD8\?logo=go&logoColor=white|Node\.js-22\.20%2B-339933\?logo=nodedotjs&logoColor=white|macOS-000000\?logo=apple&logoColor=white|Linux-FCC624\?logo=linux&logoColor=black|Windows-compile%20only-0078D4|MIT-green\.svg))$|https:\/\/raw\.githubusercontent\.com\/arasovic\/pi-worker\/main\/assets\/brand\/github-social-preview\.png$|https:\/\/pi\.dev\/$)/,
    );
  }
  assert.doesNotMatch(publicDocs, /mailto:|[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}/i);
  assert.doesNotMatch(publicDocs, /(?:\.github\/workflows|private plan|work log|review metadata|\/Users\/|\/home\/|\/tmp\/)/i);
});
