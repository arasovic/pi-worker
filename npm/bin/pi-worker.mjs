#!/usr/bin/env node
import { fileURLToPath } from "node:url";
import process from "node:process";

import { nativePath, nativeTarget, runNative } from "../lib/native.mjs";

const packageRoot = fileURLToPath(new URL("../..", import.meta.url));
const target = nativeTarget();
const binary = nativePath(packageRoot, target.platform, target.arch);

process.exitCode = await runNative(binary, process.argv.slice(2));
