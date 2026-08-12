import assert from "node:assert/strict";
import { createHash } from "node:crypto";
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
import { tmpdir } from "node:os";
import { join, relative } from "node:path";
import { createServer } from "node:net";
import { test } from "node:test";

import { classifyTarget, hashSkillTree } from "../lib/skill-tree.mjs";

const IDENTITY_FILE = "PI_WORKER_IDENTITY";
const IDENTITY_CONTENT = "pi-worker-skill/v1\n";

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function makeTree(t, files = {}) {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-skill-tree-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  for (const [relative, content] of Object.entries(files)) {
    const path = join(root, relative);
    mkdirSync(join(path, ".."), { recursive: true });
    writeFileSync(path, content);
  }
  return root;
}

function temporaryPath(t, name) {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-skill-extra-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  return join(root, name);
}

function target(path, kind = "canonical") {
  return { path, kind };
}

function receiptFor(path, tree, kind = "canonical", files = tree.files) {
  return receiptWithTargets([{ path, kind, files }]);
}

function receiptWithTargets(targets) {
  return {
    schemaVersion: 1,
    installerVersion: "0.1.0",
    skillsVersion: "1.5.22",
    outcome: "installed",
    targets: targets.map(({ path, kind, files }) => ({
      path,
      kind,
      files: files.map((file) => ({ ...file })),
    })),
    affectedTargets: [],
    recovery: [],
  };
}

test("hashSkillTree returns sorted relative file paths and lowercase SHA-256 hashes", async (t) => {
  const root = makeTree(t, {
    "z-last.txt": "last",
    "nested/b.txt": Buffer.from([0, 255, 1]),
    "nested/a.txt": "first",
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });

  const tree = await hashSkillTree(root);

  assert.deepEqual(tree.files.map(({ path }) => path), [
    IDENTITY_FILE,
    "SKILL.md",
    "nested/a.txt",
    "nested/b.txt",
    "z-last.txt",
  ]);
  assert.deepEqual(tree.files, [
    { path: IDENTITY_FILE, sha256: sha256(IDENTITY_CONTENT) },
    { path: "SKILL.md", sha256: sha256("---\nname: pi-worker\n---\n") },
    { path: "nested/a.txt", sha256: sha256("first") },
    { path: "nested/b.txt", sha256: sha256(Buffer.from([0, 255, 1])) },
    { path: "z-last.txt", sha256: sha256("last") },
  ]);
  assert.ok(tree.files.every(({ sha256: digest }) => /^[0-9a-f]{64}$/.test(digest)));

  const second = await hashSkillTree(root);
  assert.deepEqual(second, tree);
});

test("hashSkillTree preserves the identity marker bytes in the manifest", async (t) => {
  const root = makeTree(t, {
    [IDENTITY_FILE]: Buffer.from(IDENTITY_CONTENT, "utf8"),
  });

  const tree = await hashSkillTree(root);
  assert.deepEqual(tree.files, [{
    path: IDENTITY_FILE,
    sha256: sha256(Buffer.from(IDENTITY_CONTENT, "utf8")),
  }]);
  assert.equal(readFileSync(join(root, IDENTITY_FILE), "utf8"), IDENTITY_CONTENT);
});

test("hashSkillTree rejects symlinks, including a directory cycle", async (t) => {
  const root = makeTree(t, { "file.txt": "ok" });
  symlinkSync("file.txt", join(root, "link.txt"));
  await assert.rejects(hashSkillTree(root), /symlink/i);

  const cycle = makeTree(t, { "nested/file.txt": "ok" });
  symlinkSync("..", join(cycle, "nested", "cycle"));
  await assert.rejects(hashSkillTree(cycle), /symlink|cycle/i);
});

test("empty subdirectories fail hashing and make owned or unmanaged trees conflicting", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);

  const owned = makeTree(t);
  cpSync(bundled, owned, { recursive: true });
  mkdirSync(join(owned, "empty"));
  await assert.rejects(hashSkillTree(owned), /empty directory/i);
  assert.equal(
    await classifyTarget({
      target: target(owned),
      bundledTree,
      receipt: receiptFor(owned, bundledTree),
    }),
    "conflicting",
  );

  const unmanaged = makeTree(t);
  cpSync(bundled, unmanaged, { recursive: true });
  mkdirSync(join(unmanaged, "empty"));
  assert.equal(
    await classifyTarget({ target: target(unmanaged), bundledTree, receipt: null }),
    "conflicting",
  );
});

test("hashSkillTree rejects unreadable entries and special/non-directory roots", async (t) => {
  const root = makeTree(t, { "secret.txt": "secret" });
  const secretPath = join(root, "secret.txt");
  chmodSync(secretPath, 0o000);
  t.after(() => {
    try {
      chmodSync(secretPath, 0o600);
    } catch {
      // The tree cleanup hook may already have removed the fixture.
    }
  });
  await assert.rejects(hashSkillTree(root), /read|permission|unreadable/i);

  const file = temporaryPath(t, "not-a-directory");
  writeFileSync(file, "file");
  await assert.rejects(hashSkillTree(file), /directory|tree|root/i);
});

test("hashSkillTree rejects special files, path escape, and duplicate manifest paths", async (t) => {
  const root = makeTree(t, { "file.txt": "ok" });

  if (process.platform !== "win32") {
    const specialRoot = makeTree(t, { "file.txt": "ok" });
    const socketPath = join(specialRoot, "socket");
    const server = createServer();
    await new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(socketPath, resolve);
    });
    t.after(() => server.close());
    await assert.rejects(hashSkillTree(specialRoot), /special/i);
  }

  const digest = sha256("ok");
  await assert.rejects(
    hashSkillTree(root, {
      manifest: [
        { path: "file.txt", sha256: digest },
        { path: "file.txt", sha256: digest },
      ],
    }),
    /duplicate|normalized/i,
  );
  await assert.rejects(
    hashSkillTree(root, {
      manifest: [{ path: "../outside.txt", sha256: digest }],
    }),
    /escape|relative/i,
  );
});

test("classifyTarget distinguishes absent, owned, unmanaged, drifted, and conflicting", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n\nCanonical skill\n",
    "references/guide.md": "guide\n",
  });
  const bundledTree = await hashSkillTree(bundled);

  const absent = temporaryPath(t, "absent");
  assert.equal(await classifyTarget({ target: target(absent), bundledTree, receipt: null }), "absent");

  const owned = makeTree(t);
  cpSync(bundled, owned, { recursive: true });
  assert.equal(
    await classifyTarget({
      target: target(owned),
      bundledTree,
      receipt: receiptFor(owned, bundledTree),
    }),
    "owned",
  );

  const unmanaged = makeTree(t);
  cpSync(bundled, unmanaged, { recursive: true });
  assert.equal(
    await classifyTarget({ target: target(unmanaged), bundledTree, receipt: null }),
    "unmanaged",
  );

  const drifted = makeTree(t);
  cpSync(bundled, drifted, { recursive: true });
  writeFileSync(join(drifted, "references/guide.md"), "changed\n");
  assert.equal(
    await classifyTarget({
      target: target(drifted),
      bundledTree,
      receipt: receiptFor(drifted, bundledTree),
    }),
    "drifted",
  );

  const markerlessSameName = makeTree(t, {
    "SKILL.md": "---\nname: pi-worker\n---\nforeign\n",
  });
  assert.equal(
    await classifyTarget({ target: target(markerlessSameName), bundledTree, receipt: null }),
    "conflicting",
  );

  const foreign = makeTree(t, { "other.txt": "foreign" });
  assert.equal(
    await classifyTarget({ target: target(foreign), bundledTree, receipt: null }),
    "conflicting",
  );
});

test("a structurally valid receipt from another skills version cannot confer ownership", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const targetRoot = makeTree(t);
  cpSync(bundled, targetRoot, { recursive: true });
  const receipt = receiptFor(targetRoot, bundledTree);
  receipt.skillsVersion = "1.5.21";

  assert.equal(
    await classifyTarget({ target: { path: targetRoot }, bundledTree, receipt }),
    "unmanaged",
  );
});

test("expectedKind requires the receipt topology to match the filesystem role", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const targetRoot = makeTree(t);
  cpSync(bundled, targetRoot, { recursive: true });
  const canonical = makeTree(t);
  cpSync(bundled, canonical, { recursive: true });
  const receipt = receiptWithTargets([
    { path: canonical, kind: "canonical", files: bundledTree.files },
    { path: targetRoot, kind: "copy", files: bundledTree.files },
  ]);

  assert.equal(
    await classifyTarget({
      target: { path: targetRoot, expectedKind: "canonical" },
      bundledTree,
      receipt,
    }),
    "conflicting",
  );
  assert.equal(
    await classifyTarget({
      target: { path: targetRoot, expectedKind: "copy" },
      bundledTree,
      receipt,
    }),
    "owned",
  );
});

test("a copy receipt cannot authorize a root symlink destination", async (t) => {
  if (process.platform === "win32") t.skip("symlink permissions vary on windows");
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const canonical = makeTree(t);
  cpSync(bundled, canonical, { recursive: true });
  const parent = makeTree(t);
  const entry = join(parent, "pi-worker");
  const destination = relative(parent, canonical);
  symlinkSync(destination, entry);
  const receipt = receiptWithTargets([
    { path: canonical, kind: "copy", files: bundledTree.files },
    { path: parent, kind: "symlink", files: [{ path: "pi-worker", sha256: sha256(destination) }] },
  ]);

  assert.notEqual(
    await classifyTarget({
      target: { path: entry, expectedKind: "symlink" },
      bundledTree,
      receipt,
    }),
    "owned",
  );
});

test("a marker without a valid receipt is evidence only and never owned", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const targetRoot = makeTree(t);
  cpSync(bundled, targetRoot, { recursive: true });

  assert.equal(
    await classifyTarget({ target: target(targetRoot), bundledTree, receipt: {} }),
    "unmanaged",
  );
  assert.equal(
    await classifyTarget({
      target: target(targetRoot),
      bundledTree,
      receipt: receiptFor(temporaryPath(t, "different"), bundledTree),
    }),
    "unmanaged",
  );
});

test("owned requires exact receipt topology and matching managed hashes", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const targetRoot = makeTree(t);
  cpSync(bundled, targetRoot, { recursive: true });

  const canonical = makeTree(t);
  cpSync(bundled, canonical, { recursive: true });
  const wrongKind = receiptWithTargets([
    { path: canonical, kind: "canonical", files: bundledTree.files },
    { path: targetRoot, kind: "copy", files: bundledTree.files },
  ]);
  assert.equal(
    await classifyTarget({ target: target(targetRoot, "canonical"), bundledTree, receipt: wrongKind }),
    "owned",
  );

  const wrongPath = receiptFor(temporaryPath(t, "not-target"), bundledTree);
  assert.equal(
    await classifyTarget({ target: target(targetRoot), bundledTree, receipt: wrongPath }),
    "unmanaged",
  );

  const wrongHash = receiptFor(targetRoot, bundledTree);
  wrongHash.targets[0].files[0] = {
    ...wrongHash.targets[0].files[0],
    sha256: sha256("not-the-marker"),
  };
  assert.equal(
    await classifyTarget({ target: target(targetRoot), bundledTree, receipt: wrongHash }),
    "unmanaged",
  );

  const invalidTopology = receiptFor(targetRoot, bundledTree);
  invalidTopology.targets[0].files.push({ path: "../escape", sha256: "0".repeat(64) });
  assert.equal(
    await classifyTarget({ target: target(targetRoot), bundledTree, receipt: invalidTopology }),
    "unmanaged",
  );
});

test("ownership identity must belong to the matched directory target", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const canonical = makeTree(t);
  cpSync(bundled, canonical, { recursive: true });
  const unrelated = makeTree(t, {
    "SKILL.md": "---\nname: pi-worker\n---\nforeign\n",
  });
  const unrelatedTree = await hashSkillTree(unrelated);
  const receipt = receiptWithTargets([
    { path: canonical, kind: "canonical", files: bundledTree.files },
    { path: unrelated, kind: "copy", files: unrelatedTree.files },
  ]);

  assert.equal(
    await classifyTarget({ target: target(unrelated), bundledTree, receipt }),
    "conflicting",
  );
});

test("ownership receipts require exactly one canonical target", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const first = makeTree(t);
  const second = makeTree(t);
  cpSync(bundled, first, { recursive: true });
  cpSync(bundled, second, { recursive: true });
  const receipt = receiptWithTargets([
    { path: first, kind: "canonical", files: bundledTree.files },
    { path: second, kind: "canonical", files: bundledTree.files },
  ]);

  assert.equal(
    await classifyTarget({ target: target(first), bundledTree, receipt }),
    "unmanaged",
  );
});

test("files outside the bundled manifest are conflicting even with an otherwise valid receipt", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const targetRoot = makeTree(t);
  cpSync(bundled, targetRoot, { recursive: true });
  writeFileSync(join(targetRoot, "unmanaged.txt"), "foreign\n");

  assert.equal(
    await classifyTarget({
      target: target(targetRoot),
      bundledTree,
      receipt: receiptFor(targetRoot, bundledTree),
    }),
    "conflicting",
  );
});

test("classification derives directory topology instead of trusting target.kind", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const targetRoot = makeTree(t);
  cpSync(bundled, targetRoot, { recursive: true });

  assert.equal(
    await classifyTarget({
      target: target(targetRoot, "symlink"),
      bundledTree,
      receipt: receiptFor(targetRoot, bundledTree, "canonical"),
    }),
    "owned",
  );
});

test("root symlinks use the public parent-directory receipt topology", async (t) => {
  if (process.platform === "win32") t.skip("symlink permissions vary on windows");

  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const canonical = makeTree(t);
  cpSync(bundled, canonical, { recursive: true });
  const parent = makeTree(t);
  const entry = join(parent, "pi-worker");
  const destination = relative(parent, canonical);
  symlinkSync(destination, entry);

  const receipt = receiptWithTargets([
    { path: canonical, kind: "canonical", files: bundledTree.files },
    {
      path: parent,
      kind: "symlink",
      files: [{ path: "pi-worker", sha256: sha256(destination) }],
    },
  ]);

  assert.equal(
    await classifyTarget({
      target: target(entry, "canonical"),
      bundledTree,
      receipt,
    }),
    "owned",
  );
});

test("a symlink receipt cannot borrow identity from an unrelated canonical target", async (t) => {
  if (process.platform === "win32") t.skip("symlink permissions vary on windows");

  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const canonical = makeTree(t);
  cpSync(bundled, canonical, { recursive: true });
  const unrelated = makeTree(t);
  cpSync(bundled, unrelated, { recursive: true });
  const parent = makeTree(t);
  const entry = join(parent, "pi-worker");
  const destination = relative(parent, unrelated);
  symlinkSync(destination, entry);
  const receipt = receiptWithTargets([
    { path: canonical, kind: "canonical", files: bundledTree.files },
    {
      path: parent,
      kind: "symlink",
      files: [{ path: "pi-worker", sha256: sha256(destination) }],
    },
  ]);

  assert.equal(
    await classifyTarget({ target: target(entry), bundledTree, receipt }),
    "unmanaged",
  );
});

test("a hash-matching symlink receipt does not own a drifted canonical tree", async (t) => {
  if (process.platform === "win32") t.skip("symlink permissions vary on windows");

  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\nnew\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const canonical = makeTree(t);
  cpSync(bundled, canonical, { recursive: true });
  const parent = makeTree(t);
  const entry = join(parent, "pi-worker");
  const destination = relative(parent, canonical);
  symlinkSync(destination, entry);
  const receipt = receiptWithTargets([
    { path: canonical, kind: "canonical", files: bundledTree.files },
    { path: parent, kind: "symlink", files: [{ path: "pi-worker", sha256: sha256(destination) }] },
  ]);

  writeFileSync(join(canonical, "SKILL.md"), "---\nname: pi-worker\n---\ndrifted\n");
  assert.notEqual(
    await classifyTarget({ target: target(entry, "symlink"), bundledTree, receipt }),
    "owned",
  );
});

test("ownership receipts require the exact stable marker and allow prior-version files", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\nnew\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const targetRoot = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\nold\n",
    "previous-version.txt": "kept\n",
  });
  const currentTree = await hashSkillTree(targetRoot);

  assert.equal(
    await classifyTarget({
      target: target(targetRoot),
      bundledTree,
      receipt: receiptFor(targetRoot, currentTree),
    }),
    "owned",
  );

  const forgedMarker = receiptFor(targetRoot, currentTree);
  forgedMarker.targets[0].files = forgedMarker.targets[0].files.map((file) => (
    file.path === IDENTITY_FILE ? { ...file, sha256: sha256("forged") } : file
  ));
  assert.equal(
    await classifyTarget({
      target: target(targetRoot),
      bundledTree,
      receipt: forgedMarker,
    }),
    "conflicting",
  );
});

test("frontmatter identity requires valid, unique metadata", async (t) => {
  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const cases = [
    "---\nname: pi-worker\nmalformed metadata\n---\n",
    "---\nname: pi-worker\nname: pi-worker\n---\n",
    "---\nname: pi-worker\n",
  ];
  for (const content of cases) {
    const malformed = makeTree(t, {
      [IDENTITY_FILE]: IDENTITY_CONTENT,
      "SKILL.md": content,
    });
    assert.equal(
      await classifyTarget({ target: target(malformed), bundledTree, receipt: null }),
      "conflicting",
    );
  }

  const valid = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\n# generated by pi-worker\nname: pi-worker\ndescription: a valid extra field\n---\n",
  });
  assert.equal(
    await classifyTarget({ target: target(valid), bundledTree, receipt: null }),
    "unmanaged",
  );
});

test("an unowned root symlink may identify a safe skill tree but not unsafe trees", async (t) => {
  if (process.platform === "win32") t.skip("symlink permissions vary on windows");

  const bundled = makeTree(t, {
    [IDENTITY_FILE]: IDENTITY_CONTENT,
    "SKILL.md": "---\nname: pi-worker\n---\n",
  });
  const bundledTree = await hashSkillTree(bundled);
  const canonical = makeTree(t);
  cpSync(bundled, canonical, { recursive: true });
  const parent = makeTree(t);
  const entry = join(parent, "pi-worker");
  symlinkSync(relative(parent, canonical), entry);

  assert.equal(
    await classifyTarget({ target: target(entry), bundledTree, receipt: null }),
    "unmanaged",
  );

  const unsafe = makeTree(t, { [IDENTITY_FILE]: IDENTITY_CONTENT, "SKILL.md": "---\nname: pi-worker\n---\n" });
  symlinkSync("SKILL.md", join(unsafe, "internal-link"));
  const unsafeEntry = join(parent, "unsafe");
  symlinkSync(relative(parent, unsafe), unsafeEntry);
  assert.equal(
    await classifyTarget({ target: target(unsafeEntry), bundledTree, receipt: null }),
    "conflicting",
  );

  const danglingEntry = join(parent, "dangling");
  symlinkSync("missing", danglingEntry);
  assert.equal(
    await classifyTarget({ target: target(danglingEntry), bundledTree, receipt: null }),
    "conflicting",
  );
});

test("hashSkillTree enforces depth, file-count, per-file, and total-size limits", async (t) => {
  const deep = makeTree(t);
  const deepPath = [...Array(33)].map((_, index) => `d${index}`).join("/");
  mkdirSync(join(deep, deepPath), { recursive: true });
  writeFileSync(join(deep, deepPath, "file.txt"), "deep");
  await assert.rejects(hashSkillTree(deep), /depth|limit/i);

  const tooMany = makeTree(t);
  for (let index = 0; index < 257; index += 1) {
    writeFileSync(join(tooMany, `file-${index}.txt`), "x");
  }
  await assert.rejects(hashSkillTree(tooMany), /files|limit|256/i);

  const tooManyEntries = makeTree(t);
  for (let index = 0; index < 1025; index += 1) {
    mkdirSync(join(tooManyEntries, `entry-${index}`));
  }
  await assert.rejects(hashSkillTree(tooManyEntries), /entries|limit|1024/i);

  const tooLarge = makeTree(t, { "large.bin": Buffer.alloc(1024 * 1024 + 1) });
  await assert.rejects(hashSkillTree(tooLarge), /size|limit|MiB/i);

  const tooMuch = makeTree(t);
  for (let index = 0; index < 5; index += 1) {
    writeFileSync(join(tooMuch, `large-${index}.bin`), Buffer.alloc(1024 * 1024));
  }
  await assert.rejects(hashSkillTree(tooMuch), /total|size|limit|MiB/i);
});
