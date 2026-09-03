import assert from "node:assert/strict";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import process from "node:process";
import { describe, test } from "node:test";

import { nativePath, nativeTarget, runNative, runNativeCaptured } from "../lib/native.mjs";

const fixturesDir = mkdtempSync(join(tmpdir(), "pi-worker-native-test-"));
const bin = process.execPath;
const nativeModuleUrl = new URL("../lib/native.mjs", import.meta.url).href;

function writeExecutable(path, body) {
  writeFileSync(path, `#!/usr/bin/env node\n${body}\n`, { mode: 0o755 });
}

const fixture = (name) => join(fixturesDir, name);
const argsFixture = fixture("args.mjs");
const stdioFixture = fixture("stdio.mjs");
const exitFixture = fixture("exit.mjs");
const signalFixture = fixture("signal.mjs");
const optionFixture = fixture("option fixture.mjs");
const harnessFixture = fixture("harness.mjs");

function launcherFixture(name, { status = false } = {}) {
  const packageRoot = fixture(name);
  const binDir = join(packageRoot, "npm", "bin");
  const libDir = join(packageRoot, "npm", "lib");
  mkdirSync(binDir, { recursive: true });
  mkdirSync(libDir, { recursive: true });
  copyFileSync(new URL("../bin/pi-worker.mjs", import.meta.url), join(binDir, "pi-worker.mjs"));
  copyFileSync(new URL("../lib/native.mjs", import.meta.url), join(libDir, "native.mjs"));
  if (status) {
    for (const module of ["external-inspection.mjs", "skill-receipt.mjs", "skill-rules.mjs", "skill-status.mjs", "skill-tree.mjs"]) {
      copyFileSync(new URL(`../lib/${module}`, import.meta.url), join(libDir, module));
    }
  }
  return {
    packageRoot,
    launcher: join(binDir, "pi-worker.mjs"),
    binary: join(packageRoot, nativeTarget().relativePath),
  };
}

writeExecutable(
  argsFixture,
  "const args = process.argv.slice(2);\n" +
    "process.stdout.write(JSON.stringify(args));"
);

writeExecutable(
  stdioFixture,
  "let output = '';\n" +
    "process.stdin.on('data', (chunk) => {\n" +
    "  const payload = chunk.toString();\n" +
    "  process.stdout.write(`stdout:${payload}`);\n" +
    "  process.stderr.write(`stderr:${payload}`);\n" +
    "});\n" +
    "process.stdin.on('end', () => process.exit(0));\n" +
    "process.stdin.resume();"
);

writeExecutable(
  exitFixture,
  "const code = Number(process.argv[2] ?? 0);\n" +
    "process.exit(Number.isFinite(code) ? code : 0);"
);

// `ready` must be the last thing this fixture does before idling: it is the
// signal the tests wait on before killing the launcher, so anything printed
// before the handlers exist lets a forwarded signal land on the default
// disposition and kill the fixture silently. Exiting by clearing the idle
// timer instead of process.exit keeps the pending stdout write from being
// discarded, while the delay still leaves a window to observe a second
// delivery.
writeExecutable(
  signalFixture,
  "let count = 0;\n" +
    "const idle = setInterval(() => {}, 1_000_000);\n" +
    "for (const signal of ['SIGINT', 'SIGTERM']) {\n" +
    "  process.on(signal, () => {\n" +
    "    count += 1;\n" +
    "    process.stdout.write(`${signal}:${count}\\n`);\n" +
    "    setTimeout(() => clearInterval(idle), 100);\n" +
    "  });\n" +
    "}\n" +
    "process.stdout.write('ready\\n');"
);

writeExecutable(optionFixture, "process.stdout.write('native\\n');");

writeExecutable(
  harnessFixture,
  `import { runNative } from \"${nativeModuleUrl}\";\n` +
    "const printExit = process.env.PI_WORKER_PRINT_EXIT === '1';\n" +
    "const forceUnsafeOptions = process.env.PI_WORKER_FORCE_UNSAFE_OPTIONS === '1';\n" +
    "const [binary, ...args] = process.argv.slice(2);\n" +
    "const options = forceUnsafeOptions ? { shell: true, stdio: 'ignore' } : {};\n" +
    "const code = await runNative(binary, args, options);\n" +
    "if (printExit) {\n" +
    "  console.log(`exit:${code}`);\n" +
    "  process.exit(code);\n" +
    "}\n"
);

describe("native target selection", () => {
  test("maps darwin/arm64", () => {
    const target = nativeTarget("darwin", "arm64");

    assert.equal(target.platform, "darwin");
    assert.equal(target.arch, "arm64");
    assert.equal(target.relativePath, "npm/native/darwin-arm64/pi-worker");
  });

  test("maps darwin/x64", () => {
    const target = nativeTarget("darwin", "x64");

    assert.equal(target.platform, "darwin");
    assert.equal(target.arch, "x64");
    assert.equal(target.relativePath, "npm/native/darwin-x64/pi-worker");
  });

  test("maps linux/arm64", () => {
    const target = nativeTarget("linux", "arm64");

    assert.equal(target.platform, "linux");
    assert.equal(target.arch, "arm64");
    assert.equal(target.relativePath, "npm/native/linux-arm64/pi-worker");
  });

  test("maps linux/x64", () => {
    const target = nativeTarget("linux", "x64");

    assert.equal(target.platform, "linux");
    assert.equal(target.arch, "x64");
    assert.equal(target.relativePath, "npm/native/linux-x64/pi-worker");
  });

  test("resolves absolute native path", () => {
    const packageRoot = "/tmp/pi-worker";
    const absolute = nativePath(packageRoot, "linux", "x64");

    assert.equal(absolute, "/tmp/pi-worker/npm/native/linux-x64/pi-worker");
  });

  test("rejects unsupported Windows", () => {
    assert.throws(
      () => nativeTarget("win32", "x64"),
      (error) => {
        assert.equal(error.name, "UnsupportedPlatformError");
        assert.equal(error.message, "Unsupported platform/architecture: win32/x64");
        return true;
      },
      "expected windows platform to be rejected"
    );
  });

  test("rejects unsupported FreeBSD", () => {
    assert.throws(
      () => nativeTarget("freebsd", "x64"),
      (error) => {
        assert.equal(error.name, "UnsupportedPlatformError");
        assert.equal(error.message, "Unsupported platform/architecture: freebsd/x64");
        return true;
      },
      "expected freebsd platform to be rejected"
    );
  });

  test("rejects unsupported architecture", () => {
    assert.throws(
      () => nativeTarget("linux", "ia32"),
      (error) => {
        assert.equal(error.name, "UnsupportedPlatformError");
        assert.equal(error.message, "Unsupported platform/architecture: linux/ia32");
        return true;
      },
      "expected unsupported architecture to be rejected"
    );
  });

  test("rejects unknown platform", () => {
    assert.throws(
      () => nativeTarget("plan9", "x64"),
      (error) => {
        assert.equal(error.name, "UnsupportedPlatformError");
        assert.equal(error.message, "Unsupported platform/architecture: plan9/x64");
        return true;
      },
      "expected unknown platform to be rejected"
    );
  });
});

describe("launcher process behavior", () => {
	// Captured mode is reserved for the bounded skill-status projection.
	test("captures bounded native status output without changing exit code", async () => {
		const result = await runNativeCaptured(argsFixture, ["one", "two"], { maxOutputBytes: 1024 });

		assert.equal(result.code, 0);
		assert.equal(result.signal, null);
		assert.equal(result.stdout, '["one","two"]');
		assert.equal(result.stderr, "");
	});

	test("rejects oversized captured status output", async () => {
		await assert.rejects(
			runNativeCaptured(argsFixture, ["x".repeat(2048)], { maxOutputBytes: 64 }),
			/capture limit/,
		);
	});

  test("entrypoint preserves the native child exit code", () => {
    const packageRoot = join(fixturesDir, "package-exit-code");
    const binDir = join(packageRoot, "npm", "bin");
    const libDir = join(packageRoot, "npm", "lib");
    const target = nativeTarget();
    const binary = join(packageRoot, target.relativePath);

    mkdirSync(binDir, { recursive: true });
    mkdirSync(libDir, { recursive: true });
    mkdirSync(join(binary, ".."), { recursive: true });
    copyFileSync(new URL("../bin/pi-worker.mjs", import.meta.url), join(binDir, "pi-worker.mjs"));
    copyFileSync(new URL("../lib/native.mjs", import.meta.url), join(libDir, "native.mjs"));
    writeExecutable(binary, "process.exit(42);");

    const child = spawnSync(bin, [join(binDir, "pi-worker.mjs")], {
      encoding: "utf8",
    });

    assert.equal(child.status, 42, child.stderr);
    assert.equal(child.signal, null);
    assert.equal(child.stdout, "");
    assert.equal(child.stderr, "");
  });

  for (const [name, args] of [
    ["ordinary command", ["version"]],
    ["skill status human", ["skill", "status"]],
    ["skill status JSON", ["skill", "status", "--json"]],
  ]) {
    test(`missing staged binary has the documented failure shape for ${name}`, (t) => {
      if (!["darwin", "linux"].includes(process.platform) ||
        !["arm64", "x64"].includes(process.arch)) {
        t.skip("requires a supported npm platform and architecture");
        return;
      }
      const fixture = launcherFixture(`missing-binary-${name.replaceAll(" ", "-")}`, {
        status: args[0] === "skill",
      });
      assert.equal(existsSync(fixture.binary), false);

      const child = spawnSync(bin, [fixture.launcher, ...args], {
        encoding: "utf8",
      });

      assert.equal(child.status, 9);
      assert.equal(child.signal, null);
      assert.equal(child.stdout, "");
      assert.equal(child.stderr, "pi-worker: native process could not be started\n");
      assert.doesNotMatch(child.stderr, /stack|at /i);
      for (const installPath of [fixture.packageRoot, realpathSync(fixture.packageRoot)]) {
        assert.doesNotMatch(child.stderr, new RegExp(installPath.replace(/[.*+?^${}()|[\\]\\\\]/g, "\\\\$&")));
      }
      if (args[2] === "--json") assert.equal(child.stdout.trim(), "", "internal failure emits no JSON document");
    });
  }

  test("entrypoint renders one sanitized unsupported-platform diagnostic without spawning", () => {
    const packageRoot = join(fixturesDir, "package-unsupported-platform");
    const binDir = join(packageRoot, "npm", "bin");
    const libDir = join(packageRoot, "npm", "lib");
    const marker = join(packageRoot, "spawned");

    mkdirSync(binDir, { recursive: true });
    mkdirSync(libDir, { recursive: true });
    copyFileSync(new URL("../bin/pi-worker.mjs", import.meta.url), join(binDir, "pi-worker.mjs"));
    writeFileSync(join(libDir, "native.mjs"), [
      "import { writeFileSync } from \"node:fs\";",
      "export class UnsupportedPlatformError extends Error {",
      "  constructor() {",
      "    super(\"Unsupported platform/architecture: freebsd/x64\");",
      "    this.name = \"UnsupportedPlatformError\";",
      "  }",
      "}",
      "export class NativeProcessError extends Error {}",
      "export function nativeTarget() { throw new UnsupportedPlatformError(); }",
      `export function nativePath() { writeFileSync(${JSON.stringify(marker)}, \"spawned\"); }`,
      `export function runNative() { writeFileSync(${JSON.stringify(marker)}, \"spawned\"); }`,
      "",
    ].join("\n"));

    const child = spawnSync(bin, [join(binDir, "pi-worker.mjs")], {
      encoding: "utf8",
    });

    assert.equal(child.status, 1);
    assert.equal(child.signal, null);
    assert.equal(child.stdout, "");
    assert.equal(child.stderr, "Unsupported platform/architecture: freebsd/x64\n");
    assert.equal(existsSync(marker), false);
    assert.doesNotMatch(child.stderr, /stack|at /i);
  });

  test("preserves argument boundaries", () => {
    const args = ["simple", "with space", "line\nbreak", 'quoted"arg'];

    const child = spawnSync(bin, [harnessFixture, argsFixture, ...args], {
      encoding: "utf8",
    });

    assert.equal(child.status, 0);
    assert.equal(child.signal, null);
    assert.equal(child.stderr, "");
    assert.deepStrictEqual(
      JSON.parse(child.stdout),
      args
    );
  });

  test("inherits stdin/stdout/stderr", () => {
    const message = "payload with newline\nand spaces";
    const child = spawnSync(bin, [harnessFixture, stdioFixture], {
      encoding: "utf8",
      input: message,
    });

    assert.equal(child.status, 0);
    assert.equal(child.signal, null);
    assert.equal(child.stdout, `stdout:${message}`);
    assert.equal(child.stderr, `stderr:${message}`);
  });

  test("returns child exit code", () => {
    const child = spawnSync(bin, [harnessFixture, exitFixture, "42"], {
      encoding: "utf8",
      env: {
        ...process.env,
        PI_WORKER_PRINT_EXIT: "1",
      },
    });

    assert.equal(child.status, 42);
    assert.equal(child.stderr, "");
    assert.equal(child.stdout, "exit:42\n");
  });

  test("does not allow callers to override shell or inherited stdio", () => {
    const child = spawnSync(bin, [harnessFixture, optionFixture], {
      encoding: "utf8",
      env: {
        ...process.env,
        PI_WORKER_FORCE_UNSAFE_OPTIONS: "1",
        PI_WORKER_PRINT_EXIT: "1",
      },
    });

    assert.equal(child.status, 0, child.stderr);
    assert.equal(child.signal, null);
    assert.equal(child.stdout, "native\nexit:0\n");
    assert.equal(child.stderr, "");
  });

  for (const signal of ["SIGINT", "SIGTERM"]) {
    test(`forwards ${signal} once when sent to the launcher`, { timeout: 5_000 }, async (t) => {
      const child = spawn(bin, [harnessFixture, signalFixture], {
        stdio: ["ignore", "pipe", "pipe"],
      });
      let stdout = "";
      child.stdout.setEncoding("utf8");
      child.stdout.on("data", (chunk) => {
        stdout += chunk;
      });
      t.after(() => {
        if (child.exitCode === null && child.signalCode === null) {
          child.kill("SIGKILL");
        }
      });

      await new Promise((resolve) => {
        child.stdout.on("data", () => {
          if (stdout.includes("ready\n")) {
            resolve();
          }
        });
      });

      const closePromise = new Promise((resolve) => {
        child.on("close", (code, childSignal) => resolve({ code, signal: childSignal }));
      });
      assert.equal(child.kill(signal), true);
      const close = await closePromise;

      assert.equal(close.code, null);
      assert.equal(close.signal, signal);
      assert.equal(stdout, `ready\n${signal}:1\n`);
    });
  }

  const unixTest = process.platform === "win32" ? test.skip : test;

  unixTest("forwards a terminal process-group SIGINT exactly once", { timeout: 5_000 }, async (t) => {
    const child = spawn(bin, [harnessFixture, signalFixture], {
      stdio: ["ignore", "pipe", "pipe"],
      detached: true,
    });
    let stdout = "";
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      stdout += chunk;
    });
    t.after(() => {
      if (child.exitCode === null && child.signalCode === null) {
        process.kill(-child.pid, "SIGKILL");
      }
    });

    await new Promise((resolve) => {
      child.stdout.on("data", () => {
        if (stdout.includes("ready\n")) {
          resolve();
        }
      });
    });

    const closePromise = new Promise((resolve) => {
      child.on("close", (code, signal) => resolve({ code, signal }));
    });
    process.kill(-child.pid, "SIGINT");
    const close = await closePromise;

    assert.equal(close.code, null);
    assert.equal(close.signal, "SIGINT");
    assert.equal(stdout, "ready\nSIGINT:1\n");
  });

  test("does not invoke shell for execution", () => {
    const args = ["$PATH", "$(echo ignored)", "`echo ignored`", "\\n\\n"];

    const child = spawnSync(bin, [harnessFixture, argsFixture, ...args], {
      encoding: "utf8",
    });

    assert.equal(child.status, 0);
    assert.equal(child.signal, null);
    assert.equal(child.stderr, "");
    assert.deepStrictEqual(
      JSON.parse(child.stdout),
      args
    );
  });

  test("unsupported platform gives stable diagnostic", () => {
    assert.throws(
      () => nativeTarget("freebsd", "x64"),
      {
        name: "UnsupportedPlatformError",
        message: "Unsupported platform/architecture: freebsd/x64",
      },
      "expected stable unsupported-platform diagnostic"
    );
  });
});

process.on("exit", () => {
  rmSync(fixturesDir, { recursive: true, force: true });
});
