import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import process from "node:process";
import { describe, test } from "node:test";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  extractRulesFromSource,
  resolvePinnedSource,
} from "../scripts/extract-skills-rules.mjs";
import {
  detectInstalledAgents,
  evaluateDetector,
  EXPECTED_AGENT_COUNT,
  EXPECTED_GLOBAL_TARGET_COUNT,
  EXPECTED_NO_GLOBAL_TARGET_COUNT,
  loadRules,
  PINNED_SKILLS_VERSION,
  resolveAllTargets,
  resolveRule,
} from "../lib/skill-rules.mjs";

const npmRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const generatedPath = join(npmRoot, "generated", "skills-rules.json");
const extractorPath = join(npmRoot, "scripts", "extract-skills-rules.mjs");

function sourceFixture() {
  const { distPath } = resolvePinnedSource();
  return readFileSync(distPath, "utf8");
}

function expectExtractionFailure(source, pattern) {
  assert.throws(
    () => extractRulesFromSource(source),
    (error) => {
      assert.match(error.message, pattern);
      return true;
    }
  );
}

function validDocument(rules) {
  const globalRules = rules.filter((rule) => rule.kind !== "no-global-target");
  const noGlobalRules = rules.filter((rule) => rule.kind === "no-global-target");
  while (globalRules.length < EXPECTED_GLOBAL_TARGET_COUNT) {
    globalRules.push({ kind: "home-relative", path: `.generated-${globalRules.length}/skills` });
  }
  while (noGlobalRules.length < EXPECTED_NO_GLOBAL_TARGET_COUNT) {
    noGlobalRules.push({ kind: "no-global-target" });
  }
  assert.equal(globalRules.length, EXPECTED_GLOBAL_TARGET_COUNT);
  assert.equal(noGlobalRules.length, EXPECTED_NO_GLOBAL_TARGET_COUNT);
  const agents = [...globalRules, ...noGlobalRules].map((rule, index) => ({
    id: `agent-${index}`,
    usesUniversalTarget: false,
    rule,
    detector: { kind: "never" },
  }));
  return {
    schemaVersion: 3,
    skillsVersion: PINNED_SKILLS_VERSION,
    agentCount: EXPECTED_AGENT_COUNT,
    globalTargetCount: EXPECTED_GLOBAL_TARGET_COUNT,
    noGlobalTargetCount: EXPECTED_NO_GLOBAL_TARGET_COUNT,
    agents,
  };
}

describe("pinned skills target-rule extraction contract", () => {
  test("resolves the bin entry and derives the sibling dist bundle", () => {
    const resolved = resolvePinnedSource();

    assert.match(resolved.cliPath, /node_modules[\\/]skills[\\/]bin[\\/]cli\.mjs$/);
    assert.match(resolved.distPath, /node_modules[\\/]skills[\\/]dist[\\/]cli\.mjs$/);
    assert.notEqual(resolved.cliPath, resolved.distPath);
  });

  test("extracts the exact pinned inventory", () => {
    const document = extractRulesFromSource(sourceFixture());
    const ids = document.agents.map((agent) => agent.id);
    const rules = document.agents.map((agent) => agent.rule);
    const codex = document.agents.find((agent) => agent.id === "codex");
    const claudeCode = document.agents.find((agent) => agent.id === "claude-code");

    assert.equal(document.schemaVersion, 3);
    assert.equal(document.skillsVersion, PINNED_SKILLS_VERSION);
    assert.equal(document.agentCount, EXPECTED_AGENT_COUNT);
    assert.equal(document.globalTargetCount, EXPECTED_GLOBAL_TARGET_COUNT);
    assert.equal(document.noGlobalTargetCount, EXPECTED_NO_GLOBAL_TARGET_COUNT);
    assert.equal(ids.length, EXPECTED_AGENT_COUNT);
    assert.equal(new Set(ids).size, EXPECTED_AGENT_COUNT);
    assert.equal(rules.length, EXPECTED_AGENT_COUNT);
    assert.equal(rules.filter((rule) => rule.kind === "no-global-target").length, EXPECTED_NO_GLOBAL_TARGET_COUNT);
    assert.equal(rules.filter((rule) => rule.kind !== "no-global-target").length, EXPECTED_GLOBAL_TARGET_COUNT);
    assert.ok(rules.every((rule) => [
      "home-relative",
      "config-home-relative",
      "environment-or-home",
      "first-existing-home-relative",
      "no-global-target",
    ].includes(rule.kind)));
    assert.ok(document.agents.every((agent) => agent.detector));
    assert.equal(codex.usesUniversalTarget, true);
    assert.equal(claudeCode.usesUniversalTarget, false);
  });

  test("checked-in JSON exactly matches the pinned source", () => {
    const document = loadRules(generatedPath);
    assert.deepEqual(document, extractRulesFromSource(sourceFixture()));
  });

  test("rejects an unknown global target expression", () => {
    const source = sourceFixture().replace(
      'globalSkillsDir: join(home, ".aider-desk/skills")',
      "globalSkillsDir: getMysteryTarget()"
    );
    expectExtractionFailure(source, /unknown|unrecognized/i);
  });

  test("rejects a missing agent name", () => {
    const source = sourceFixture().replace('name: "aider-desk",', "");
    expectExtractionFailure(source, /missing|name|agent/i);
  });

  test("rejects duplicate agent IDs", () => {
    const source = sourceFixture()
      .replace('\tamp: {', '\t"aider-desk": {')
      .replace('name: "amp",', 'name: "aider-desk",');
    expectExtractionFailure(source, /duplicate/i);
  });

  test("rejects an empty agents result", () => {
    const source = sourceFixture().replace(
      /const agents = \{[\s\S]*?\n\};\nasync function detectInstalledAgents/,
      "const agents = {};\nasync function detectInstalledAgents"
    );
    expectExtractionFailure(source, /empty|count|agent|anchor/i);
  });

  test("rejects a bundle version mismatch", () => {
    const source = sourceFixture().replace(
      `var version$1 = "${PINNED_SKILLS_VERSION}";`,
      'var version$1 = "1.5.21";'
    );
    expectExtractionFailure(source, /version/i);
  });

  test("rejects a rule count mismatch", () => {
    const source = sourceFixture().replace(
      'globalSkillsDir: join(home, ".aider-desk/skills")',
      "globalSkillsDir: void 0"
    );
    expectExtractionFailure(source, /count/i);
  });

  test("rejects an oversized source before parsing it", () => {
    expectExtractionFailure(`${sourceFixture()}${" ".repeat(1024 * 1024)}`, /size|oversized|large/i);
  });

  test("rejects duplicate anchors", () => {
    expectExtractionFailure(`${sourceFixture()}\nconst agents = {};\n`, /duplicate|anchor/i);
  });

  test("rejects an unterminated recognized section", () => {
    const source = sourceFixture().replace(/\n\};\nasync function detectInstalledAgents/, "\nasync function detectInstalledAgents");
    expectExtractionFailure(source, /unterminated|section|agent|missing/i);
  });

  test("the generator supports write followed by check", () => {
    const writePath = join(npmRoot, "generated", ".skills-rules-test.json");
    const write = spawnSync(process.execPath, [extractorPath, "--write", writePath], {
      cwd: dirname(npmRoot),
      encoding: "utf8",
    });
    assert.equal(write.status, 0, write.stderr);

    const check = spawnSync(process.execPath, [extractorPath, "--check", writePath], {
      cwd: dirname(npmRoot),
      encoding: "utf8",
    });
    assert.equal(check.status, 0, check.stderr);

    const cleanup = spawnSync(process.execPath, [
      "-e",
      `const { unlinkSync } = require("node:fs"); unlinkSync(${JSON.stringify(writePath)});`,
    ], { encoding: "utf8" });
    assert.equal(cleanup.status, 0, cleanup.stderr);
  });
});

describe("runtime target resolution", () => {
  const home = "/home/tester";
  const runtime = {
    env: {},
    home,
    platform: "linux",
    exists: () => false,
  };

  test("resolves literal home-relative paths", () => {
    assert.equal(
      resolveRule({ kind: "home-relative", path: ".pi/agent/skills" }, runtime),
      "/home/tester/.pi/agent/skills"
    );
  });

  test("uses trimmed environment values and falls back for blank values", () => {
    const cases = [
      ["CLAUDE_CONFIG_DIR", ".claude"],
      ["CODEX_HOME", ".codex"],
      ["HERMES_HOME", ".hermes"],
      ["GROK_HOME", ".grok"],
      ["VIBE_HOME", ".vibe"],
      ["AUTOHAND_HOME", ".autohand"],
    ];

    for (const [variable, fallback] of cases) {
      const rule = {
        kind: "environment-or-home",
        variable,
        fallback,
        suffix: "skills",
      };
      assert.equal(
        resolveRule(rule, { ...runtime, env: { [variable]: `  /custom/${variable}  ` } }),
        `/custom/${variable}/skills`
      );
      assert.equal(
        resolveRule(rule, { ...runtime, env: { [variable]: " \n\t" } }),
        `${home}/${fallback}/skills`
      );
    }
  });

  test("resolves config-home targets using pinned XDG precedence", () => {
    const rule = {
      kind: "config-home-relative",
      path: "agents/skills",
    };
    assert.equal(
      resolveRule(rule, { ...runtime, env: { XDG_CONFIG_HOME: "/xdg/config" } }),
      "/xdg/config/agents/skills"
    );
    assert.equal(resolveRule(rule, runtime), "/home/tester/.config/agents/skills");
    assert.equal(
      resolveRule(rule, { ...runtime, env: { XDG_CONFIG_HOME: "" } }),
      "/home/tester/.config/agents/skills"
    );
  });

  test("follows OpenClaw home-directory priority and fallback", () => {
    const rule = {
      kind: "first-existing-home-relative",
      candidates: [".openclaw", ".clawdbot", ".moltbot"],
      suffix: "skills",
      fallback: ".openclaw",
    };
    for (const selected of [".openclaw", ".clawdbot", ".moltbot"]) {
      const checked = [];
      const target = resolveRule(rule, {
        ...runtime,
        exists: (path) => {
          checked.push(path);
          return path === `${home}/${selected}`;
        },
      });
      assert.equal(target, `${home}/${selected}/skills`);
      assert.deepEqual(checked, [
        `${home}/.openclaw`,
        ...(selected === ".openclaw" ? [] : [`${home}/.clawdbot`]),
        ...(selected === ".moltbot" && selected !== ".openclaw" ? [`${home}/.moltbot`] : []),
      ]);
    }
    assert.equal(
      resolveRule(rule, { ...runtime, exists: () => false }),
      `${home}/.openclaw/skills`
    );
  });

  test("evaluates every pinned detector path expression kind", () => {
    const existing = new Set([
      `${home}/.home-marker`,
      "/xdg/marker",
      "/cwd/marker",
      "/absolute/marker",
      "/custom/app/marker",
      "/cwd/relative-codex",
      `${home}/.codex`,
    ]);
    const exists = (candidate) => existing.has(candidate);
    const base = { ...runtime, cwd: "/cwd", exists };
    assert.equal(evaluateDetector({ kind: "any-existing", paths: [{ kind: "home-relative", path: ".home-marker" }] }, base), true);
    assert.equal(evaluateDetector({ kind: "any-existing", paths: [{ kind: "config-home-relative", path: "marker" }] }, { ...base, env: { XDG_CONFIG_HOME: "/xdg" } }), true);
    assert.equal(evaluateDetector({ kind: "any-existing", paths: [{ kind: "cwd-relative", path: "marker" }] }, base), true);
    assert.equal(evaluateDetector({ kind: "any-existing", paths: [{ kind: "absolute", path: "/absolute/marker" }] }, base), true);
    assert.equal(evaluateDetector({ kind: "any-existing", paths: [{ kind: "environment-relative", variable: "APPDATA", suffix: "marker" }] }, { ...base, env: { APPDATA: "  /custom/app  " } }), true);
    assert.equal(evaluateDetector({ kind: "any-existing", paths: [{ kind: "environment-or-home", variable: "CODEX_HOME", fallback: ".codex" }] }, { ...base, env: { CODEX_HOME: " \t" } }), true);
    assert.equal(evaluateDetector({ kind: "any-existing", paths: [{ kind: "environment-or-home", variable: "CODEX_HOME", fallback: ".codex" }] }, { ...base, env: { CODEX_HOME: "relative-codex" } }), true);
  });

  test("detects Eve projects only for valid bounded package JSON", (t) => {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-eve-test-"));
    t.after(() => rmSync(root, { recursive: true, force: true }));
    mkdirSync(join(root, "agent"));
    const detector = {
      kind: "eve-project",
      agentPath: { kind: "cwd-relative", path: "agent" },
      packageJsonPath: { kind: "cwd-relative", path: "package.json" },
      dependency: "eve",
    };
    const runtimeValue = { ...runtime, cwd: root, exists: (candidate) => candidate === join(root, "agent") };
    writeFileSync(join(root, "package.json"), JSON.stringify({ devDependencies: { eve: "1" } }));
    assert.equal(evaluateDetector(detector, runtimeValue), true);
    writeFileSync(join(root, "package.json"), "{not-json");
    assert.equal(evaluateDetector(detector, runtimeValue), false);
    writeFileSync(join(root, "package.json"), "x".repeat(1024 * 1024 + 1));
    assert.equal(evaluateDetector(detector, runtimeValue), false);
  });

  test("filters no-global detections and preserves generated order", () => {
    const document = {
      schemaVersion: 3,
      skillsVersion: PINNED_SKILLS_VERSION,
      agentCount: 3,
      globalTargetCount: 2,
      noGlobalTargetCount: 1,
      agents: [
        { id: "first", usesUniversalTarget: false, rule: { kind: "home-relative", path: ".first" }, detector: { kind: "never" } },
        { id: "second", usesUniversalTarget: false, rule: { kind: "home-relative", path: ".second" }, detector: { kind: "any-existing", paths: [{ kind: "home-relative", path: ".second" }] } },
        { id: "eve", usesUniversalTarget: false, rule: { kind: "no-global-target" }, detector: { kind: "any-existing", paths: [{ kind: "home-relative", path: ".eve" }] } },
      ],
    };
    const detected = detectInstalledAgents(document, { ...runtime, exists: (candidate) => candidate === `${home}/.second` || candidate === `${home}/.eve` });
    assert.deepEqual(detected, ["second", "eve"]);
  });

  test("resolves no-global markers to no target", () => {
    assert.equal(resolveRule({ kind: "no-global-target" }, runtime), null);
  });

  test("rejects relative path traversal at load and resolution boundaries", (t) => {
    const unsafeRules = [
      { kind: "home-relative", path: "../../etc" },
      { kind: "config-home-relative", path: "../escape" },
      {
        kind: "environment-or-home",
        variable: "CODEX_HOME",
        fallback: "../../etc",
        suffix: "skills",
      },
      {
        kind: "environment-or-home",
        variable: "CODEX_HOME",
        fallback: ".codex",
        suffix: "../escape",
      },
      {
        kind: "first-existing-home-relative",
        candidates: ["../escape"],
        fallback: ".openclaw",
        suffix: "skills",
      },
    ];

    for (const rule of unsafeRules) {
      assert.throws(() => resolveRule(rule, runtime), /relative|unsafe|path|traversal/i);
    }

    const temporaryDirectory = mkdtempSync(join(tmpdir(), "pi-worker-rules-test-"));
    const documentPath = join(temporaryDirectory, "rules.json");
    t.after(() => rmSync(temporaryDirectory, { recursive: true, force: true }));
    writeFileSync(documentPath, JSON.stringify(validDocument([unsafeRules[0]])), "utf8");
    assert.throws(() => loadRules(documentPath), /relative|unsafe|path|traversal/i);
  });

  test("rejects unknown fields at the document, agent, and every rule level", (t) => {
    const temporaryDirectory = mkdtempSync(join(tmpdir(), "pi-worker-rules-schema-test-"));
    t.after(() => rmSync(temporaryDirectory, { recursive: true, force: true }));
    let serial = 0;
    const assertRejected = (document) => {
      const documentPath = join(temporaryDirectory, `rules-${serial++}.json`);
      writeFileSync(documentPath, JSON.stringify(document), "utf8");
      assert.throws(() => loadRules(documentPath), /unknown field/i);
    };

    const topLevel = validDocument([]);
    topLevel.unexpected = true;
    assertRejected(topLevel);

    const agent = validDocument([]);
    agent.agents[0].unexpected = true;
    assertRejected(agent);

    for (const rule of [
      { kind: "home-relative", path: ".skills" },
      { kind: "config-home-relative", path: "skills" },
      {
        kind: "environment-or-home",
        variable: "CODEX_HOME",
        fallback: ".codex",
        suffix: "skills",
      },
      {
        kind: "first-existing-home-relative",
        candidates: [".openclaw"],
        fallback: ".openclaw",
        suffix: "skills",
      },
      { kind: "no-global-target" },
    ]) {
      const ruleDocument = validDocument([rule]);
      ruleDocument.agents[0].rule.unexpected = true;
      assertRejected(ruleDocument);
    }
  });

  test("requires an explicit filesystem predicate for first-existing rules", () => {
    assert.throws(
      () => resolveRule({
        kind: "first-existing-home-relative",
        candidates: [".openclaw", ".clawdbot", ".moltbot"],
        fallback: ".openclaw",
        suffix: "skills",
      }, { env: {}, home, platform: "linux" }),
      /exists/i
    );
  });

  test("revalidates the full document before resolving targets", () => {
    assert.throws(
      () => resolveAllTargets({ agents: [] }, runtime),
      /schema|version|count|invalid/i
    );

    const document = loadRules(generatedPath);
    document.agents[0].rule.path = "../../etc";
    assert.throws(() => resolveAllTargets(document, runtime), /relative|unsafe|path|traversal/i);
  });

  test("deduplicates only after resolving every rule", () => {
    const calls = [];
    const document = validDocument([
      {
        kind: "environment-or-home",
        variable: "CODEX_HOME",
        fallback: ".unused",
        suffix: "skills",
      },
      { kind: "home-relative", path: ".shared/skills" },
      { kind: "no-global-target" },
    ]);
    const targets = resolveAllTargets(document, {
      ...runtime,
      env: { CODEX_HOME: `${home}/.shared` },
      exists: (path) => {
        calls.push(path);
        return false;
      },
    });

    assert.equal(targets[0], `${home}/.shared/skills`);
    assert.equal(targets.filter((target) => target === `${home}/.shared/skills`).length, 1);
    assert.deepEqual(calls, []);
  });

  test("deduplicates case-insensitively after Windows resolution", () => {
    const document = validDocument([
      { kind: "home-relative", path: ".Shared/skills" },
      {
        kind: "environment-or-home",
        variable: "CODEX_HOME",
        fallback: ".unused",
        suffix: "skills",
      },
    ]);
    const targets = resolveAllTargets(document, {
      env: { CODEX_HOME: "c:\\users\\u\\.shared" },
      home: "C:\\Users\\U",
      platform: "win32",
      exists: () => false,
    });

    assert.equal(
      targets.filter((target) => target.toLowerCase() === "c:\\users\\u\\.shared\\skills").length,
      1
    );
  });
});
