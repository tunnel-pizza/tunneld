#!/usr/bin/env node
// Launcher for the tunneld npm package.
//
// The Go binary is the whole program; this file only finds the build for the
// host and hands it the process. Two places to look, in order:
//
//   1. A platform package, @tunnel-pizza/tunneld-<platform>-<arch>: one per
//      platform, each holding a single binary and declaring os/cpu so npm
//      installs only the one that matches the host. They are listed as
//      optionalDependencies of this package, pinned to the same version.
//   2. dist/tunneld-<platform>-<arch> beside this package: what `make
//      binaries` writes, so a checkout runs with `npx .`, and what a package
//      that ships every binary in dist/ carries.
//
// The key is process.platform + "-" + process.arch, which is also how the
// Makefile names its targets; win32 adds .exe. An x64 Node under Rosetta 2
// reports x64 and gets the darwin-x64 binary, which is fine: the Go build has
// no AVX requirement, so it runs translated.
//
// The binary is spawned asynchronously rather than with spawnSync so that this
// process outlives a Ctrl-C: the terminal delivers SIGINT to both, the tunnel
// tears down, and only then does the prompt come back carrying the binary's
// exit status. A signal aimed at this process alone (kill, a supervisor's
// SIGTERM) is forwarded, so the tunnel never outlives its launcher. The
// duplicate a Ctrl-C produces is harmless: tunneld's signal context swallows
// repeats until it exits.

const { spawn } = require("child_process");
const { existsSync } = require("fs");
const { constants } = require("os");
const path = require("path");

const PACKAGE_PREFIX = "@tunnel-pizza/tunneld";
const BINARY_NAME = "tunneld";
const WRAPPER_NAME = require("../package.json").name;

// Keep in step with PLATFORMS in the Makefile.
const PLATFORMS = [
  "darwin-arm64",
  "darwin-x64",
  "linux-arm64",
  "linux-x64",
  "win32-arm64",
  "win32-x64",
];

function fail(message) {
  console.error(`${WRAPPER_NAME}: ${message}`);
  process.exit(1);
}

function binaryPath() {
  const key = `${process.platform}-${process.arch}`;
  if (!PLATFORMS.includes(key)) {
    fail(
      `unsupported platform ${process.platform} ${process.arch}; supported: ${PLATFORMS.join(", ")}`,
    );
  }
  const exe = process.platform === "win32" ? ".exe" : "";
  const pkg = `${PACKAGE_PREFIX}-${key}`;

  try {
    const dir = path.dirname(require.resolve(`${pkg}/package.json`));
    return path.join(dir, `${BINARY_NAME}${exe}`);
  } catch {
    // Not installed: --omit=optional, a lockfile written on another platform,
    // or a checkout. Fall through to the local build.
  }

  const local = path.join(__dirname, "..", "dist", `${BINARY_NAME}-${key}${exe}`);
  if (existsSync(local)) {
    return local;
  }

  fail(
    `no binary for ${key}: ${pkg} is not installed and ${local} does not exist.\n` +
      "  Reinstall without --omit=optional, or in a checkout run: make binaries",
  );
}

function main() {
  const bin = binaryPath();
  const child = spawn(bin, process.argv.slice(2), { stdio: "inherit" });

  // Listening keeps this process alive until the child is done. On POSIX the
  // signal is also forwarded, for the case where only this pid was targeted.
  // On Windows the console has already delivered Ctrl-C to the child, and
  // child.kill() there is TerminateProcess, which would cut teardown short,
  // so it only waits.
  for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(signal, () => {
      if (process.platform !== "win32") {
        child.kill(signal);
      }
    });
  }

  child.on("error", (err) => fail(`cannot run ${bin}: ${err.message}`));
  child.on("exit", (code, signal) => {
    // Node ignores some signals itself (SIGPIPE), so re-raising is
    // unreliable; report a signal death the way a shell does, 128+signum.
    process.exit(signal ? 128 + (constants.signals[signal] ?? 0) : (code ?? 1));
  });
}

main();
