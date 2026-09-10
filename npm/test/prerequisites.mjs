import { createRequire } from "node:module";

const requireFromHere = createRequire(import.meta.url);

export const REQUIRED_MODULE = "skills/bin/cli.mjs";
export const INSTALL_COMMAND = "npm ci --ignore-scripts";

function defaultResolve(specifier) {
  return requireFromHere.resolve(specifier);
}

function firstLine(text) {
  const index = text.indexOf("\n");
  return index === -1 ? text : text.slice(0, index);
}

/**
 * Fail fast when the pinned skills CLI that the skill suite needs is missing.
 *
 * Two product surfaces resolve `skills/bin/cli.mjs` at run time
 * (`npm/lib/skill-install.mjs` and `npm/scripts/extract-skills-rules.mjs`), so
 * in a checkout without installed dependencies one skill test file throws,
 * another awaits a promise nothing ever settles, and `npm test` reports every
 * test file as interrupted instead of naming the cause. This check states what
 * is missing and how to fix it, and it never installs anything on the caller's
 * behalf.
 *
 * `resolve` is injectable so the guard test can drive the failure path.
 */
export function assertPrerequisites({ resolve = defaultResolve, specifier = REQUIRED_MODULE } = {}) {
  try {
    return resolve(specifier);
  } catch (error) {
    const code = typeof error?.code === "string" ? error.code : "unresolved";
    const detail = typeof error?.message === "string" ? `${code}: ${firstLine(error.message)}` : code;
    const failure = new Error(
      `missing test prerequisite: cannot resolve ${specifier} (${detail}). ` +
        `Install the pinned dependencies without running the postinstall script: ${INSTALL_COMMAND}.`,
      { cause: error },
    );
    // The runner evaluates this module in every per-file child process, so one
    // resolution failure would otherwise print the same stack per test file.
    failure.stack = `${String(failure.message)} [cause: ${detail}]`;
    throw failure;
  }
}

// Wired through "node --test --import ./npm/test/prerequisites.mjs" in the npm
// test script: the runner evaluates that import in each per-file child process
// before the child resolves its own test file, so the throw ends the run here
// rather than inside a test.
assertPrerequisites();
