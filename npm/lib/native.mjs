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
