import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  cpSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { test } from "node:test";

const repository = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const checker = join(repository, "npm", "scripts", "check-gofmt.mjs");

const formatted = "package fixture\n\nfunc identity(value int) int { return value }\n";
const unformatted = "package fixture\nfunc identity(value int) int { return value }\n";
const syntaxError = "package fixture\nfunc broken( {\n";

function git(root, ...args) {
  const result = spawnSync("git", args, { cwd: root, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
}

function fixture(t, entries = {}, afterAdd) {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-gofmt-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  cpSync(checker, join(root, "check-gofmt.mjs"));
  git(root, "init", "--quiet");
  git(root, "config", "user.email", "test@example.invalid");
  git(root, "config", "user.name", "Gofmt Test");
  for (const [path, value] of Object.entries(entries)) {
    const target = join(root, path);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, value);
  }
  git(root, "add", "--all");
  if (afterAdd) afterAdd(root);
  return root;
}

function run(root) {
  return spawnSync(process.execPath, [join(root, "check-gofmt.mjs")], {
    cwd: root,
    encoding: "utf8",
  });
}

test("accepts a formatted go file", (t) => {
  const root = fixture(t, { "plain.go": formatted });
  const result = run(root);
  assert.equal(result.status, 0, result.stderr);
});

test("rejects an unformatted go file", (t) => {
  const root = fixture(t, { "plain.go": unformatted });
  const result = run(root);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /plain\.go/);
});

test("rejects an unformatted go file whose name contains a space", (t) => {
  const root = fixture(t, { "with space.go": unformatted });
  const result = run(root);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /with space\.go/);
});

test("rejects a go file with a syntax error", (t) => {
  const root = fixture(t, { "bad.go": syntaxError });
  const result = run(root);
  assert.notEqual(result.status, 0);
});

test("rejects a tracked go file that was deleted from disk", (t) => {
  const root = fixture(t, { "gone.go": formatted }, (dir) => {
    unlinkSync(join(dir, "gone.go"));
  });
  const result = run(root);
  assert.notEqual(result.status, 0);
});

test("accepts a repository with no go files", (t) => {
  const root = fixture(t, { "README.md": "No go here.\n" });
  const result = run(root);
  assert.equal(result.status, 0, result.stderr);
});

test("fails when not inside a git repository", (t) => {
  const root = mkdtempSync(join(tmpdir(), "pi-worker-gofmt-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  cpSync(checker, join(root, "check-gofmt.mjs"));
  const result = spawnSync(process.execPath, [join(root, "check-gofmt.mjs")], {
    cwd: root,
    encoding: "utf8",
    env: { ...process.env, GIT_CEILING_DIRECTORIES: dirname(root) },
  });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /not a git repository/i);
});
