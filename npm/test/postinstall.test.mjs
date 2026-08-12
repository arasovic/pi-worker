import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import process from "node:process";
import { test } from "node:test";

import {
  postinstallDiagnostic,
  runPostinstall,
} from "../scripts/postinstall.mjs";

test("non-interactive postinstall renders one concise diagnostic for every product outcome", async () => {
  const cases = [
    ["installed", "pi-worker: skill installed"],
    ["blocked", "pi-worker: skill installation blocked; run pi-worker skill status"],
    ["skipped", "pi-worker: skill installation skipped; run pi-worker skill status"],
    ["failed", "pi-worker: skill installation failed; run pi-worker skill status"],
  ];

  for (const [outcome, expected] of cases) {
    const lines = [];
    assert.equal(postinstallDiagnostic({ outcome, interactive: false, version: "0.1.0" }), expected);
    assert.equal(await runPostinstall({
      install: async () => ({ outcome }),
      isTTY: false,
      write: (line) => lines.push(line),
    }), outcome);
    assert.deepEqual(lines, [expected]);
  }
});

test("interactive installed postinstall renders an aligned status block", async () => {
  const lines = [];
  const outcome = await runPostinstall({
    install: async () => ({ outcome: "installed", targetCount: 3 }),
    env: {},
    isTTY: true,
    version: "0.1.0",
    write: (line) => lines.push(line),
  });

  assert.equal(outcome, "installed");
  assert.deepEqual(lines, [
    "Pi Worker 0.1.0\n" +
    "  skill   \u001b[32minstalled\u001b[0m · 3 targets\n" +
    "  next    pi-worker doctor",
  ]);
});

test("interactive unsuccessful postinstall keeps status and recovery aligned", () => {
  const cases = [
    ["blocked", 33],
    ["skipped", 33],
    ["failed", 31],
  ];

  for (const [outcome, color] of cases) {
    assert.equal(
      postinstallDiagnostic({ outcome, interactive: true, version: "0.1.0" }),
      "Pi Worker 0.1.0\n" +
      `  skill   \u001b[${color}m${outcome}\u001b[0m\n` +
      "  next    pi-worker skill status",
    );
  }
});

test("NO_COLOR and CI suppress the interactive status block", async () => {
  for (const env of [{ NO_COLOR: "" }, { CI: "1" }]) {
    const lines = [];
    await runPostinstall({
      install: async () => ({ outcome: "installed", targetCount: 2 }),
      env,
      isTTY: true,
      version: "0.1.0",
      write: (line) => lines.push(line),
    });

    assert.deepEqual(lines, ["pi-worker: skill installed"]);
  }
});

test("postinstall soft-fails thrown product errors without leaking their message", async () => {
  const upstreamDetail = ["cred", "ential=secret"].join("");
  const lines = [];
  const outcome = await runPostinstall({
    install: async () => { throw new Error(`${upstreamDetail} raw upstream output`); },
    isTTY: false,
    write: (line) => lines.push(line),
  });

  assert.equal(outcome, "failed");
  assert.deepEqual(lines, ["pi-worker: skill installation failed; run pi-worker skill status"]);
  assert.doesNotMatch(lines[0], /credential|secret|upstream/i);
});

test("a throwing product install leaves the hosting Node process at exit zero", () => {
  const moduleURL = new URL("../scripts/postinstall.mjs", import.meta.url).href;
  const source = `
    import { runPostinstall } from ${JSON.stringify(moduleURL)};
    await runPostinstall({ install: async () => { throw new Error("private detail"); } });
  `;
  const child = spawnSync(process.execPath, ["--input-type=module", "--eval", source], {
    encoding: "utf8",
  });

  assert.equal(child.status, 0, child.stderr);
  assert.equal(child.stdout, "");
  assert.equal(child.stderr, "pi-worker: skill installation failed; run pi-worker skill status\n");
});
