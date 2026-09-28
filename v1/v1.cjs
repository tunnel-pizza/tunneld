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
// Beyond that, the launcher does what the binary does not:
//
//   - It refuses a command line that could mean two things (misread, below),
//     before the run starts, and names the quoting for each.
//   - `-d`, as the first word, detaches: the run goes on in the background,
//     and once it signals that its addresses are out the launcher hands the
//     console back.
//   - `-k`, as the only word, ends every run on the machine, detached or
//     not, the way Ctrl-C would, so the teardown runs. The binary registers
//     each run as a file it holds open beside its cached spec
//     (v1alpha1/cache), and -k ends whatever holds one.
//   - `-kd`, or `-dk`, as the first word, is both: every run ended, then this
//     one started detached.
//
// The flags are read only as the first word and stripped before the binary
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

// How long -k waits for a run it has signalled to finish tearing down, and
// how often it looks.
const STOP_MS = 30000;
const POLL_MS = 50;

// What the binary reads to know a launcher is waiting on it; see detach.
const NOTIFY_ENV = "TUNNELD_NOTIFY_PID";

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
      "  Reinstall without --omit=optional. In a checkout, npm run dev -- <args> builds it and runs,\n" +
      "  or make host builds it for npx .",
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

// misread looks for the command line that could mean two things:
//
//   npx tunneld claude "next dev"
//
// A bare program is greedy: it takes every word after it up to one that can
// only be an origin, so as written that is one program, claude, with "next
// dev" as its argument. Whoever typed it more likely meant two, and quoted
// "next dev" because a quoted group is complete. The quotes are gone before
// anything here runs, but what they left is visible: a word with whitespace
// inside it, whose first word is itself a program. That word, in a bare
// program's arguments and not the value of a flag before it (`sh -c "npm run
// dev"` is one program on purpose), is the case.
//
// The launcher refuses such a line rather than guessing, since a guess either
// way starts somebody a program they did not ask for, in public. It returns
// null, or the two lines that each say one thing: `two`, the bare program
// moved last so the quoted word is an origin of its own, and `one`, the
// program quoted together with its arguments, which is a word with
// whitespace in it and so never a bare program. Neither is itself misread,
// so whichever gets pasted back runs. A prompt (`claude "fix the bug"`) is
// not refused, because fix is not a program.
//
// Words starting with - before the first origin are the binary's flags and
// are skipped; the value of one given as a separate word is looked at like
// any other, which only matters if it names a program.
function misread(words) {
  const found = locate(words);
  if (!found) {
    return null;
  }
  const { first, i, j, end } = found;
  const word = words[i];
  const arg = words[j];

  // Each reading goes where the program stood. If what comes before it is a
  // run of the same program — `bash bash "next dev"` — that run would swallow
  // the reading in turn, so there it goes ahead of every origin instead: a
  // quoted group is complete, so nothing it precedes is affected. Only there,
  // since this cannot tell `--log-level debug`'s value from an origin, and
  // moving ahead of one would split a flag.
  const place = (reading, rest) => {
    const here = [...words.slice(0, i), ...reading, ...words.slice(end), ...rest];
    if (locate(here) === null) {
      return here;
    }
    return [...words.slice(0, first), ...reading, ...words.slice(first, i), ...words.slice(end), ...rest];
  };
  return {
    program: word,
    arg,
    two: place([arg], [word, ...words.slice(i + 1, j), ...words.slice(j + 1, end)]),
    one: place([group(words.slice(i, end))], []),
  };
}

// locate finds what misread describes: the first positional word, the bare
// program at i, the argument at j that could be an origin, and the end of the
// program's run. Null when there is none.
function locate(words) {
  let first = 0;
  while (first < words.length && words[first].startsWith("-")) {
    first++;
  }
  for (let i = first; i < words.length; i++) {
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
      if (onPath(arg.trim().split(/\s+/)[0])) {
        return { first, i, j, end };
      }
    }
    i = end - 1;
  }
  return null;
}

// group spells a program and its arguments as the one word the Go parser
// splits back into them: each word with anything special in it quoted with
// the quote quote() will not wrap the whole in, so the two nest.
function group(words) {
  return words
    .map((w) => {
      if (/^[\w@%+=:,./-]+$/.test(w)) {
        return w;
      }
      if (process.platform === "win32") {
        return `'${w}'`;
      }
      return `"${w.replace(/["\\$`]/g, "\\$&")}"`;
    })
    .join(" ");
}

// ambiguous is the refusal for a line misread found: both readings, each
// ready to paste back.
function ambiguous(reading, prefix) {
  const line = (words) => [prefix, ...words.map(quote)].join(" ");
  const two = line(reading.two);
  const one = line(reading.one);
  const width = Math.max(two.length, one.length);
  return (
    `ambiguous: ${quote(reading.arg)} could be ${reading.program}'s argument or an origin of its own.\n` +
    `  Say which with quotes:\n` +
    `    ${two.padEnd(width)}  two origins\n` +
    `    ${one.padEnd(width)}  one origin`
  );
}

// cacheDir is <user cache dir>/tunneld, the binary's own: Go's
// os.UserCacheDir, spelled out. Each run registers there as <key>.pid, beside
// its spec, and a detached one logs to <key>.log.
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
    fail("no cache directory: $HOME is not set");
  }
  return path.join(base, BINARY_NAME);
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

// detach starts the binary in a session of its own, on this process's own
// streams, and waits to be told it is up. The banner, the addresses and the
// origin each reaches go straight to the caller, the same two streams a
// foreground run uses.
//
// The telling is a signal. The binary is given this process's pid in
// NOTIFY_ENV, and once its addresses are out it moves its stdout and stderr
// to <key>.log beside its spec, says so on stderr, and sends SIGUSR2 to its
// parent when that is the pid it was given — and to nobody otherwise, since
// SIGUSR2 ends a process that has not asked for it. Moving the streams first
// is what lets the caller go: once this exits, nothing holds them, so
// `$(npx tunneld -d …)` returns, and nothing lands on a prompt later.
// SIGUSR2 rather than SIGUSR1, which Node keeps for its debugger.
//
// A run that ends first — a bad origin, --help, a mint that fails — has
// already said so on the caller's streams, and its exit status is passed on,
// so a detached run that could not start fails like a foreground one. Ctrl-C
// while waiting ends the run: it is in a session of its own, where the
// terminal's Ctrl-C does not reach, and nothing has been handed back yet.
function detach(bin, args) {
  const signals = ["SIGINT", "SIGTERM", "SIGHUP"];
  let interrupted = null;
  let child = null;

  // Leaves by letting the event loop run dry rather than process.exit, which
  // can cut short a write to a pipe that is still draining.
  const finish = (status) => {
    for (const signal of [...signals, "SIGUSR2"]) {
      process.removeAllListeners(signal);
    }
    process.exitCode = status;
  };

  // Listening before the spawn: unheard, SIGUSR2 would end this process.
  process.on("SIGUSR2", () => {
    child.removeAllListeners("exit");
    child.unref();
    finish(0);
    process.stderr.write(summary(child.pid));
  });
  for (const signal of signals) {
    process.on(signal, () => {
      interrupted = signal;
      child.kill("SIGINT");
    });
  }

  child = spawn(bin, args, {
    detached: true,
    stdio: ["ignore", "inherit", "inherit"],
    env: { ...process.env, [NOTIFY_ENV]: String(process.pid) },
  });
  child.on("error", (err) => fail(`cannot run ${bin}: ${err.message}`));
  child.on("exit", (code, signal) => {
    finish(interrupted ? signum(interrupted) : signal ? signum(signal) : (code ?? 1));
  });
}

// paint wraps text in an ANSI style when stderr is a terminal that wants
// one: not when it is a file or a pipe, not under NO_COLOR, not on a dumb
// terminal.
const COLOR = process.stderr.isTTY && !("NO_COLOR" in process.env) && process.env.TERM !== "dumb";
const paint = (code) => (text) => (COLOR ? `\x1b[${code}m${text}\x1b[0m` : text);
const bold = paint("1");
const dim = paint("2");
const cyan = paint("36");

// home spells a path under $HOME the short way.
function home(p) {
  const h = process.env.HOME;
  return h && p.startsWith(h + path.sep) ? "~" + p.slice(h.length) : p;
}

// envFile reads the NAME='value' lines the binary caches a run's spec and
// settings in. The spec line is among them and is never shown.
function envFile(text) {
  const out = {};
  for (const line of text.split("\n")) {
    const m = line.match(/^([A-Z_][A-Z0-9_]*)='(.*)'$/);
    if (m) {
      out[m[1]] = m[2];
    }
  }
  return out;
}

// label is an origin as a person reads it: a program as its path and
// arguments rather than the exec:// URL carrying them.
function label(origin) {
  try {
    const u = new URL(origin);
    if (u.protocol === "exec:") {
      return [decodeURIComponent(u.pathname), ...u.searchParams.getAll("arg")].map(quote).join(" ");
    }
  } catch {
    // Not a URL this can read; shown as it is.
  }
  return origin;
}

// describe names an origin by what somebody opening it gets: a terminal for
// a program or a container, and for a service, what is serving where.
function describe(origin) {
  try {
    const u = new URL(origin);
    if (u.protocol === "exec:") {
      return `a terminal running ${label(origin)}`;
    }
    if (u.protocol === "attach:") {
      return `the terminal of the ${decodeURIComponent(u.pathname.slice(1))} container`;
    }
    if (/^(http|ws)/.test(u.protocol)) {
      return `what's serving on ${u.host}${u.pathname === "/" ? "" : u.pathname}`;
    }
  } catch {
    // Not a URL this can read; named as it is.
  }
  return origin;
}

// and joins names the way a sentence does: a, b and c.
function and(names) {
  return names.length <= 1 ? names.join("") : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
}

// wrap breaks prose into lines of at most width visible columns, indented,
// never inside a word, so a URL stays whole to be clicked. Styling is applied
// by the caller's style function to each word, after measuring.
function wrap(words, width, indent) {
  const lines = [];
  let line = [];
  let used = 0;
  for (const [text, style] of words) {
    if (line.length > 0 && used + 1 + text.length > width) {
      lines.push(indent + line.join(" "));
      line = [];
      used = 0;
    }
    used += (line.length > 0 ? 1 : 0) + text.length;
    line.push(style(text));
  }
  if (line.length > 0) {
    lines.push(indent + line.join(" "));
  }
  return lines;
}

// prose is what the run shares, said as a sentence: each origin by what it
// gives, and where to open it.
function prose(host, origins, multiview) {
  const plain = (s) => s.split(" ").map((w) => [w, (x) => x]);
  const link = (url, tail = "") => [[url + tail, (x) => cyan(url) + tail]];
  const up = (s) => s.charAt(0).toUpperCase() + s.slice(1);
  const names = up(and(origins.map(describe)));
  const base = `https://${host}/`;
  let words;
  if (origins.length <= 1) {
    words = [...plain(`${names} is now available at`), ...link(base, "."), ...plain("Open it in any web browser.")];
  } else if (multiview) {
    words = [
      ...plain(`${names} are now available side by side at`),
      ...link(base, ","),
      ...plain("in any web browser, and each at an address of its own below."),
    ];
  } else {
    words = plain(`${names} are now available in any web browser, each at an address of its own below.`);
  }
  return wrap(words, 76, "  ");
}

// summary is what a detached run is handed back with, once it has signalled:
// where it answers and what each address reaches, where it runs from, its
// pid and its log.
//
// Read from the files the run keeps beside its spec, which it wrote before
// signalling: <key>.pid, to find the key by the pid, and <key>.env, the
// settings the run settled on and the hostname it got. A run under
// --no-cache writes no .env, and gets what the pid file alone can say; its
// addresses are on stdout above either way.
function summary(pid) {
  const dir = cacheDir();
  let key = null;
  try {
    key = fs
      .readdirSync(dir)
      .filter((n) => n.endsWith(".pid"))
      .map((n) => path.basename(n, ".pid"))
      .find((k) => read(path.join(dir, `${k}.pid`)).trim() === String(pid));
  } catch {
    // No cache directory: nothing more to say than the pid.
  }
  const env = key ? envFile(read(path.join(dir, `${key}.env`))) : {};

  const lines = ["", `${bold("🍕 tunneld is now running in the background")}`, ""];
  const host = env.LIBTUNNEL_HOSTNAME;
  if (!host) {
    lines.push("  What it shares is available in any web browser, at the address above.", "");
  } else {
    const base = `https://${host}/`;
    const origins = (env.TUNNELD_ORIGINS || "").split(",").filter(Boolean);
    lines.push(...prose(host, origins, env.TUNNELD_MULTIVIEW === "true"), "");
    if (origins.length > 1) {
      const width = `${base}?${origins.length - 1}`.length;
      if (env.TUNNELD_MULTIVIEW === "true") {
        lines.push(`  ${cyan(base.padEnd(width))}  ${dim("→")} all ${origins.length}, side by side`);
      }
      origins.forEach((o, i) => lines.push(`  ${cyan(`${base}?${i}`.padEnd(width))}  ${dim("→")} ${label(o)}`));
      lines.push("");
    }
  }

  const row = (name, value) => lines.push(`  ${dim(name.padEnd(8))} ${value}`);
  if (env.PWD) {
    row("from", home(env.PWD));
  }
  row("pid", String(pid));
  if (key) {
    row("log", home(path.join(dir, `${key}.log`)));
  }
  lines.push("");
  return lines.join("\n") + "\n";
}

// holders is every process holding file open. A run keeps its <key>.pid open
// for as long as it runs, and the kernel closes it however the process ends,
// so this is the live runs registered there and nothing else — not a process
// that has since been given a dead run's pid. Two runs of the same thing at
// once share the file, so it can be more than one. Linux answers from /proc,
// where only this user's processes can be read, which are the only ones this
// could signal anyway; everywhere else lsof does, which macOS always has.
function holders(file) {
  try {
    if (process.platform === "linux") {
      const target = fs.realpathSync(file);
      const found = [];
      for (const pid of fs.readdirSync("/proc").filter((n) => /^\d+$/.test(n))) {
        let fds;
        try {
          fds = fs.readdirSync(`/proc/${pid}/fd`);
        } catch {
          continue; // Not ours, or gone.
        }
        for (const fd of fds) {
          try {
            if (fs.readlinkSync(`/proc/${pid}/fd/${fd}`) === target) {
              found.push(Number(pid));
              break;
            }
          } catch {
            // Closed while being looked at.
          }
        }
      }
      return found;
    }
    const out = execFileSync("lsof", ["-t", "--", file], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    });
    return out.split("\n").filter(Boolean).map(Number);
  } catch {
    return [];
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

// kill ends every registered run with SIGINT, which is what Ctrl-C sends a
// foreground run, so each one tears down the way it would there: the
// programs its origins started, the attach servers, the tunnel. Detached or
// not, npx or brew: whatever holds a <key>.pid in cacheDir is a run. It waits
// for them to finish, and names each one it stopped.
//
// A file nobody holds is a run that ended without removing it — kill -9, a
// crash — and is cleared without a word. One still going after STOP_MS is
// named and the exit status is 1: it was asked, and asking twice changes
// nothing, since the binary swallows repeats.
//
// then, if given, runs once every run is gone, and not at all if one is
// still there: -kd starts its run only on a clean slate.
function kill(then) {
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
  for (const name of names.filter((n) => n.endsWith(".pid"))) {
    const file = path.join(dir, name);
    const pids = holders(file);
    if (pids.length === 0) {
      fs.rmSync(file, { force: true });
      continue;
    }
    for (const pid of pids) {
      if (runs.some((r) => r.pid === pid)) {
        continue;
      }
      try {
        process.kill(pid, "SIGINT");
      } catch (err) {
        console.error(`${WRAPPER_NAME}: cannot stop pid ${pid}: ${err.message}`);
        continue;
      }
      runs.push({ pid, key: path.basename(name, ".pid") });
    }
  }

  if (runs.length === 0) {
    if (!then) {
      console.error(`${WRAPPER_NAME}: no runs`);
    }
    then?.();
    return;
  }

  const deadline = Date.now() + STOP_MS;
  const timer = setInterval(() => {
    for (const r of runs.filter((r) => !r.done && !alive(r.pid))) {
      r.done = true;
      console.error(`${WRAPPER_NAME}: stopped pid ${r.pid} (${r.key})`);
    }
    const left = runs.filter((r) => !r.done);
    if (left.length === 0) {
      clearInterval(timer);
      then?.();
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

// FLAGS is the launcher's own first words. -kd and -dk are the two together,
// in either order: end every run, then start this one detached — a restart.
const FLAGS = { "-d": { detach: true }, "-k": { kill: true }, "-kd": { kill: true, detach: true }, "-dk": { kill: true, detach: true } };

function main() {
  const argv = process.argv.slice(2);
  const flag = FLAGS[argv[0]] ? argv[0] : null;
  const { kill: killing = false, detach: detaching = false } = FLAGS[flag] ?? {};
  const args = flag ? argv.slice(1) : argv;
  if (flag) {
    refuseOnWindows(flag);
  }

  if (killing && !detaching) {
    if (args.length > 0) {
      fail(`-k takes no arguments: it ends every run. To end them and start this one detached: npx ${WRAPPER_NAME} -kd …`);
    }
    kill();
    return;
  }

  // Refused before anything is ended: -kd with a line that could mean two
  // things should not cost the runs it was going to replace.
  const reading = misread(args);
  if (reading) {
    fail(ambiguous(reading, flag ? `npx ${WRAPPER_NAME} ${flag}` : `npx ${WRAPPER_NAME}`));
  }

  const bin = binaryPath();
  if (killing) {
    kill(() => detach(bin, args));
  } else if (detaching) {
    detach(bin, args);
  } else {
    run(bin, args);
  }
}

if (require.main === module) {
  main();
}

module.exports = { ambiguous, describe, isOrigin, misread, onPath, prose, quote };
