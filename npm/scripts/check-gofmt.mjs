#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import { realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";

function fail(stderr) {
  if (stderr) process.stderr.write(stderr);
  process.exitCode = 1;
}

function main() {
  const listed = spawnSync("git", ["ls-files", "-z", "--", "*.go"], {
    cwd: process.cwd(),
    encoding: "utf8",
  });
  if (listed.error || listed.status !== 0) {
    fail(listed.stderr || `${listed.error?.message ?? "git ls-files failed"}\n`);
    return;
  }

  const files = listed.stdout.split("\0").filter((file) => file !== "");
  if (files.length === 0) return;

  const gofmt = spawnSync("gofmt", ["-l", ...files], {
    cwd: process.cwd(),
    encoding: "utf8",
  });
  if (gofmt.error || gofmt.status !== 0) {
    fail(gofmt.stderr || `${gofmt.error?.message ?? "gofmt failed"}\n`);
    return;
  }

  const unformatted = gofmt.stdout.trim();
  if (unformatted !== "") {
    console.error(unformatted);
    process.exitCode = 1;
  }
}

const invoked = process.argv[1] ? realpathSync(process.argv[1]) : "";
if (invoked === realpathSync(fileURLToPath(import.meta.url))) main();
