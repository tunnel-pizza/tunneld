#!/usr/bin/env node
// Launcher for the tunneld npm package.
//
// The Go binary is the whole program; this file finds the build for the host
// and hands it the process. Two places to look, in order:
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
//
// Beyond that, the launcher does three things the binary does not:
//
//   - It warns about a command line that likely does not say what was meant
//     (misread, below), before the run starts. The words go to the binary
//     unchanged either way.
//   - `-d`, as the first word, detaches: the run goes on in the background,
//     and the launcher prints its addresses and hands the console back.
//   - `-k`, as the only word, ends every run `-d` left behind, the way Ctrl-C
//     would, so the teardown runs.
//
// Both flags are read only as the first word and stripped before the binary
// sees the line. The binary refuses them by name for that reason, so a brew
// user and an npx user never mean different things by the same flag.

const { spawn, execFileSync } = require("child_process");
const fs = require("fs");
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

// How often a detaching launcher looks at the run's stdout, and how long that
// stdout has to sit still before the addresses on it count as all of them.
// The binary writes every address in one loop once the tunnel is verified,
// so the quiet is only there to not stop between two lines of it.
const POLL_MS = 50;
const SETTLE_MS = 300;

// How long -k waits for a run it has signalled to finish tearing down.
const STOP_MS = 30000;

// What the binary tells a console it has nothing to draw on, once the
// addresses are up. Relayed from a detached run it would be wrong — Ctrl-C
// here ends the launcher, which has already let go — so it is left out, and
// the line saying how to end a detached run stands where it was.
const STOP_HINT = "Press Ctrl+C to stop the tunnel...";

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
  if (fs.existsSync(local)) {
    return local;
  }

  fail(
    `no binary for ${key}: ${pkg} is not installed and ${local} does not exist.\n` +
      "  Reinstall without --omit=optional, or in a checkout run: make binaries",
  );
}

// isOrigin mirrors the Go parser's: a word that can only be an origin, a bare
// port (":3000") or a URL with a scheme. It is what ends a program's
// arguments.
function isOrigin(word) {
  if (word.startsWith(":")) {
    return /^:\d+$/.test(word);
  }
  return word.includes("://");
}

// onPath reports whether a word names a program on $PATH, the way the Go
// parser decides a bare word is a program: $PATH and the executable bit on
// Unix, PATHEXT on Windows. A relative $PATH entry (an empty one is the
// working directory) is skipped, as Go's exec.LookPath refuses what it finds
// there.
function onPath(word) {
  if (!word || word.includes("/") || word.includes(path.sep)) {
    return false;
  }
  const win = process.platform === "win32";
  let exts = [""];
  if (win) {
    exts = (process.env.PATHEXT || ".COM;.EXE;.BAT;.CMD").split(";").filter(Boolean);
    if (exts.some((ext) => word.toLowerCase().endsWith(ext.toLowerCase()))) {
      exts = [""];
    }
  }
  for (const dir of (process.env.PATH || "").split(path.delimiter)) {
    if (!path.isAbsolute(dir)) {
      continue;
    }
    for (const ext of exts) {
      try {
        const st = fs.statSync(path.join(dir, word + ext));
        if (st.isFile() && (win || st.mode & 0o111)) {
          return true;
        }
      } catch {
        // Not here; the next directory may have it.
      }
    }
  }
  return false;
}

// quote spells a word so that pasting it back into a shell gives the same
// word: as it is when nothing in it is special, single-quoted otherwise, or
// double-quoted for cmd.exe.
function quote(word) {
  if (/^[\w@%+=:,./-]+$/.test(word)) {
    return word;
  }
  if (process.platform === "win32") {
    return `"${word.replace(/"/g, '\\"')}"`;
  }
  return `'${word.replace(/'/g, "'\\''")}'`;
}

// misread looks for the command line that says one thing and runs another:
//
//   npx tunneld claude "next dev"
//
// A bare program is greedy: it takes every word after it up to one that can
// only be an origin, so that is one program, claude, with "next dev" as its
// argument. Whoever typed it almost certainly meant two, and quoted "next
// dev" because a quoted group is complete. The quotes are gone before
// anything here runs, but what they left is visible: a word with whitespace
// inside it, whose first word is itself a program. That word, in a bare
// program's arguments and not the value of a flag before it (`sh -c "npm run
// dev"` is one program on purpose), is the case.
//
// It returns the warning to print, or null. The run goes ahead as written
// either way: `claude "fix the bug"` is a program with an argument, and so is
// the rare line this guesses wrong about. The warning names the reordering
// that gives two origins, which is the bare program last.
//
// Words starting with - before the first origin are the binary's flags and
// are skipped; the value of one given as a separate word is looked at like
// any other, which only matters if it names a program.
function misread(words, prefix) {
  let i = 0;
  while (i < words.length && words[i].startsWith("-")) {
    i++;
  }
  for (; i < words.length; i++) {
    const word = words[i];
    if (isOrigin(word) || /\s/.test(word) || !onPath(word)) {
      continue;
    }
    let end = i + 1;
    while (end < words.length && !isOrigin(words[end]) && words[end] !== word) {
      end++;
    }
    for (let j = i + 1; j < end; j++) {
      const arg = words[j];
      if (!/\s/.test(arg) || words[j - 1].startsWith("-")) {
        continue;
      }
      if (!onPath(arg.trim().split(/\s+/)[0])) {
        continue;
      }
      const program = words.slice(i, j);
      const line = [
        ...words.slice(0, i),
        ...words.slice(j, end),
        ...words.slice(end),
        ...program,
      ];
      return (
        `warning: ${quote(arg)} is an argument to ${word}, not an origin of its own:\n` +
        `  a bare program takes the words after it, up to a port or a URL.\n` +
        `  Running it as written. For two origins, put the bare program last:\n` +
        `    ${[prefix, ...line.map(quote)].join(" ")}`
      );
    }
    i = end - 1;
  }
  return null;
}

// cacheDir is Go's os.UserCacheDir, so detached runs sit beside the tunnel
// specs the binary caches, in <user cache dir>/tunneld/.
function cacheDir() {
  const env = process.env;
  let base;
  switch (process.platform) {
    case "darwin":
      base = env.HOME && path.join(env.HOME, "Library", "Caches");
      break;
    case "win32":
      base = env.LocalAppData;
      break;
    default:
      base = env.XDG_CACHE_HOME || (env.HOME && path.join(env.HOME, ".cache"));
  }
  if (!base || !path.isAbsolute(base)) {
    fail("no cache directory to keep detached runs in: $HOME is not set");
  }
  return path.join(base, BINARY_NAME, "detached");
}

// Windows has no way for this process to deliver a Ctrl-C to one that is not
// on its console: process.kill there is TerminateProcess, which skips the
// teardown -k exists to run. A run -k could only cut short is not one to
// leave behind, so neither flag is offered there.
function refuseOnWindows(flag) {
  if (process.platform === "win32") {
    fail(`${flag} is not supported on Windows yet`);
  }
}

function read(file) {
  try {
    return fs.readFileSync(file, "utf8");
  } catch {
    return "";
  }
}

function remove(stem) {
  for (const ext of [".json", ".out", ".log"]) {
    fs.rmSync(stem + ext, { force: true });
  }
}

function signum(signal) {
  return 128 + (constants.signals[signal] ?? 0);
}

// run is the plain launch: the binary in the foreground, on this terminal.
function run(bin, args) {
  const child = spawn(bin, args, { stdio: "inherit" });

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
    process.exit(signal ? signum(signal) : (code ?? 1));
  });
}

// detach starts the binary in a session of its own, with its stdout and
// stderr in files under cacheDir, and waits for the addresses. Once stdout
// carries them and has gone quiet, they are printed to this stdout and what
// the run said on stderr so far (the banner, which origin each address
// reaches) to this stderr, the same two streams a foreground run uses. Then
// this process exits and the run goes on.
//
// A run that ends before that — a bad origin, --help, a mint that fails —
// has its output relayed the same way and its exit status passed on, so a
// detached run that could not start fails like a foreground one. Ctrl-C while
// waiting ends the run: nothing has been handed back yet, so nothing should
// be left behind.
//
// Each run leaves <pid>.json (what -k reads), <pid>.out and <pid>.log. The
// log keeps growing for as long as the run does, and is where to look when a
// detached run misbehaves.
function detach(bin, args) {
  const dir = cacheDir();
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  const pending = path.join(dir, `pending-${process.pid}`);
  const out = fs.openSync(`${pending}.out`, "w", 0o600);
  const log = fs.openSync(`${pending}.log`, "w", 0o600);
  const child = spawn(bin, args, { detached: true, stdio: ["ignore", out, log] });
  fs.closeSync(out);
  fs.closeSync(log);

  let stem = pending;
  let interrupted = null;
  let timer = null;
  const signals = ["SIGINT", "SIGTERM", "SIGHUP"];

  // Leaves by letting the event loop run dry rather than process.exit, which
  // can cut short a write to a pipe that is still draining.
  const finish = (status) => {
    clearInterval(timer);
    for (const signal of signals) {
      process.removeAllListeners(signal);
    }
    process.exitCode = status;
  };

  child.on("error", (err) => {
    remove(pending);
    fail(`cannot run ${bin}: ${err.message}`);
  });

  child.on("spawn", () => {
    stem = path.join(dir, String(child.pid));
    fs.renameSync(`${pending}.out`, `${stem}.out`);
    fs.renameSync(`${pending}.log`, `${stem}.log`);
    const record = { pid: child.pid, bin, args, cwd: process.cwd(), started: new Date().toISOString() };
    fs.writeFileSync(`${stem}.json.tmp`, JSON.stringify(record) + "\n", { mode: 0o600 });
    fs.renameSync(`${stem}.json.tmp`, `${stem}.json`);

    let seen = "";
    let still = 0;
    timer = setInterval(() => {
      const now = read(`${stem}.out`);
      if (now !== seen || !now.endsWith("\n")) {
        seen = now;
        still = 0;
        return;
      }
      still += POLL_MS;
      if (still < SETTLE_MS) {
        return;
      }
      child.removeAllListeners("exit");
      child.unref();
      finish(0);
      process.stderr.write(
        read(`${stem}.log`)
          .split("\n")
          .filter((line) => line !== STOP_HINT)
          .join("\n"),
      );
      process.stdout.write(now);
      console.error(
        `${WRAPPER_NAME}: detached as pid ${child.pid}; its log is ${stem}.log\n` +
          `  npx ${WRAPPER_NAME} -k ends it`,
      );
    }, POLL_MS);
  });

  for (const signal of signals) {
    process.on(signal, () => {
      interrupted = signal;
      child.kill("SIGINT");
    });
  }

  child.on("exit", (code, signal) => {
    finish(interrupted ? signum(interrupted) : signal ? signum(signal) : (code ?? 1));
    if (!interrupted) {
      process.stdout.write(read(`${stem}.out`));
      process.stderr.write(read(`${stem}.log`));
    }
    remove(stem);
  });
}

// commandLine is what a process was started as, for telling a run -d left
// behind from an unrelated process that has since been given its pid.
function commandLine(pid) {
  try {
    if (process.platform === "linux") {
      return fs.readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0").join(" ");
    }
    return execFileSync("ps", ["-o", "command=", "-p", String(pid)], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    });
  } catch {
    return "";
  }
}

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return err.code === "EPERM";
  }
}

// kill ends every run -d left behind with SIGINT, which is what Ctrl-C sends
// a foreground run, so each one tears down the way it would there: the
// programs its origins started, the attach servers, the tunnel. It waits for
// them to finish, and names each one it stopped with its addresses.
//
// A record whose pid is gone, or now belongs to some other program, is a run
// that ended on its own; it is cleared without a word. One that is still
// there after STOP_MS is named and the exit status is 1: it was asked, and
// asking twice changes nothing, since the binary swallows repeats.
function kill() {
  const dir = cacheDir();
  let names = [];
  try {
    names = fs.readdirSync(dir);
  } catch (err) {
    if (err.code !== "ENOENT") {
      fail(`cannot read ${dir}: ${err.message}`);
    }
  }

  const runs = [];
  for (const name of names.filter((n) => /^\d+\.json$/.test(n))) {
    const stem = path.join(dir, path.basename(name, ".json"));
    let record;
    try {
      record = JSON.parse(read(`${stem}.json`));
    } catch {
      remove(stem);
      continue;
    }
    if (!alive(record.pid) || !commandLine(record.pid).includes(record.bin)) {
      remove(stem);
      continue;
    }
    try {
      process.kill(record.pid, "SIGINT");
    } catch (err) {
      console.error(`${WRAPPER_NAME}: cannot stop pid ${record.pid}: ${err.message}`);
      continue;
    }
    runs.push({ stem, pid: record.pid, addresses: read(`${stem}.out`).trim().split("\n").join(" ") });
  }

  if (runs.length === 0) {
    console.error(`${WRAPPER_NAME}: no detached runs`);
    return;
  }

  const deadline = Date.now() + STOP_MS;
  const timer = setInterval(() => {
    for (const r of runs.filter((r) => !r.done && !alive(r.pid))) {
      r.done = true;
      remove(r.stem);
      console.error(`${WRAPPER_NAME}: stopped pid ${r.pid} ${r.addresses}`.trimEnd());
    }
    const left = runs.filter((r) => !r.done);
    if (left.length === 0) {
      clearInterval(timer);
      return;
    }
    if (Date.now() > deadline) {
      clearInterval(timer);
      for (const r of left) {
        console.error(`${WRAPPER_NAME}: pid ${r.pid} is still tearing down after ${STOP_MS / 1000}s`);
      }
      process.exitCode = 1;
    }
  }, POLL_MS);
}

function main() {
  const argv = process.argv.slice(2);

  if (argv[0] === "-k") {
    refuseOnWindows("-k");
    if (argv.length > 1) {
      fail("-k takes no arguments: it ends every detached run");
    }
    kill();
    return;
  }

  const detaching = argv[0] === "-d";
  const args = detaching ? argv.slice(1) : argv;
  if (detaching) {
    refuseOnWindows("-d");
  }

  const warning = misread(args, detaching ? `npx ${WRAPPER_NAME} -d` : `npx ${WRAPPER_NAME}`);
  if (warning) {
    console.error(`${WRAPPER_NAME}: ${warning}`);
  }

  const bin = binaryPath();
  if (detaching) {
    detach(bin, args);
  } else {
    run(bin, args);
  }
}

if (require.main === module) {
  main();
}

module.exports = { isOrigin, misread, onPath, quote };
