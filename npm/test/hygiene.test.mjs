import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  cpSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { test } from "node:test";

const repository = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const checker = join(repository, "npm", "scripts", "check-hygiene.mjs");
const ciWorkflow = readFileSync(join(repository, ".github", "workflows", "ci.yml"), "utf8");
const releaseRunbook = readFileSync(join(repository, "docs", "releasing.md"), "utf8");
const releaseWorkflow = (() => {
  try {
    return readFileSync(join(repository, ".github", "workflows", "release.yml"), "utf8");
  } catch {
    return null;
  }
})();
const joinParts = (...parts) => parts.join("");
const machineHome = joinParts("/Us", "ers/");
const codexMarker = joinParts(".", "cod", "ex");
const superpowersMarker = joinParts(".", "super", "powers");
const dependencyDirectory = joinParts("node_", "modules");
const buildDirectory = joinParts("di", "st");
const nativeDirectory = joinParts("npm/", "native");
const reviewName = joinParts("re", "view-notes.md");
const workLogName = joinParts("work", "-log.md");

function git(root, ...args) {
  const result = spawnSync("git", args, { cwd: root, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
}

function fixture(t, entries = {}) {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-hygiene-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  cpSync(checker, join(root, "check-hygiene.mjs"));
  git(root, "init", "--quiet");
  git(root, "config", "user.email", "test@example.invalid");
  git(root, "config", "user.name", "Hygiene Test");
  for (const [path, value] of Object.entries(entries)) {
    const target = join(root, path);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, value);
  }
  git(root, "add", "--all");
  return root;
}

function run(root, extraEnv = {}) {
  return spawnSync(process.execPath, [join(root, "check-hygiene.mjs")], {
    cwd: root,
    encoding: "utf8",
    env: { ...process.env, ...extraEnv },
  });
}

function assertRejected(entries, expectedPath, expectedRule, t) {
  const root = fixture(t, entries);
  const result = run(root);
  assert.notEqual(result.status, 0, `fixture unexpectedly passed: ${expectedPath}`);
  assert.match(`${result.stdout}${result.stderr}`, new RegExp(`${expectedPath}.*${expectedRule}`));
}

test("accepts bounded public tracked files", (t) => {
  const root = fixture(t, { "README.md": "Public project description.\n" });
  const result = run(root);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, "");
});

test("allows the public Codex target contract only in its exact generated surfaces", (t) => {
  const root = fixture(t, {
    "npm/generated/skills-rules.json": `{"fallback":"${codexMarker}"}\n`,
    "npm/scripts/extract-skills-rules.mjs": `const fallback = "${codexMarker}";\n`,
    "npm/test/skill-rules.test.mjs": `const expected = "${codexMarker}";\n`,
  });
  const result = run(root);
  assert.equal(result.status, 0, result.stderr);
});

test("rejects credential names, private markers, internal material, and package debris", (t) => {
  const cases = [
    [{ "local.env": "not a credential value\n" }, "local.env", "H001"],
    [{ "notes.txt": `path: ${machineHome}account\n` }, "notes.txt", "H002"],
    [{ "notes.txt": `config ${codexMarker}\n` }, "notes.txt", "H002"],
    [{ "notes.txt": `tool ${superpowersMarker}\n` }, "notes.txt", "H002"],
    [{ [`${codexMarker}/config`]: "private\n" }, `${codexMarker}/config`, "H002"],
    [{ [`${superpowersMarker}/state`]: "private\n" }, `${superpowersMarker}/state`, "H002"],
    [{ [reviewName]: "internal\n" }, reviewName, "H003"],
    [{ [workLogName]: "internal\n" }, workLogName, "H003"],
    [{ [`${dependencyDirectory}/package.json`]: "{}\n" }, `${dependencyDirectory}/package.json`, "H004"],
    [{ [`${buildDirectory}/output.js`]: "built\n" }, `${buildDirectory}/output.js`, "H004"],
    [{ [`${nativeDirectory}/linux-x64/pi-worker`]: "binary\n" }, `${nativeDirectory}/linux-x64/pi-worker`, "H004"],
    [{ "snapshot.tgz": "archive\n" }, "snapshot.tgz", "H005"],
    [{ "program.exe": "binary\n" }, "program.exe", "H006"],
  ];
  for (const [entries, path, rule] of cases) assertRejected(entries, path, rule, t);
});

test("reports only the relative path and stable rule ID", (t) => {
  const secretText = joinParts("do", "-not-report-this");
  const root = fixture(t, { "notes.txt": `${joinParts("cred", "ential=")}${secretText}\n` });
  const result = run(root);
  assert.notEqual(result.status, 0);
  const output = `${result.stdout}${result.stderr}`;
  assert.match(output, /notes\.txt.*H007/);
  assert.doesNotMatch(output, new RegExp(secretText));
  assert.doesNotMatch(output, /(?:\/Us|cod|super)/i);
});

test("fails closed for tracked symlinks and oversized regular files", (t) => {
  const symlinkRoot = fixture(t, { "target.txt": "safe\n" });
  symlinkSync(join(symlinkRoot, "target.txt"), join(symlinkRoot, "link.txt"));
  git(symlinkRoot, "add", "link.txt");
  const symlinkResult = run(symlinkRoot);
  assert.notEqual(symlinkResult.status, 0);
  assert.match(`${symlinkResult.stdout}${symlinkResult.stderr}`, /link\.txt.*H008/);

  const oversizedRoot = fixture(t, { "large.txt": Buffer.alloc(1024 * 1024 + 1, 65) });
  const oversizedResult = run(oversizedRoot);
  assert.notEqual(oversizedResult.status, 0);
  assert.match(`${oversizedResult.stdout}${oversizedResult.stderr}`, /large\.txt.*H009/);
});

test("fails closed for malformed inventories and git failures", (t) => {
  const root = fixture(t, { "safe.txt": "safe\n" });
  const fakeBin = join(root, "fake-bin");
  mkdirSync(fakeBin);
  const fakeGit = join(fakeBin, "git");
  writeFileSync(fakeGit, "#!/bin/sh\nif [ \"$1\" = \"rev-parse\" ]; then pwd; exit 0; fi\nprintf 'safe.txt\\0bad\\nname\\0'\n");
  chmodSync(fakeGit, 0o755);
  const malformed = run(root, { PATH: `${fakeBin}:${process.env.PATH}` });
  assert.notEqual(malformed.status, 0);
  assert.match(`${malformed.stdout}${malformed.stderr}`, /H010/);

  writeFileSync(fakeGit, "#!/bin/sh\nexit 1\n");
  const failedGit = run(root, { PATH: `${fakeBin}:${process.env.PATH}` });
  assert.notEqual(failedGit.status, 0);
  assert.match(`${failedGit.stdout}${failedGit.stderr}`, /H011/);
});

test("ignores Git environment redirection and verifies the repository root", (t) => {
  const root = fixture(t, { "notes.txt": `path: ${machineHome}account\n` });
  const alternateIndex = join(root, "alternate-index");
  const prepared = spawnSync("git", ["read-tree", "--empty"], {
    cwd: root,
    encoding: "utf8",
    env: { ...process.env, GIT_INDEX_FILE: alternateIndex },
  });
  assert.equal(prepared.status, 0, prepared.stderr);

  const redirected = run(root, {
    GIT_INDEX_FILE: alternateIndex,
    GIT_WORK_TREE: join(root, "elsewhere"),
  });
  assert.notEqual(redirected.status, 0);
  assert.match(`${redirected.stdout}${redirected.stderr}`, /notes\.txt.*H002/);
});

test("CI keeps read-only reproducible source and snapshot gates", () => {
  for (const job of ["go-linux", "go-macos", "windows-readiness", "node-package", "release-snapshot"]) {
    assert.match(ciWorkflow, new RegExp(`^  ${job}:$`, "m"), `CI includes ${job}`);
  }
  assert.match(ciWorkflow, /^permissions:\n  contents: read$/m);
  assert.match(ciWorkflow, /pull_request:/);
  assert.match(ciWorkflow, /push:/);
  assert.match(ciWorkflow, /go test -race -count=1 \.\/\.\.\./);
  assert.match(ciWorkflow, /GOOS=windows GOARCH=amd64 go build \.\/\.\.\./);
  assert.match(ciWorkflow, /GOOS=windows GOARCH=amd64 go test -c/);
  assert.match(ciWorkflow, /npm ci --ignore-scripts/);
  assert.match(ciWorkflow, /npm run verify/);
  assert.match(ciWorkflow, /go run \.\/tools\/release/);
  assert.match(ciWorkflow, /npm run stage -- --dist dist/);
  assert.match(ciWorkflow, /npm run check:hygiene/);
  assert.match(ciWorkflow, /PI_WORKER_ASSERT_STAGED=1 node --test --test-name-pattern='current checkout npm pack' npm\/test\/package\.test\.mjs/);
  assert.match(ciWorkflow, /node-package:[\s\S]*?actions\/setup-node@v7[\s\S]*?actions\/setup-go@v7[\s\S]*?npm run verify/);
  assert.deepEqual(
    [...ciWorkflow.matchAll(/uses:\s+(\S+)/g)].map(([, action]) => action).filter((action, index, all) => all.indexOf(action) === index).sort(),
    ["actions/checkout@v7", "actions/setup-go@v7", "actions/setup-node@v7"],
  );
  assert.doesNotMatch(ciWorkflow, /npm publish|NPM_TOKEN|id-token:\s*write|contents:\s*write|packages:\s*write|secrets\.|upload-artifact|create.?release/i);
});

test("release workflow is a non-publishing, verification-first snapshot policy", () => {
  assert.ok(releaseWorkflow, "missing .github/workflows/release.yml for snapshot policy assertions");
  const workflow = releaseWorkflow;
  const requiredActions = Object.freeze([
    "actions/checkout@v7",
    "actions/setup-go@v7",
    "actions/setup-node@v7",
    "actions/upload-artifact@v7",
  ]);

  assert.ok(workflow.includes("on:"));
  const onStart = workflow.indexOf("on:");
  const permissionsStart = workflow.indexOf("\npermissions:", onStart);
  assert.ok(onStart >= 0 && permissionsStart > onStart);
  const onSection = workflow.slice(onStart, permissionsStart);
  assert.match(onSection, /^\s*on:\n/m);
  assert.match(onSection, /^\s{2}workflow_dispatch:\s*$/m);
  const onTriggers = [...onSection.matchAll(/^\s{2}(?:"([^"]+)"|'([^']+)'|([A-Za-z_][\w-]*))\s*:/gm)]
    .map(([, doubleQuoted, singleQuoted, plain]) => doubleQuoted ?? singleQuoted ?? plain);
  const normalizedTriggers = [...new Set(onTriggers)];
  assert.deepEqual(normalizedTriggers, ["workflow_dispatch"], "release workflow has workflow_dispatch trigger only");

  assert.match(workflow, /^permissions:\n\s*contents:\s*read$/m);
  assert.doesNotMatch(workflow, /^\s*id-token:\s*write$/m);
  assert.doesNotMatch(workflow, /^\s*(?:contents|packages|issues|pull-requests|attestations):\s*write$/m);

  assert.match(workflow, /npm run verify/);
  assert.match(workflow, /go run \.\/tools\/release/);
  assert.match(workflow, /npm run stage -- --dist dist/);
  assert.match(workflow, /PI_WORKER_ASSERT_STAGED=1 node --test --test-name-pattern='current checkout npm pack' npm\/test\/package\.test\.mjs/);
  assert.match(workflow, /npm pack/);
  assert.match(workflow, /upload-artifact@v7/);

  const actionUsages = [...workflow.matchAll(/^\s*uses:\s+([^\s#]+)\s*$/gm)].map(([, action]) => action);
  assert.ok(actionUsages.length > 0, "release workflow references GitHub actions");
  for (const action of actionUsages) {
    assert.ok(action.endsWith("@v7"), `action pinning to v7 required: ${action}`);
  }
  const uniqueActions = [...new Set(actionUsages)].sort();
  assert.deepEqual(uniqueActions, [...requiredActions].sort(), "release workflow uses exact required actions");

  const uploadStart = workflow.indexOf("uses: actions/upload-artifact@v7");
  assert.ok(uploadStart >= 0, "release workflow uploads one snapshot artifact");
  const uploadBlock = workflow.slice(uploadStart);
  const pathMarker = "          path: |\n";
  const pathStart = uploadBlock.indexOf(pathMarker);
  assert.ok(pathStart >= 0, "upload action has a multiline path contract");
  const uploadedPaths = [];
  for (const line of uploadBlock.slice(pathStart + pathMarker.length).split("\n")) {
    if (!line.startsWith("            ")) break;
    uploadedPaths.push(line.slice(12));
  }
  assert.deepEqual(uploadedPaths, [
    "dist/pi-worker_v0.1.0_darwin_arm64.tar.gz",
    "dist/pi-worker_v0.1.0_darwin_amd64.tar.gz",
    "dist/pi-worker_v0.1.0_linux_arm64.tar.gz",
    "dist/pi-worker_v0.1.0_linux_amd64.tar.gz",
    "dist/checksums.txt",
    "dist/${{ steps.npm_pack.outputs.npm_tarball }}",
    "dist/npm-pack.json",
  ]);

  const shellBlocks = [...workflow.matchAll(/^ {8}run: \|\n((?:^ {10}.*(?:\n|$))+)/gm)];
  assert.ok(shellBlocks.length > 0, "release workflow has multiline shell blocks");
  for (const [, indented] of shellBlocks) {
    const script = indented.replace(/^ {10}/gm, "").replace(/\$\{\{[^}]+\}\}/g, "value");
    const syntax = spawnSync("bash", ["-n"], { input: script, encoding: "utf8" });
    assert.equal(syntax.status, 0, syntax.stderr);
  }

  assert.doesNotMatch(workflow, /^\s*push:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*pull_request:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*pull_request_target:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*schedule:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*workflow_run:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*repository_dispatch:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*release:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*workflow_call:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*tags:\s*$/m);
  assert.doesNotMatch(workflow, /^\s*workflow_dispatch:\s*release\s*$/m);

  assert.doesNotMatch(workflow, /\bnpm publish\b/);
  assert.doesNotMatch(workflow, /\bNPM_TOKEN\b/);
  assert.doesNotMatch(workflow, /\$\{\{\s*secrets\.[^}]+\}\}/);
  assert.doesNotMatch(workflow, /git\s+(?:push|tag)|gh\s+release\s+(?:create|upload|edit|delete)|npm\s+dist-tag|softprops\/action-gh-release|ncipollo\/release-action/i);
  assert.doesNotMatch(workflow, /rm\s+-rf|npm pack[^\n]*--ignore-scripts/);
});

test("release runbook stays reproducible and stops before publication", () => {
  assert.match(releaseRunbook, /npm run verify/);
  assert.match(releaseRunbook, /Go 1\.26\.1/);
  assert.match(releaseRunbook, /git show -s --format=%ct/);
  assert.match(releaseRunbook, /PI_WORKER_ASSERT_STAGED=1/);
  assert.match(releaseRunbook, /test "\$NPM_TARBALL" = "pi-worker-0\.1\.0\.tgz"/);
  for (const target of ["darwin_arm64", "darwin_amd64", "linux_arm64", "linux_amd64"]) {
    assert.match(releaseRunbook, new RegExp(`dist/pi-worker_v0\\.1\\.0_${target}\\.tar\\.gz`));
  }
  assert.match(releaseRunbook, /shasum -a 256 -c checksums\.txt/);
  assert.match(releaseRunbook, /sha256sum -c checksums\.txt/);
  assert.match(releaseRunbook, /p\.version!=='0\.1\.0'/);
  assert.match(releaseRunbook, /github\.com\/arasovic\/pi-worker/);
  assert.match(releaseRunbook, /single-use granular bootstrap[\s\S]*provenance[\s\S]*trusted publishing[\s\S]*revoke/i);
  assert.doesNotMatch(releaseRunbook, /npm publish|NPM_TOKEN|https?:\/\//i);
});
