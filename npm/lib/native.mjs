import { resolve } from "node:path";
import { spawn } from "node:child_process";
import process from "node:process";

export class UnsupportedPlatformError extends Error {
  constructor(platform, arch) {
    super(`Unsupported platform/architecture: ${platform}/${arch}`);
    this.name = "UnsupportedPlatformError";
  }
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
    const child = spawn(binary, args, {
      ...options,
      detached: process.platform !== "win32",
      shell: false,
      stdio: "inherit",
      windowsHide: true,
    });

    let signal = null;

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
      reject(error);
    });

    child.once("close", (code, childSignal) => {
      cleanup();

      const exitSignal = childSignal ?? signal;
      if (exitSignal) {
        process.kill(process.pid, exitSignal);
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
    const child = spawn(binary, args, {
      detached: process.platform !== "win32",
      shell: false,
      stdio: ["inherit", "pipe", "pipe"],
      windowsHide: true,
    });
    const stdout = [];
    const stderr = [];
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
    const capture = (chunks, chunk, streamName) => {
      if (captureError) return;
      const data = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      const next = (streamName === "stdout" ? stdoutBytes : stderrBytes) + data.length;
      if (next > maxOutputBytes) {
        captureError = new Error(`${streamName} exceeded the native capture limit`);
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
      reject(error);
    });
    child.once("close", (code, childSignal) => {
      if (settled) return;
      settled = true;
      cleanup();
      if (captureError) {
        reject(captureError);
        return;
      }
      const exitSignal = childSignal ?? signal;
      if (exitSignal) {
        process.kill(process.pid, exitSignal);
        return;
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
