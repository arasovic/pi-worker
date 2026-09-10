import assert from "node:assert/strict";
import { test } from "node:test";

import { assertPrerequisites, INSTALL_COMMAND, REQUIRED_MODULE } from "./prerequisites.mjs";

function escapeForRegExp(text) {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

test("names the unresolvable module and the install remedy when resolution fails", () => {
  let requested;
  assert.throws(
    () =>
      assertPrerequisites({
        resolve: (specifier) => {
          requested = specifier;
          throw Object.assign(new Error(`Cannot find module '${specifier}'`), { code: "MODULE_NOT_FOUND" });
        },
      }),
    (error) => {
      assert.match(error.message, new RegExp(escapeForRegExp(REQUIRED_MODULE)));
      assert.match(error.message, new RegExp(escapeForRegExp(INSTALL_COMMAND)));
      return true;
    },
  );
  assert.equal(requested, REQUIRED_MODULE);
});
