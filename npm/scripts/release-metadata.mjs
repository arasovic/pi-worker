#!/usr/bin/env node

import { appendFileSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

const VERSION_PATTERN = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;

function parseArguments(args) {
  const options = {
    packageJson: resolve("package.json"),
  };
  for (let index = 0; index < args.length; index += 2) {
    const flag = args[index];
    const value = args[index + 1];
    if (!value) throw new Error(`missing value for ${flag ?? "argument"}`);
    if (flag === "--tag") options.tag = value;
    else if (flag === "--ref-type") options.refType = value;
    else if (flag === "--package-json") options.packageJson = resolve(value);
    else if (flag === "--github-output") options.githubOutput = resolve(value);
    else throw new Error(`unknown argument: ${flag}`);
  }
  if (!options.tag || !options.refType || !options.githubOutput) {
    throw new Error("usage: release-metadata --tag <tag> --ref-type <type> --github-output <path> [--package-json <path>]");
  }
  return options;
}

function resolveReleaseMetadata(tag, refType, manifest) {
  if (refType !== "tag") throw new Error("release ref must be a tag");
  if (manifest?.name !== "pi-worker") throw new Error("package name must be pi-worker");
  const version = manifest?.version;
  if (typeof version !== "string" || version.length > 128 || !VERSION_PATTERN.test(version)) {
    throw new Error("package version must be a stable semantic version");
  }
  if (tag !== `v${version}`) throw new Error("release tag does not match package version");
  return Object.freeze({
    tag,
    version,
    releaseVersion: tag,
    npmTarball: `pi-worker-${version}.tgz`,
    archivePrefix: `pi-worker_${tag}_`,
  });
}

function main() {
  const options = parseArguments(process.argv.slice(2));
  const manifest = JSON.parse(readFileSync(options.packageJson, "utf8"));
  const metadata = resolveReleaseMetadata(options.tag, options.refType, manifest);
  appendFileSync(options.githubOutput, [
    `tag=${metadata.tag}`,
    `version=${metadata.version}`,
    `release_version=${metadata.releaseVersion}`,
    `npm_tarball=${metadata.npmTarball}`,
    `archive_prefix=${metadata.archivePrefix}`,
    "",
  ].join("\n"));
  process.stdout.write(`release ${metadata.tag} matches package.json ${metadata.version}\n`);
}

try {
  main();
} catch (error) {
  process.stderr.write(`${error instanceof Error ? error.message : "release metadata failed"}\n`);
  process.exitCode = 1;
}
