import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import process from "node:process";
import { test } from "node:test";

import {
  postinstallDiagnostic,
  runPostinstall,
} from "../scripts/postinstall.mjs";

test("postinstall renders one concise diagnostic for every product outcome", async () => {
  const cases = [
    ["installed", "pi-worker: skill installed"],
    ["blocked", "pi-worker: skill installation blocked; run pi-worker skill status"],
    ["skipped", "pi-worker: skill installation skipped; run pi-worker skill status"],
    ["failed", "pi-worker: skill installation failed; run pi-worker skill status"],
  ];

  for (const [outcome, expected] of cases) {
    const lines = [];
    assert.equal(postinstallDiagnostic(outcome), expected);
    assert.equal(await runPostinstall({
      install: async () => ({ outcome }),
      write: (line) => lines.push(line),
    }), outcome);
    assert.deepEqual(lines, [expected]);
  }
});

test("postinstall soft-fails thrown product errors without leaking their message", async () => {
  const upstreamDetail = ["cred", "ential=secret"].join("");
  const lines = [];
  const outcome = await runPostinstall({
    install: async () => { throw new Error(`${upstreamDetail} raw upstream output`); },
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
