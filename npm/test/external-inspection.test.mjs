import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { inspectExternalTargets } from "../lib/external-inspection.mjs";

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-external-inspection-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  return { root, home: join(root, "home") };
}

function rules() {
  return {
    schemaVersion: 3,
    skillsVersion: "1.5.22",
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
}

function writeSkill(root, marker, name = "pi-worker") {
  mkdirSync(root, { recursive: true });
  if (marker !== null) writeFileSync(join(root, "PI_WORKER_IDENTITY"), marker);
  writeFileSync(join(root, "SKILL.md"), `---\nname: ${name}\n---\nexternal\n`);
}

test("reports current, unknown, and markerless external identities atomically", async (t) => {
  const f = fixture(t);
  const current = join(f.home, ".agents", "skills", "pi-worker");
  const unknown = join(f.home, ".test", "skills", "pi-worker");
  const markerless = join(f.home, ".third", "skills", "pi-worker");
  writeSkill(current, "pi-worker-skill/v1\n");
  writeSkill(unknown, "pi-worker-skill/v99\n");
  writeSkill(markerless, null);

  const result = await inspectExternalTargets({
    home: f.home,
    cwd: f.root,
    env: { HOME: f.home },
    platform: process.platform,
    rules: rules(),
    resolveTargets: () => [join(f.home, ".test", "skills"), join(f.home, ".third", "skills")],
    excludePaths: [],
  });

  assert.equal(result.state, "performed");
  assert.deepEqual(result.targets, [
    { path: current, identity: "current" },
    { path: markerless, identity: "none" },
    { path: unknown, identity: "unknown" },
  ].sort((left, right) => left.path.localeCompare(right.path)));
});

test("omits missing and receipt-managed targets", async (t) => {
  const f = fixture(t);
  const managed = join(f.home, ".agents", "skills", "pi-worker");
  writeSkill(managed, "pi-worker-skill/v1\n");

  const result = await inspectExternalTargets({
    home: f.home,
    cwd: f.root,
    env: { HOME: f.home },
    platform: process.platform,
    rules: rules(),
    resolveTargets: () => [join(f.home, ".missing", "skills")],
    excludePaths: [managed],
  });

  assert.deepEqual(result, { state: "performed", targets: [] });
});

test("discards partial results when any target cannot be inspected", async (t) => {
  if (process.platform === "win32") t.skip("symlink permissions vary on windows");
  const f = fixture(t);
  const current = join(f.home, ".agents", "skills", "pi-worker");
  const dangling = join(f.home, ".test", "skills", "pi-worker");
  writeSkill(current, "pi-worker-skill/v1\n");
  mkdirSync(join(dangling, ".."), { recursive: true });
  symlinkSync(join(f.root, "missing-destination"), dangling);

  const result = await inspectExternalTargets({
    home: f.home,
    cwd: f.root,
    env: { HOME: f.home },
    platform: process.platform,
    rules: rules(),
    resolveTargets: () => [join(f.home, ".test", "skills")],
    excludePaths: [],
  });

  assert.deepEqual(result, { state: "unavailable", targets: [] });
});

test("recognizes a safe root symlink without adopting its destination", async (t) => {
  if (process.platform === "win32") t.skip("symlink permissions vary on windows");
  const f = fixture(t);
  const destination = join(f.root, "external-skill");
  const linked = join(f.home, ".test", "skills", "pi-worker");
  writeSkill(destination, "pi-worker-skill/v1\n");
  mkdirSync(join(linked, ".."), { recursive: true });
  symlinkSync(destination, linked);

  const result = await inspectExternalTargets({
    home: f.home,
    cwd: f.root,
    env: { HOME: f.home },
    platform: process.platform,
    rules: rules(),
    resolveTargets: () => [join(f.home, ".test", "skills")],
    excludePaths: [],
  });

  assert.deepEqual(result, {
    state: "performed",
    targets: [{ path: linked, identity: "current" }],
  });
});
