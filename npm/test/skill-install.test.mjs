import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { spawn as childProcessSpawn } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { installSkill } from "../lib/skill-install.mjs";
import { writeReceipt } from "../lib/skill-receipt.mjs";
import { PINNED_SKILLS_VERSION } from "../lib/skill-rules.mjs";

const realRulesPath = fileURLToPath(new URL("../generated/skills-rules.json", import.meta.url));
const packageRoot = fileURLToPath(new URL("../..", import.meta.url));
const SAFE_RETRY = "npm install -g --foreground-scripts pi-worker";

const rules = {
  schemaVersion: 3,
  skillsVersion: PINNED_SKILLS_VERSION,
  agentCount: 1,
  globalTargetCount: 1,
  noGlobalTargetCount: 0,
  agents: [{
    id: "test",
    usesUniversalTarget: false,
    rule: { kind: "home-relative", path: ".test/skills" },
    detector: { kind: "never" },
  }],
};

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-install-test-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const skill = join(root, "skill");
  mkdirSync(skill, { recursive: true });
  writeFileSync(join(skill, "SKILL.md"), "---\nname: pi-worker\n---\n");
  writeFileSync(join(skill, "PI_WORKER_IDENTITY"), "pi-worker-skill/v1\n");
  return { root, skill, home: join(root, "home"), receipt: join(root, "receipt", "skill-install.json") };
}

function childFor(action, { code = 0, signal = null, stdout = "", stderr = "" } = {}) {
  const calls = [];
  const spawn = (binary, args, options) => {
    calls.push({ binary, args, options });
    const child = new EventEmitter();
    child.stdout = new PassThrough();
    child.stderr = new PassThrough();
    queueMicrotask(() => {
      action?.();
      if (stdout) child.stdout.write(stdout);
      if (stderr) child.stderr.write(stderr);
      child.stdout.end();
      child.stderr.end();
      child.emit("close", code, signal);
    });
    return child;
  };
  return { spawn, calls };
}

function options(fixtureValue, child, extra = {}) {
  return {
    packageRoot: fixtureValue.root,
    bundledSkill: fixtureValue.skill,
    binary: "/native/pi-worker",
    home: fixtureValue.home,
    env: { HOME: fixtureValue.home, KEEP_ME: "present" },
    receiptPathFromNative: async () => fixtureValue.receipt,
    loadRules: () => rules,
    resolveAllTargets: () => [join(fixtureValue.home, ".test", "skills")],
    spawn: child.spawn,
    ...extra,
  };
}

test("installs an absent skill with the exact package-local CLI invocation", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  });

  const result = await installSkill(options(f, child));

  assert.equal(result.outcome, "installed");
  assert.equal(result.targetCount, 2);
  assert.equal(child.calls.length, 1);
  assert.match(child.calls[0].args[0], /node_modules\/skills\/bin\/cli\.mjs$/);
  assert.deepEqual(child.calls[0].args.slice(1), [
    "add",
    f.skill,
    "--skill",
    "pi-worker",
    "--global",
    "--yes",
    "--agent",
    "universal",
  ]);
  assert.equal(child.calls[0].options.shell, false);
  assert.equal(child.calls[0].binary, process.execPath);
  if (process.platform !== "win32") assert.equal(child.calls[0].options.detached, true);
  assert.deepEqual(child.calls[0].options.env, {
    HOME: f.home,
    KEEP_ME: "present",
    DO_NOT_TRACK: "1",
  });
  assert.deepEqual(JSON.parse(readFileSync(f.receipt, "utf8")).outcome, "installed");
});

test("installs through the actual pinned skills CLI for a detected universal agent target", async (t) => {
  const f = fixture(t);
  mkdirSync(join(f.home, ".config", "amp"), { recursive: true });

  const result = await installSkill({
    packageRoot,
    binary: "/native/pi-worker",
    home: f.home,
    cwd: f.root,
    env: { HOME: f.home, PATH: process.env.PATH ?? "" },
    receiptPathFromNative: async () => f.receipt,
  });

  assert.equal(result.outcome, "installed");
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(receipt.outcome, "installed");
  assert.deepEqual(receipt.targets.map(({ path, kind }) => ({ path, kind })), [
    { path: join(f.home, ".agents", "skills", "pi-worker"), kind: "canonical" },
  ]);
});

test("passes only detected global-capable agent ids in generated order", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const firstCopy = join(f.home, ".first", "skills", "pi-worker");
  const secondCopy = join(f.home, ".second", "skills", "pi-worker");
  const detectedRules = {
    schemaVersion: 3,
    skillsVersion: PINNED_SKILLS_VERSION,
    agentCount: 3,
    globalTargetCount: 2,
    noGlobalTargetCount: 1,
    agents: [
      {
        id: "first",
        usesUniversalTarget: false,
        rule: { kind: "home-relative", path: ".first/skills" },
        detector: { kind: "any-existing", paths: [{ kind: "home-relative", path: ".first" }] },
      },
      {
        id: "promptscript",
        usesUniversalTarget: false,
        rule: { kind: "no-global-target" },
        detector: { kind: "any-existing", paths: [{ kind: "cwd-relative", path: ".promptscript" }] },
      },
      {
        id: "second",
        usesUniversalTarget: false,
        rule: { kind: "home-relative", path: ".second/skills" },
        detector: { kind: "any-existing", paths: [{ kind: "home-relative", path: ".second" }] },
      },
    ],
  };
  mkdirSync(join(f.home, ".first"), { recursive: true });
  mkdirSync(join(f.home, ".second"), { recursive: true });
  mkdirSync(join(f.root, ".promptscript"), { recursive: true });
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(firstCopy, ".."), { recursive: true });
    cpSync(f.skill, firstCopy, { recursive: true });
    mkdirSync(join(secondCopy, ".."), { recursive: true });
    cpSync(f.skill, secondCopy, { recursive: true });
  });

  const result = await installSkill(options(f, child, {
    cwd: f.root,
    loadRules: () => detectedRules,
    resolveAllTargets: () => [join(f.home, ".first", "skills"), join(f.home, ".second", "skills")],
  }));

  assert.equal(result.outcome, "installed");
  assert.deepEqual(child.calls[0].args.slice(-3), ["--agent", "first", "second"]);
  const receiptTargets = JSON.parse(readFileSync(f.receipt, "utf8")).targets.map(({ path }) => path);
  assert.ok(receiptTargets.includes(firstCopy));
  assert.ok(receiptTargets.includes(secondCopy));
  assert.equal(child.calls[0].args.includes("promptscript"), false);
  assert.equal(child.calls[0].args.includes("*"), false);
  assert.equal(child.calls[0].args.includes("--all"), false);
});

test("fails when a detected global-capable agent target is absent despite a zero child exit", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const detectedRules = {
    schemaVersion: 3,
    skillsVersion: PINNED_SKILLS_VERSION,
    agentCount: 1,
    globalTargetCount: 1,
    noGlobalTargetCount: 0,
    agents: [{
      id: "detected",
      usesUniversalTarget: false,
      rule: { kind: "home-relative", path: ".detected/skills" },
      detector: { kind: "any-existing", paths: [{ kind: "home-relative", path: ".detected" }] },
    }],
  };
  mkdirSync(join(f.home, ".detected"), { recursive: true });
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
  });

  const result = await installSkill(options(f, child, {
    loadRules: () => detectedRules,
    resolveAllTargets: () => [join(f.home, ".detected", "skills")],
  }));

  assert.equal(result.outcome, "failed");
  assert.deepEqual(child.calls[0].args.slice(-2), ["--agent", "detected"]);
  assert.equal(JSON.parse(readFileSync(f.receipt, "utf8")).outcome, "failed");
});

test("does not spawn when a detected target is absent from the conservative preflight inventory", async (t) => {
  const f = fixture(t);
  const detectedRules = {
    schemaVersion: 3,
    skillsVersion: PINNED_SKILLS_VERSION,
    agentCount: 1,
    globalTargetCount: 1,
    noGlobalTargetCount: 0,
    agents: [{
      id: "detected",
      usesUniversalTarget: false,
      rule: { kind: "home-relative", path: ".detected/skills" },
      detector: { kind: "any-existing", paths: [{ kind: "home-relative", path: ".detected" }] },
    }],
  };
  mkdirSync(join(f.home, ".detected"), { recursive: true });
  const child = childFor();

  const result = await installSkill(options(f, child, {
    loadRules: () => detectedRules,
    resolveAllTargets: () => [],
  }));

  assert.equal(result.outcome, "failed");
  assert.equal(child.calls.length, 0);
  assert.equal(JSON.parse(readFileSync(f.receipt, "utf8")).outcome, "failed");
});

test("falls back to universal when only no-global agents are detected", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const noGlobalRules = {
    schemaVersion: 3,
    skillsVersion: PINNED_SKILLS_VERSION,
    agentCount: 1,
    globalTargetCount: 0,
    noGlobalTargetCount: 1,
    agents: [{
      id: "promptscript",
      usesUniversalTarget: false,
      rule: { kind: "no-global-target" },
      detector: { kind: "any-existing", paths: [{ kind: "cwd-relative", path: ".promptscript" }] },
    }],
  };
  mkdirSync(join(f.root, ".promptscript"), { recursive: true });
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
  });

  const result = await installSkill(options(f, child, {
    cwd: f.root,
    loadRules: () => noGlobalRules,
    resolveAllTargets: () => [],
  }));

  assert.equal(result.outcome, "installed");
  assert.deepEqual(child.calls[0].args.slice(-2), ["--agent", "universal"]);
});

test("reinstalls owned targets and uses the prior receipt as ownership evidence", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const first = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  });
  assert.equal((await installSkill(options(f, first))).outcome, "installed");

  const second = childFor();
  assert.equal((await installSkill(options(f, second))).outcome, "installed");
  assert.equal(second.calls.length, 1);
});

test("records canonical, copy, and root-symlink receipt topology", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const linked = join(f.home, ".linked", "skills", "pi-worker");
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(linked, ".."), { recursive: true });
    symlinkSync("../../.agents/skills/pi-worker", linked);
  });

  assert.equal((await installSkill(options(f, child, {
    resolveAllTargets: () => [join(f.home, ".linked", "skills")],
  }))).outcome, "installed");
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.deepEqual(receipt.targets.map(({ path, kind }) => ({ path, kind })), [
    { path: canonical, kind: "canonical" },
    { path: join(f.home, ".linked", "skills"), kind: "symlink" },
  ]);
  assert.deepEqual(receipt.targets[1].files.map(({ path }) => path), ["pi-worker"]);
});

test("blocks a markerless same-name tree without spawning or changing it", async (t) => {
  const f = fixture(t);
  const conflict = join(f.home, ".agents", "skills", "pi-worker");
  mkdirSync(conflict, { recursive: true });
  writeFileSync(join(conflict, "SKILL.md"), "---\nname: pi-worker\n---\nforeign\n");
  const before = readFileSync(join(conflict, "SKILL.md"));
  const child = childFor();

  const result = await installSkill(options(f, child));

  assert.equal(result.outcome, "blocked");
  assert.equal(child.calls.length, 0);
  assert.deepEqual(readFileSync(join(conflict, "SKILL.md")), before);
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(receipt.outcome, "blocked");
  assert.equal(receipt.affectedTargets[0].path, conflict);
  assert.equal(receipt.recovery.length, 0);
});

test("preserves a recognized external skill without spawning or persisting ownership", async (t) => {
  const f = fixture(t);
  const external = join(f.home, ".agents", "skills", "pi-worker");
  mkdirSync(external, { recursive: true });
  writeFileSync(join(external, "PI_WORKER_IDENTITY"), "pi-worker-skill/v1\n");
  writeFileSync(join(external, "SKILL.md"), "---\nname: pi-worker\n---\nexternal contract\n");
  const before = readFileSync(join(external, "SKILL.md"));
  const child = childFor();

  const result = await installSkill(options(f, child));

  assert.equal(result.outcome, "skipped");
  assert.equal(child.calls.length, 0);
  assert.deepEqual(readFileSync(join(external, "SKILL.md")), before);
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(receipt.outcome, "skipped");
  assert.deepEqual(receipt.targets, []);
  assert.deepEqual(receipt.affectedTargets, []);
});

test("blocks an unknown identity marker as possibly newer content without overwriting it", async (t) => {
  const f = fixture(t);
  const conflict = join(f.home, ".agents", "skills", "pi-worker");
  mkdirSync(conflict, { recursive: true });
  writeFileSync(join(conflict, "PI_WORKER_IDENTITY"), "pi-worker-skill/v99\n");
  writeFileSync(join(conflict, "SKILL.md"), "---\nname: pi-worker\n---\npossibly newer\n");
  const before = readFileSync(join(conflict, "SKILL.md"));
  const child = childFor();

  const result = await installSkill(options(f, child));

  assert.equal(result.outcome, "blocked");
  assert.equal(child.calls.length, 0);
  assert.deepEqual(readFileSync(join(conflict, "SKILL.md")), before);
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(receipt.affectedTargets[0].state, "conflicting");
  assert.deepEqual(receipt.recovery, []);
});

test("ignores verified PromptScript-only aggregate failure prose", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  }, { stdout: "Failed to install 1\n  ✗ pi-worker → PromptScript: no global target\n" });

  assert.equal((await installSkill(options(f, child))).outcome, "installed");
});

test("does not hide a detected-agent failure when only canonical postconditions exist", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
  }, { stdout: "Failed to install 1\n  ✗ pi-worker → Claude Code: permission denied\n" });

  assert.equal((await installSkill(options(f, child))).outcome, "failed");
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(receipt.outcome, "failed");
  assert.ok(Array.isArray(receipt.targets));
  assert.ok(Array.isArray(receipt.affectedTargets));
  assert.ok(Array.isArray(receipt.recovery));
  assert.deepEqual(receipt.targets.map(({ path, kind }) => ({ path, kind })), [
    { path: canonical, kind: "canonical" },
  ]);
  assert.deepEqual(receipt.affectedTargets, []);
  assert.deepEqual(receipt.recovery, [SAFE_RETRY]);
  assert.doesNotMatch(JSON.stringify(receipt), /Claude Code|permission denied|Failed to install/);
});

test("persists verified partial targets when the child exits unsuccessfully", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  }, { code: 9, stderr: `upstream ${["sec", "ret=credential-value"].join("")}\n` });

  const result = await installSkill(options(f, child));

  assert.equal(result.outcome, "failed");
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(receipt.outcome, "failed");
  assert.ok(Array.isArray(receipt.targets));
  assert.ok(Array.isArray(receipt.affectedTargets));
  assert.ok(Array.isArray(receipt.recovery));
  assert.deepEqual(receipt.targets.map(({ path, kind }) => ({ path, kind })), [
    { path: canonical, kind: "canonical" },
    { path: copy, kind: "copy" },
  ]);
  for (const target of receipt.targets) {
    assert.deepEqual(Object.keys(target).sort(), ["files", "kind", "path"]);
    for (const file of target.files) assert.deepEqual(Object.keys(file).sort(), ["path", "sha256"]);
  }
  assert.deepEqual(receipt.affectedTargets, []);
  assert.deepEqual(receipt.recovery, [SAFE_RETRY]);
  assert.doesNotMatch(JSON.stringify(receipt), /upstream|secret|credential-value|KEEP_ME|HOME/);
});

test("fails softly after a nonzero child exit and never persists child prose", async (t) => {
  const f = fixture(t);
  const child = childFor(undefined, { code: 9, stderr: `Failed to install 1\n${["cred", "ential=secret"].join("")}\n` });

  const result = await installSkill(options(f, child));

  assert.equal(result.outcome, "failed");
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(receipt.outcome, "failed");
  assert.ok(Array.isArray(receipt.targets));
  assert.ok(Array.isArray(receipt.affectedTargets));
  assert.ok(Array.isArray(receipt.recovery));
  assert.deepEqual(receipt.targets, []);
  assert.deepEqual(receipt.affectedTargets, []);
  assert.deepEqual(receipt.recovery, [SAFE_RETRY]);
  assert.doesNotMatch(JSON.stringify(receipt), /Failed|credential|secret/);
});

test("records an incomplete canonical tree as affected and blocks its recovery", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const missing = join(f.skill, "references", "guide.md");
  mkdirSync(join(missing, ".."), { recursive: true });
  writeFileSync(missing, "guide\n");
  const child = childFor(() => {
    mkdirSync(canonical, { recursive: true });
    for (const name of ["SKILL.md", "PI_WORKER_IDENTITY"]) {
      cpSync(join(f.skill, name), join(canonical, name));
    }
  }, { code: null, signal: "SIGTERM" });

  const result = await installSkill(options(f, child));

  assert.equal(result.outcome, "failed");
  const failed = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.deepEqual(failed.targets, []);
  assert.deepEqual(failed.affectedTargets.map(({ path, state }) => ({ path, state })), [
    { path: canonical, state: "conflicting" },
  ]);
  assert.deepEqual(failed.recovery, [SAFE_RETRY]);
  assert.equal(readFileSync(join(canonical, "SKILL.md"), "utf8"), readFileSync(join(f.skill, "SKILL.md"), "utf8"));

  const nextChild = childFor();
  const next = await installSkill(options(f, nextChild));
  assert.equal(next.outcome, "blocked", JSON.stringify(next));
  assert.equal(nextChild.calls.length, 0);
  assert.equal(next.affectedTargets.find(({ path }) => path === canonical)?.state, "unmanaged");
  assert.deepEqual(JSON.parse(readFileSync(f.receipt, "utf8")).recovery, [
    `npx --yes skills@${PINNED_SKILLS_VERSION} remove pi-worker -g -y`,
    SAFE_RETRY,
  ]);
  assert.throws(() => readFileSync(join(canonical, "references", "guide.md")), /ENOENT/);
});

test("does not spawn when the durable receipt guard fails", async (t) => {
  const f = fixture(t);
  const child = childFor();
  let writes = 0;

  const result = await installSkill(options(f, child, {
    writeReceipt: async () => {
      writes += 1;
      throw new Error("injected guard failure");
    },
  }));

  assert.equal(result.outcome, "skipped");
  assert.equal(writes, 1);
  assert.equal(child.calls.length, 0);
});

test("soft-fails spawn errors, signal exits, and unverifiable postconditions", async (t) => {
  for (const [name, child] of [
    ["spawn error", { spawn: () => { throw new Error("spawn detail"); } }],
    ["signal exit", childFor(undefined, { code: null, signal: "SIGTERM" })],
    ["missing postconditions", childFor()],
  ]) {
    await t.test(name, async (t) => {
      const f = fixture(t);
      const result = await installSkill(options(f, child));
      assert.equal(result.outcome, "failed");
      const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
      assert.equal(receipt.outcome, "failed");
      assert.doesNotMatch(JSON.stringify(receipt), /spawn detail|SIGTERM|postcondition/i);
    });
  }
});

test("records receipt-tracked drifted Pi Worker targets with exact global recovery", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const first = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  });
  assert.equal((await installSkill(options(f, first))).outcome, "installed");
  writeFileSync(join(canonical, "SKILL.md"), "---\nname: pi-worker\n---\ndrifted\n");

  const drifted = await installSkill(options(f, childFor()));
  assert.equal(drifted.outcome, "blocked");
  const driftedReceipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(driftedReceipt.affectedTargets.find(({ path }) => path === canonical).state, "drifted");
  assert.deepEqual(driftedReceipt.recovery, [
    `npx --yes skills@${PINNED_SKILLS_VERSION} remove pi-worker -g -y`,
    "npm install -g --foreground-scripts pi-worker",
  ]);
});

test("a mixed drifted and conflicting set suppresses global removal", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const first = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  });
  assert.equal((await installSkill(options(f, first))).outcome, "installed");
  writeFileSync(join(canonical, "SKILL.md"), "---\nname: pi-worker\n---\ndrifted\n");
  writeFileSync(join(copy, "foreign.txt"), "foreign\n");

  const result = await installSkill(options(f, childFor()));
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(result.outcome, "blocked");
  assert.deepEqual(new Set(receipt.affectedTargets.map(({ state }) => state)), new Set(["drifted", "conflicting"]));
  assert.deepEqual(receipt.recovery, []);
});

test("preserves an owned dormant copy from the prior version", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const installBoth = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  });
  assert.equal((await installSkill(options(f, installBoth))).outcome, "installed");
  const oldCopy = readFileSync(join(copy, "SKILL.md"));
  writeFileSync(join(f.skill, "SKILL.md"), "---\nname: pi-worker\n---\nnew version\n");

  const updateCanonicalOnly = childFor(() => {
    rmSync(canonical, { recursive: true, force: true });
    cpSync(f.skill, canonical, { recursive: true });
  });
  const result = await installSkill(options(f, updateCanonicalOnly));

  assert.equal(result.outcome, "installed");
  assert.deepEqual(readFileSync(join(copy, "SKILL.md")), oldCopy);
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.ok(receipt.targets.find(({ path, kind }) => path === copy && kind === "copy")?.files.length > 0);
});

test("fails when a previously owned target disappears", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const first = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  });
  assert.equal((await installSkill(options(f, first))).outcome, "installed");

  const removeOwned = childFor(() => rmSync(copy, { recursive: true, force: true }));
  assert.equal((await installSkill(options(f, removeOwned))).outcome, "failed");
  assert.equal(JSON.parse(readFileSync(f.receipt, "utf8")).outcome, "failed");
});

test("blocks when target state changes during the final preflight", async (t) => {
  const f = fixture(t);
  const child = childFor();
  let calls = 0;
  const result = await installSkill(options(f, child, {
    classifyTarget: async () => {
      calls += 1;
      return calls <= 2 ? "absent" : calls === 3 ? "conflicting" : "absent";
    },
  }));

  assert.equal(result.outcome, "blocked");
  assert.equal(child.calls.length, 0);
  assert.equal(JSON.parse(readFileSync(f.receipt, "utf8")).affectedTargets[0].state, "conflicting");
});

test("persists failed when package-local CLI resolution fails after the guard", async (t) => {
  const f = fixture(t);
  const child = childFor();
  const result = await installSkill(options(f, child, {
    resolveCLI: () => { throw new Error("missing pinned CLI"); },
  }));

  assert.equal(result.outcome, "failed");
  assert.equal(child.calls.length, 0);
  assert.equal(JSON.parse(readFileSync(f.receipt, "utf8")).outcome, "failed");
});

test("retries a one-shot blocked receipt failure as a failed receipt", async (t) => {
  const f = fixture(t);
  const conflict = join(f.home, ".agents", "skills", "pi-worker");
  mkdirSync(conflict, { recursive: true });
  writeFileSync(join(conflict, "foreign.txt"), "foreign\n");
  let writes = 0;
  const result = await installSkill(options(f, childFor(), {
    writeReceipt: async (...args) => {
      writes += 1;
      if (writes === 2) throw new Error("one-shot blocked write failure");
      return writeReceipt(...args);
    },
  }));

  assert.equal(result.outcome, "failed");
  assert.equal(writes, 3);
  assert.equal(JSON.parse(readFileSync(f.receipt, "utf8")).outcome, "failed");
});

test("a symlinked prior receipt never confers ownership on a recognized external skill", async (t) => {
  if (process.platform === "win32") t.skip("symlink permissions vary on windows");
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const first = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  });
  assert.equal((await installSkill(options(f, first))).outcome, "installed");
  const realReceipt = join(f.root, "prior.json");
  cpSync(f.receipt, realReceipt);
  rmSync(f.receipt);
  symlinkSync(realReceipt, f.receipt);
  const child = childFor();

  const result = await installSkill(options(f, child));
  assert.equal(result.outcome, "skipped");
  assert.equal(child.calls.length, 0);
  const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
  assert.equal(receipt.outcome, "skipped");
  assert.deepEqual(receipt.targets, []);
});

test("malformed and oversized prior receipts never confer ownership on recognized external skills", async (t) => {
  for (const [name, prior] of [
    ["malformed", "{not-json"],
    ["oversized", "x".repeat(1024 * 1024 + 1)],
  ]) {
    await t.test(name, async (t) => {
      const f = fixture(t);
      const canonical = join(f.home, ".agents", "skills", "pi-worker");
      mkdirSync(join(canonical, ".."), { recursive: true });
      cpSync(f.skill, canonical, { recursive: true });
      mkdirSync(join(f.receipt, ".."), { recursive: true });
      writeFileSync(f.receipt, prior);
      const child = childFor();

      const result = await installSkill(options(f, child));
      assert.equal(result.outcome, "skipped");
      assert.equal(child.calls.length, 0);
      const receipt = JSON.parse(readFileSync(f.receipt, "utf8"));
      assert.equal(receipt.outcome, "skipped");
      assert.deepEqual(receipt.targets, []);
      assert.deepEqual(receipt.affectedTargets, []);
    });
  }
});

test("foreign agent copy and symlink targets block without mutation", async (t) => {
  for (const topology of ["copy", "symlink"]) {
    await t.test(topology, async (t) => {
      if (topology === "symlink" && process.platform === "win32") {
        t.skip("symlink permissions vary on windows");
      }
      const f = fixture(t);
      const agentTarget = join(f.home, ".test", "skills", "pi-worker");
      mkdirSync(join(agentTarget, ".."), { recursive: true });
      if (topology === "copy") {
        mkdirSync(agentTarget);
        writeFileSync(join(agentTarget, "foreign.txt"), "foreign\n");
      } else {
        const foreign = join(f.root, "foreign-skill");
        mkdirSync(foreign);
        writeFileSync(join(foreign, "foreign.txt"), "foreign\n");
        symlinkSync(foreign, agentTarget);
      }
      const child = childFor();
      const result = await installSkill(options(f, child));

      assert.equal(result.outcome, "blocked");
      assert.equal(child.calls.length, 0);
      assert.ok(result.affectedTargets.some(({ path, state }) => (
        path === agentTarget && state === "conflicting"
      )));
    });
  }
});

test("deduplicates conservative targets before preflight and receipt construction", async (t) => {
  const f = fixture(t);
  const canonical = join(f.home, ".agents", "skills", "pi-worker");
  const copy = join(f.home, ".test", "skills", "pi-worker");
  const child = childFor(() => {
    mkdirSync(join(canonical, ".."), { recursive: true });
    cpSync(f.skill, canonical, { recursive: true });
    mkdirSync(join(copy, ".."), { recursive: true });
    cpSync(f.skill, copy, { recursive: true });
  });
  const result = await installSkill(options(f, child, {
    resolveAllTargets: () => [join(f.home, ".test", "skills"), join(f.home, ".test", "skills")],
  }));

  assert.equal(result.outcome, "installed");
  const targets = JSON.parse(readFileSync(f.receipt, "utf8")).targets;
  assert.equal(targets.filter(({ path }) => path === copy).length, 1);
  assert.equal(targets.filter(({ path }) => path === canonical).length, 1);
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  test(`forwards ${signal} to the detached installer and keeps partial ownership recoverable`, { timeout: 10_000 }, async (t) => {
    if (process.platform === "win32") t.skip("process-group signals are not portable on windows");

    const f = fixture(t);
    const canonical = join(f.home, ".agents", "skills", "pi-worker");
    const stopped = join(f.root, `${signal.toLowerCase()}-stopped`);
    const childPidFile = join(f.root, `${signal.toLowerCase()}-child.pid`);
    const resultFile = join(f.root, `${signal.toLowerCase()}-result`);
    const cli = join(f.root, `${signal.toLowerCase()}-installer.mjs`);
    const harness = join(f.root, `${signal.toLowerCase()}-harness.mjs`);
    const installModule = fileURLToPath(new URL("../lib/skill-install.mjs", import.meta.url));
    writeFileSync(cli, `
import { cpSync, mkdirSync, writeFileSync } from "node:fs";
import process from "node:process";

process.on("SIGINT", () => {
  writeFileSync(${JSON.stringify(stopped)}, "SIGINT\\n");
  process.exit(130);
});
process.on("SIGTERM", () => {
  writeFileSync(${JSON.stringify(stopped)}, "SIGTERM\\n");
  process.exit(143);
});
mkdirSync(${JSON.stringify(join(canonical, ".."))}, { recursive: true });
cpSync(${JSON.stringify(f.skill)}, ${JSON.stringify(canonical)}, { recursive: true });
process.stdout.write("READY\\n");
setInterval(() => {}, 1_000_000);
`);
    writeFileSync(harness, `
import { writeFileSync } from "node:fs";
import { spawn as realSpawn } from "node:child_process";
import process from "node:process";
import { installSkill } from ${JSON.stringify(installModule)};

// Keep the harness alive so this test isolates the installer's signal forwarding.
process.on("SIGINT", () => {});
process.on("SIGTERM", () => {});

const childSpawn = (...args) => {
  const child = realSpawn(...args);
  writeFileSync(${JSON.stringify(childPidFile)}, String(child.pid));
  child.stdout?.on("data", (chunk) => process.stdout.write(chunk));
  child.stderr?.on("data", (chunk) => process.stderr.write(chunk));
  return child;
};
const home = ${JSON.stringify(f.home)};
const result = await installSkill({
  packageRoot: ${JSON.stringify(f.root)},
  bundledSkill: ${JSON.stringify(f.skill)},
  binary: "/native/pi-worker",
  home,
  env: { HOME: home },
  receiptPathFromNative: async () => ${JSON.stringify(f.receipt)},
  loadRules: () => (${JSON.stringify(rules)}),
  resolveAllTargets: () => [${JSON.stringify(join(f.home, ".test", "skills"))}],
  spawn: childSpawn,
  cli: ${JSON.stringify(cli)},
  timeoutMs: 10_000,
});
writeFileSync(${JSON.stringify(resultFile)}, result.outcome);
process.stdout.write("RESULT:" + result.outcome + "\\n");
`);

    const runner = childProcessSpawn(process.execPath, [harness], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    const waitForRunnerExit = () => {
      if (runner.exitCode !== null || runner.signalCode !== null) return Promise.resolve();
      return new Promise((resolve) => runner.once("close", resolve));
    };
    const waitForChildExit = async (pid) => {
      const deadline = Date.now() + 2_000;
      while (true) {
        try {
          process.kill(pid, 0);
        } catch (error) {
          if (error?.code === "ESRCH") return;
          throw error;
        }
        if (Date.now() >= deadline) throw new Error(`child ${pid} did not terminate`);
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    };
    runner.stdout.setEncoding("utf8");
    runner.stderr.setEncoding("utf8");
    runner.stdout.on("data", (chunk) => { output += chunk; });
    runner.stderr.on("data", (chunk) => { output += chunk; });
    t.after(async () => {
      let childPid;
      try {
        childPid = Number(readFileSync(childPidFile, "utf8"));
      } catch {
        childPid = undefined;
      }
      if (Number.isInteger(childPid) && childPid > 0 && childPid !== process.pid) {
        try { process.kill(-childPid, "SIGKILL"); } catch { /* already stopped */ }
      }
      if (runner.exitCode === null && runner.signalCode === null) runner.kill("SIGKILL");
      await waitForRunnerExit();
      if (Number.isInteger(childPid) && childPid > 0 && childPid !== process.pid) {
        await waitForChildExit(childPid);
      }
    });

    await new Promise((resolve, reject) => {
      const onData = () => {
        if (output.includes("READY\n")) {
          runner.stdout.off("data", onData);
          resolve();
        }
      };
      runner.stdout.on("data", onData);
      runner.once("error", reject);
      runner.once("close", (closedCode, closedSignal) => reject(new Error(`installer exited before READY: ${closedCode}/${closedSignal}; ${output}`)));
      if (output.includes("READY\n")) onData();
    });
    const childPid = Number(readFileSync(childPidFile, "utf8"));
    assert.ok(Number.isInteger(childPid) && childPid > 0);
    runner.kill(signal);
    const [code, exitSignal] = await new Promise((resolve, reject) => {
      runner.once("error", reject);
      runner.once("exit", (closedCode, closedSignal) => resolve([closedCode, closedSignal]));
    });

    assert.equal(exitSignal, null, `installer harness was not allowed to finish: ${output}`);
    assert.equal(code, 0, output);
    assert.equal(readFileSync(stopped, "utf8"), `${signal}\n`);
    assert.equal(readFileSync(resultFile, "utf8"), "failed");
    assert.throws(() => process.kill(childPid, 0), /ESRCH/);
    const interruptedReceipt = JSON.parse(readFileSync(f.receipt, "utf8"));
    assert.equal(interruptedReceipt.outcome, "failed");
    assert.ok(interruptedReceipt.targets.some(({ path }) => path === canonical));
    assert.deepEqual(interruptedReceipt.recovery, [SAFE_RETRY]);

    const nextChild = childFor();
    const next = await installSkill(options(f, nextChild));
    assert.equal(next.outcome, "blocked", JSON.stringify(next));
    assert.equal(next.affectedTargets.find(({ path }) => path === canonical)?.state, "unmanaged");
    assert.deepEqual(JSON.parse(readFileSync(f.receipt, "utf8")).recovery, [
      `npx --yes skills@${PINNED_SKILLS_VERSION} remove pi-worker -g -y`,
      SAFE_RETRY,
    ]);
    assert.equal(nextChild.calls.length, 0);
  });
}

for (const streamName of ["stdout", "stderr"]) {
  test(`bounds ${streamName}, escalates once, and waits for child close`, async (t) => {
    const f = fixture(t);
    const signals = [];
    const spawn = () => {
      const child = new EventEmitter();
      child.stdout = new PassThrough();
      child.stderr = new PassThrough();
      child.kill = (signal) => {
        signals.push(signal);
        if (signal === "SIGKILL") queueMicrotask(() => child.emit("close", null, "SIGKILL"));
        return true;
      };
      queueMicrotask(() => child[streamName].write(Buffer.alloc(64 * 1024 + 1)));
      return child;
    };

    const result = await installSkill(options(f, { spawn }, { timeoutMs: 2_000 }));
    assert.equal(result.outcome, "failed");
    assert.deepEqual(signals, ["SIGTERM", "SIGKILL"]);
    assert.equal(JSON.parse(readFileSync(f.receipt, "utf8")).outcome, "failed");
  });
}

test("times out, escalates, and waits for child close", async (t) => {
  const f = fixture(t);
  const signals = [];
  const spawn = () => {
    const child = new EventEmitter();
    child.stdout = new PassThrough();
    child.stderr = new PassThrough();
    child.kill = (signal) => {
      signals.push(signal);
      if (signal === "SIGKILL") queueMicrotask(() => child.emit("close", null, "SIGKILL"));
      return true;
    };
    return child;
  };

  const result = await installSkill(options(f, { spawn }, { timeoutMs: 5 }));
  assert.equal(result.outcome, "failed");
  assert.deepEqual(signals, ["SIGTERM", "SIGKILL"]);
});

test("preflights CLAUDE_CONFIG_DIR and OpenClaw first-existing targets", async (t) => {
  const f = fixture(t);
  const claudeRoot = join(f.root, "claude-home");
  const claudeConflict = join(claudeRoot, "skills", "pi-worker");
  mkdirSync(claudeConflict, { recursive: true });
  writeFileSync(join(claudeConflict, "foreign.txt"), "foreign\n");
  const child = childFor();
  const base = options(f, child, {
    env: { HOME: f.home, CLAUDE_CONFIG_DIR: claudeRoot },
    rulesPath: realRulesPath,
  });
  delete base.loadRules;
  delete base.resolveAllTargets;
  const claude = await installSkill(base);
  assert.equal(claude.outcome, "blocked");
  assert.ok(claude.affectedTargets.some(({ path }) => path === claudeConflict));
  assert.equal(child.calls.length, 0);

  rmSync(claudeConflict, { recursive: true, force: true });
  rmSync(f.receipt, { force: true });
  const clawRoot = join(f.home, ".clawdbot");
  const clawConflict = join(clawRoot, "skills", "pi-worker");
  mkdirSync(clawConflict, { recursive: true });
  writeFileSync(join(clawConflict, "foreign.txt"), "foreign\n");
  const openClaw = await installSkill({ ...base, env: { HOME: f.home } });
  assert.equal(openClaw.outcome, "blocked");
  assert.ok(openClaw.affectedTargets.some(({ path }) => path === clawConflict));
});
