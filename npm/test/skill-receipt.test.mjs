import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import {
  chmod,
  mkdir,
  readFile,
  readdir,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { mkdtempSync, realpathSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { describe, test } from "node:test";

import {
  receiptPathFromNative,
  writeReceipt,
} from "../lib/skill-receipt.mjs";

function nativeSpawn({ stdout = "", stderr = "", code = 0, signal = null }) {
  const calls = [];
  const spawn = (binary, args, options) => {
    calls.push({ binary, args, options });
    const child = new EventEmitter();
    child.stdout = new PassThrough();
    child.stderr = new PassThrough();
    queueMicrotask(() => {
      child.stdout.end(stdout);
      child.stderr.end(stderr);
      child.emit("close", code, signal);
    });
    return child;
  };
  return { calls, spawn };
}

function validReceipt(targetPath) {
  return {
    schemaVersion: 1,
    installerVersion: "0.0.0-private",
    skillsVersion: "1.5.22",
    outcome: "installed",
    targets: [{
      path: targetPath,
      kind: "canonical",
      files: [{ path: "SKILL.md", sha256: "a".repeat(64) }],
    }],
    affectedTargets: [],
    recovery: [],
  };
}

function tempNames(parent) {
  return readdir(parent).then((names) => names.filter((name) => name.includes(".skill-install.json.")));
}

describe("receiptPathFromNative", () => {
  test("executes the package-local binary directly and accepts one valid document", async () => {
    const fake = nativeSpawn({
      stdout: JSON.stringify({ schemaVersion: 1, receiptPath: "/private/config/skill-install.json" }),
    });

    const result = await receiptPathFromNative({ binary: "/package/npm/native/linux-x64/pi-worker", spawn: fake.spawn });

    assert.equal(result, "/private/config/skill-install.json");
    assert.deepEqual(fake.calls, [{
      binary: "/package/npm/native/linux-x64/pi-worker",
      args: ["skill", "receipt-path", "--json"],
      options: {
        stdio: ["ignore", "pipe", "pipe"],
        shell: false,
        windowsHide: true,
      },
    }]);
  });

  for (const [name, result] of [
    ["stdout noise", { stdout: "notice\n{\"schemaVersion\":1,\"receiptPath\":\"/tmp/r\"}" }],
    ["multiple documents", { stdout: "{\"schemaVersion\":1,\"receiptPath\":\"/tmp/r\"}\n{\"schemaVersion\":1,\"receiptPath\":\"/tmp/s\"}" }],
    ["malformed JSON", { stdout: "{not-json" }],
    ["stderr noise", { stdout: JSON.stringify({ schemaVersion: 1, receiptPath: "/tmp/r" }), stderr: "warning\n" }],
    ["nonzero exit", { stdout: JSON.stringify({ schemaVersion: 1, receiptPath: "/tmp/r" }), code: 7 }],
    ["signal exit", { stdout: JSON.stringify({ schemaVersion: 1, receiptPath: "/tmp/r" }), signal: "SIGTERM", code: null }],
    ["relative path", { stdout: JSON.stringify({ schemaVersion: 1, receiptPath: "relative/skill-install.json" }) }],
    ["wrong schema", { stdout: JSON.stringify({ schemaVersion: 2, receiptPath: "/tmp/r" }) }],
    ["extra upstream field", { stdout: JSON.stringify({ schemaVersion: 1, receiptPath: "/tmp/r", env: "secret" }) }],
  ]) {
    test(`rejects ${name}`, async () => {
      const fake = nativeSpawn(result);
      await assert.rejects(
        receiptPathFromNative({ binary: "/package/npm/native/linux-x64/pi-worker", spawn: fake.spawn }),
        /receipt path|JSON|document|exit|signal|absolute|schema|noise|output/i
      );
    });
  }

  for (const stream of ["stdout", "stderr"]) {
    test(`bounds ${stream}, escalates, and reaps an overflowing native process`, async () => {
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
        queueMicrotask(() => {
          child[stream].emit("data", Buffer.alloc(64 * 1024 + 1, 0x41));
        });
        return child;
      };

      await assert.rejects(
        receiptPathFromNative({ binary: "/native/pi-worker", spawn }),
        /limit|overflow|output/i,
      );
      assert.deepEqual(signals, ["SIGTERM", "SIGKILL"]);
    });
  }

  test("escalates, reaps, and rejects a native process that exceeds its timeout", async () => {
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

    await assert.rejects(
      receiptPathFromNative({ binary: "/native/pi-worker", spawn, timeoutMs: 5 }),
      /timed out|timeout/i,
    );
    assert.deepEqual(signals, ["SIGTERM", "SIGKILL"]);
  });
});

describe("writeReceipt", () => {
  test("writes the exact public schema atomically with private modes", async (t) => {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-receipt-test-"));
    t.after(() => rm(root, { recursive: true, force: true }));
    const parent = join(root, "config");
    const path = join(parent, "skill-install.json");
    await mkdir(parent, { recursive: true, mode: 0o755 });
    await chmod(parent, 0o755);

    await writeReceipt(path, validReceipt(join(root, "target")));

    const stored = JSON.parse(await readFile(path, "utf8"));
    assert.deepEqual(Object.keys(stored), [
      "schemaVersion",
      "installerVersion",
      "skillsVersion",
      "outcome",
      "targets",
      "affectedTargets",
      "recovery",
    ]);
    assert.deepEqual(Object.keys(stored.affectedTargets), []);
    assert.equal((await stat(parent)).mode & 0o777, 0o700);
    assert.equal((await stat(path)).mode & 0o777, 0o600);
    assert.deepEqual(await tempNames(parent), []);
  });

  test("preserves an existing target when parent hardening is injected to fail", async (t) => {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-receipt-test-"));
    t.after(() => rm(root, { recursive: true, force: true }));
    const parent = join(root, "config");
    const path = join(parent, "skill-install.json");
    const before = "existing receipt";
    await mkdir(parent, { recursive: true, mode: 0o755 });
    await writeFile(path, before, { mode: 0o600 });

    await assert.rejects(
      writeReceipt(path, validReceipt(join(root, "target")), {
        platform: "linux",
        fs: {
          chmod: async (candidate, mode) => {
            throw new Error(`injected chmod failure for ${candidate}`);
          },
        },
      }),
      /chmod|injected/
    );

    assert.equal(await readFile(path, "utf8"), before);
    assert.deepEqual(await tempNames(parent), []);
  });

  test("preserves an existing target and cleans the temp file on injected write failure", async (t) => {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-receipt-test-"));
    t.after(() => rm(root, { recursive: true, force: true }));
    const parent = join(root, "config");
    const path = join(parent, "skill-install.json");
    const before = "existing receipt";
    await mkdir(parent, { recursive: true, mode: 0o700 });
    await writeFile(path, before, { mode: 0o600 });

    await assert.rejects(
      writeReceipt(path, validReceipt(join(root, "target")), {
        fs: {
          writeFile: async () => { throw new Error("injected write failure"); },
        },
      }),
      /write|injected/
    );

    assert.equal(await readFile(path, "utf8"), before);
    assert.deepEqual(await tempNames(parent), []);
  });

  test("rejects a receipt path whose final parent is a symlink", async (t) => {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-receipt-test-"));
    t.after(() => rm(root, { recursive: true, force: true }));
    const realParent = join(root, "real-config");
    const linkedParent = join(root, "linked-config");
    await mkdir(realParent, { recursive: true, mode: 0o700 });
    symlinkSync(realParent, linkedParent);
    const path = join(linkedParent, "skill-install.json");
    const existing = join(realParent, "skill-install.json");
    await writeFile(existing, "existing receipt", { mode: 0o600 });

    await assert.rejects(
      writeReceipt(path, validReceipt(join(root, "target"))),
      /symlink|parent/i,
    );
    assert.equal(await readFile(existing, "utf8"), "existing receipt");
    assert.deepEqual(await tempNames(realParent), []);
  });

  test("preserves the existing receipt when the real parent identity changes before rename", async (t) => {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-receipt-test-"));
    t.after(() => rm(root, { recursive: true, force: true }));
    const parent = join(root, "config");
    const path = join(parent, "skill-install.json");
    const before = "existing receipt";
    await mkdir(parent, { recursive: true, mode: 0o700 });
    await writeFile(path, before, { mode: 0o600 });

    const realParent = realpathSync(parent);
    let identityChecks = 0;
    await assert.rejects(
      writeReceipt(path, validReceipt(join(root, "target")), {
        fs: {
          stat: async (candidate) => {
            const info = await stat(candidate);
            if (candidate === realParent) {
              identityChecks += 1;
              if (identityChecks === 2) return { ...info, ino: info.ino + 1 };
            }
            return info;
          },
        },
      }),
      /identity|directory|changed/i,
    );

    assert.equal(identityChecks, 2);
    assert.equal(await readFile(path, "utf8"), before);
    assert.deepEqual(await tempNames(parent), []);
  });

  test("requires arrays and installed target/file cardinality without requiring targets for non-installed outcomes", async (t) => {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-receipt-test-"));
    t.after(() => rm(root, { recursive: true, force: true }));
    const path = join(root, "config", "skill-install.json");

    const noInstalledTargets = validReceipt(join(root, "target"));
    noInstalledTargets.targets = [];
    await assert.rejects(
      writeReceipt(path, noInstalledTargets),
      /target|installed|receipt/i,
    );

    const noTargetFiles = validReceipt(join(root, "target"));
    noTargetFiles.targets[0].files = [];
    await assert.rejects(
      writeReceipt(path, noTargetFiles),
      /file|target|receipt/i,
    );

    for (const [outcome, changes] of [
      ["blocked", {
        affectedTargets: [{ path: join(root, "blocked"), state: "conflicting", recovery: [] }],
      }],
      ["skipped", {}],
      ["failed", {}],
    ]) {
      const receipt = {
        ...validReceipt(join(root, "target")),
        outcome,
        targets: [],
        ...changes,
      };
      await writeReceipt(path, receipt);
      const stored = JSON.parse(await readFile(path, "utf8"));
      assert.equal(stored.outcome, outcome);
      assert.ok(Array.isArray(stored.targets));
      assert.ok(Array.isArray(stored.affectedTargets));
      assert.ok(Array.isArray(stored.recovery));
    }
  });

  test("rejects invalid schemas and enums before touching the target", async (t) => {
    const root = mkdtempSync(join(tmpdir(), "pi-worker-receipt-test-"));
    t.after(() => rm(root, { recursive: true, force: true }));
    const path = join(root, "config", "skill-install.json");
    const cases = [
      ["extra top-level field", { extra: "no" }],
      ["unknown outcome", { outcome: "unknown" }],
      ["unknown affected state", { outcome: "blocked", affectedTargets: [{ path: join(root, "x"), state: "unknown", recovery: [] }] }],
      ["installed affected target", { affectedTargets: [{ path: join(root, "x"), state: "unmanaged", recovery: [] }] }],
      ["blocked without affected target", { outcome: "blocked" }],
    ];

    for (const [name, changes] of cases) {
      await assert.rejects(
        writeReceipt(path, { ...validReceipt(join(root, "target")), ...changes }),
        /receipt|schema|outcome|state|affected|field|enum/i,
        name
      );
    }

    await assert.rejects(stat(dirname(path)), /ENOENT/);
  });
});
