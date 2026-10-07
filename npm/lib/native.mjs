import { resolve } from "node:path";
import { spawn } from "node:child_process";
import { constants } from "node:os";
import process from "node:process";

export class UnsupportedPlatformError extends Error {
  constructor(platform, arch) {
    super(`Unsupported platform/architecture: ${platform}/${arch}; Pi Worker supports macOS and Linux on arm64 and x64`);
    this.name = "UnsupportedPlatformError";
  }
}

export class NativeProcessError extends Error {
  constructor(message = "native process could not be started") {
    super(message);
    this.name = "NativeProcessError";
  }
}

function nativeProcessError(error, spawned) {
  return spawned ? error : new NativeProcessError();
}

// A signal that ends the launcher makes the return value unused; a signal Node
// ignores (SIGPIPE) leaves the launcher alive, so the caller settles with the
// status a shell reports for that signal.
function reRaiseSignal(signal) {
  process.kill(process.pid, signal);
  return 128 + constants.signals[signal];
}

export function nativeTarget(platform = process.platform, arch = process.arch) {
  const combos = {
    darwin: {
      arm64: "darwin-arm64",
      x64: "darwin-x64",
    },
    linux: {
      arm64: "linux-arm64",
      x64: "linux-x64",
    },
  };

  if (!Object.hasOwn(combos, platform) || !Object.hasOwn(combos[platform], arch)) {
    throw new UnsupportedPlatformError(platform, arch);
  }

  return {
    platform,
    arch,
    relativePath: `npm/native/${combos[platform][arch]}/pi-worker`,
  };
}

export function nativePath(packageRoot, platform = process.platform, arch = process.arch) {
  const target = nativeTarget(platform, arch);
  return resolve(packageRoot, target.relativePath);
}

export function runNative(binary, args, options = {}) {
  return new Promise((resolve, reject) => {
    let child;
    try {
      child = spawn(binary, args, {
        ...options,
        detached: true,
        shell: false,
        stdio: "inherit",
      });
    } catch {
      reject(new NativeProcessError());
      return;
    }

    let spawned = false;
    let signal = null;

    child.once("spawn", () => {
      spawned = true;
    });

    const handleSignal = (signalName) => {
      if (signal) {
        return;
      }

      signal = signalName;
      child.kill(signalName);
    };

    process.on("SIGINT", handleSignal);
    process.on("SIGTERM", handleSignal);

    const cleanup = () => {
      process.off("SIGINT", handleSignal);
      process.off("SIGTERM", handleSignal);
    };

    child.once("error", (error) => {
      cleanup();
      reject(nativeProcessError(error, spawned));
    });

    child.once("close", (code, childSignal) => {
      cleanup();

      if (childSignal) {
        resolve(reRaiseSignal(childSignal));
        return;
      }

      resolve(code);
    });
  });
}

export function runNativeCaptured(binary, args, options = {}) {
  const maxOutputBytes = options.maxOutputBytes ?? 1024 * 1024;
  if (!Number.isSafeInteger(maxOutputBytes) || maxOutputBytes <= 0) {
    return Promise.reject(new TypeError("maxOutputBytes must be a positive safe integer"));
  }

  return new Promise((resolve, reject) => {
    let child;
    try {
      child = spawn(binary, args, {
        detached: true,
        shell: false,
        stdio: ["inherit", "pipe", "pipe"],
      });
    } catch {
      reject(new NativeProcessError());
      return;
    }
    const stdout = [];
    const stderr = [];
    let spawned = false;
    let stdoutBytes = 0;
    let stderrBytes = 0;
    let signal = null;
    let captureError = null;
    let settled = false;

    const handleSignal = (signalName) => {
      if (signal) return;
      signal = signalName;
      child.kill(signalName);
    };
    process.on("SIGINT", handleSignal);
    process.on("SIGTERM", handleSignal);

    const cleanup = () => {
      process.off("SIGINT", handleSignal);
      process.off("SIGTERM", handleSignal);
    };

    child.once("spawn", () => {
      spawned = true;
    });

    const capture = (chunks, chunk, streamName) => {
      if (captureError) return;
      const data = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      const next = (streamName === "stdout" ? stdoutBytes : stderrBytes) + data.length;
      if (next > maxOutputBytes) {
        captureError = new NativeProcessError(`${streamName} exceeded the native capture limit`);
        child.kill("SIGKILL");
        return;
      }
      if (streamName === "stdout") stdoutBytes = next;
      else stderrBytes = next;
      chunks.push(data);
    };

    child.stdout.on("data", (chunk) => capture(stdout, chunk, "stdout"));
    child.stderr.on("data", (chunk) => capture(stderr, chunk, "stderr"));
    child.once("error", (error) => {
      if (settled) return;
      settled = true;
      cleanup();
      reject(nativeProcessError(error, spawned));
    });
    child.once("close", (code, childSignal) => {
      if (settled) return;
      settled = true;
      cleanup();
      if (captureError) {
        reject(captureError);
        return;
      }
      if (childSignal) {
        code = reRaiseSignal(childSignal);
      }
      resolve({
        code,
        signal: null,
        stdout: Buffer.concat(stdout, stdoutBytes).toString("utf8"),
        stderr: Buffer.concat(stderr, stderrBytes).toString("utf8"),
      });
    });
  });
}
