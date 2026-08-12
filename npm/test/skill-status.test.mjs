import assert from "node:assert/strict";
import { test } from "node:test";

import { runSkillStatus } from "../lib/skill-status.mjs";

function nativeDocument(status = "verified") {
  return {
    schemaVersion: 1,
    receiptPath: "/receipt.json",
    status,
    verifiedTargets: ["/managed"],
    trackedTargets: ["/managed", "/drifted"],
    affectedTargets: [],
    recovery: [],
    externalInspection: { state: "unavailable", targets: [] },
  };
}

test("augments JSON status without changing the native exit code", async () => {
  let stdout = "";
  let stderr = "";
  const code = await runSkillStatus({
    binary: "/native/pi-worker",
    json: true,
    runCaptured: async () => ({
      code: 3,
      signal: null,
      stdout: `${JSON.stringify(nativeDocument("missing"))}\n`,
      stderr: "",
    }),
    inspect: async ({ excludePaths }) => {
      assert.deepEqual(excludePaths, ["/managed", "/drifted"]);
      return { state: "performed", targets: [{ path: "/external", identity: "current" }] };
    },
    writeStdout: (value) => { stdout += value; },
    writeStderr: (value) => { stderr += value; },
  });

  assert.equal(code, 3);
  assert.equal(stderr, "");
  const result = JSON.parse(stdout);
  assert.deepEqual(result.externalInspection, {
    state: "performed",
    targets: [{ path: "/external", identity: "current" }],
  });
});

test("reports unavailable atomically and keeps informational findings out of exit classification", async () => {
  let stdout = "";
  let stderr = "";
  const code = await runSkillStatus({
    binary: "/native/pi-worker",
    json: false,
    runCaptured: async () => ({
      code: 0,
      signal: null,
      stdout: `${JSON.stringify(nativeDocument())}\n`,
      stderr: "",
    }),
    inspect: async () => ({ state: "unavailable", targets: [] }),
    writeStdout: (value) => { stdout += value; },
    writeStderr: (value) => { stderr += value; },
  });

  assert.equal(code, 0);
  assert.match(stdout, /status: verified/);
  assert.match(stdout, /external-inspection: unavailable/);
  assert.match(stderr, /external skill inspection unavailable/);
});

test("rejects unknown or malformed fields in the native status document", async (t) => {
  const cases = [
    { ...nativeDocument(), unexpected: true },
    { ...nativeDocument(), status: "other" },
    { ...nativeDocument(), recovery: [7] },
    { ...nativeDocument(), externalInspection: { state: "performed", targets: null } },
    {
      ...nativeDocument(),
      affectedTargets: [{ path: "/affected", state: "drifted", recovery: [], extra: true }],
    },
  ];

  for (const [index, document] of cases.entries()) {
    await t.test(String(index), async () => {
      let stdout = "";
      let stderr = "";
      const code = await runSkillStatus({
        binary: "/native/pi-worker",
        json: true,
        runCaptured: async () => ({
          code: 0,
          signal: null,
          stdout: `${JSON.stringify(document)}\n`,
          stderr: "",
        }),
        inspect: async () => ({ state: "performed", targets: [] }),
        writeStdout: (value) => { stdout += value; },
        writeStderr: (value) => { stderr += value; },
      });

      assert.equal(code, 9);
      assert.equal(stdout, "");
      assert.equal(stderr, "pi-worker: invalid native skill status document\n");
    });
  }
});

test("bounds and flattens dynamic values in human status output", async () => {
  const document = nativeDocument();
  document.receiptPath = `/receipt\ninjected-${"x".repeat(2000)}`;
  let stdout = "";
  const code = await runSkillStatus({
    binary: "/native/pi-worker",
    json: false,
    runCaptured: async () => ({
      code: 0,
      signal: null,
      stdout: `${JSON.stringify(document)}\n`,
      stderr: "",
    }),
    inspect: async () => ({
      state: "performed",
      targets: [{ path: "/external\nforged", identity: "current" }],
    }),
    writeStdout: (value) => { stdout += value; },
  });

  assert.equal(code, 0);
  assert.doesNotMatch(stdout, /\ninjected-/);
  assert.doesNotMatch(stdout, /\nforged/);
  for (const line of stdout.trimEnd().split("\n")) assert.ok(line.length <= 1100, line);
});
