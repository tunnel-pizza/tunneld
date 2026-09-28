// Tests for v1.cjs, the npm launcher, under node's own runner: make launcher.
//
// The launcher is exercised two ways. misread is a function and is called
// directly, against a $PATH built for the case. Everything that spawns is
// driven as npx would drive it: v1.cjs copied into a package tree of its own
// in a temporary directory, beside a package.json and a stand-in binary in
// dist/, with $HOME pointed inside that tree so a detached run's records never
// land in the real cache directory.

const assert = require("node:assert/strict");
const { spawn, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { test } = require("node:test");

const { ambiguous, misread, isOrigin, quote } = require("./v1.cjs");

const win = process.platform === "win32";
const posixOnly = { skip: win && "-d and -k are refused on Windows" };

function tempDir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tunneld-launcher-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

// withPath points $PATH at a directory holding an executable for each name,
// for as long as the test runs.
function withPath(t, names) {
  const dir = tempDir(t);
  for (const name of names) {
    const file = path.join(dir, win ? `${name}.exe` : name);
    fs.writeFileSync(file, "", { mode: 0o755 });
  }
  const saved = { PATH: process.env.PATH, PATHEXT: process.env.PATHEXT };
  process.env.PATH = dir;
  process.env.PATHEXT = ".EXE";
  t.after(() => {
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  });
}

test("isOrigin matches the Go parser's", () => {
  for (const [word, want] of [
    [":3000", true],
    ["http://localhost:3000", true],
    ["attach://dockerd/app", true],
    [":", false],
    [":http", false],
    ["localhost:3000", false],
    ["--resume", false],
    ["next dev", false],
  ]) {
    assert.equal(isOrigin(word), want, word);
  }
});

test("quote spells a word that pastes back as itself", { skip: win && "cmd.exe quoting" }, () => {
  assert.equal(quote(":3000"), ":3000");
  assert.equal(quote("next dev"), "'next dev'");
  assert.equal(quote("it's"), "'it'\\''s'");
});

test("misread", async (t) => {
  withPath(t, ["claude", "next", "sh", "bash"]);
  const cases = [
    {
      name: "a bare program swallowing a quoted program",
      words: ["claude", "next dev"],
      two: ["next dev", "claude"],
      one: ['claude "next dev"'],
    },
    {
      name: "flags stay first, the program's own arguments move with it",
      words: ["--no-cache", "claude", "--resume", "abc", "next dev", ":3000"],
      two: ["--no-cache", "next dev", ":3000", "claude", "--resume", "abc"],
      one: ["--no-cache", 'claude --resume abc "next dev"', ":3000"],
    },
    {
      name: "the program again starts the next origin",
      words: ["bash", "bash", "next dev"],
      two: ["next dev", "bash", "bash"],
      one: ['bash "next dev"', "bash"],
    },
    {
      name: "args after the quoted word stay the program's",
      words: ["--no-cache", "claude", "next dev", "--resume"],
      two: ["--no-cache", "next dev", "claude", "--resume"],
      one: ["--no-cache", 'claude "next dev" --resume'],
    },
    { name: "a prompt is an argument", words: ["claude", "fix the bug"] },
    { name: "a flag's value is an argument", words: ["sh", "-c", "next dev"] },
    { name: "quoted first is already two origins", words: ["next dev", "claude"] },
    { name: "after a port the run has ended", words: ["claude", ":3000", "next dev"] },
    { name: "a word that is no program is not a program", words: ["nope", "next dev"] },
  ];
  for (const c of cases) {
    await t.test(c.name, () => {
      const got = misread(c.words);
      if (!c.two) {
        assert.equal(got, null);
        return;
      }
      assert.ok(got, "want the line refused");
      assert.deepEqual(got.two, c.two);
      if (!win) {
        assert.deepEqual(got.one, c.one);
      }
      // Each reading is what somebody pastes back, so neither may be refused.
      assert.equal(misread(got.two), null, "two origins, misread again");
      assert.equal(misread(got.one), null, "one origin, misread again");
    });
  }
});

test("ambiguous names both readings, ready to paste", { skip: win && "cmd.exe quoting" }, () => {
  const got = ambiguous(
    { program: "claude", arg: "next dev", two: ["next dev", "claude"], one: ['claude "next dev"'] },
    "npx tunneld",
  );
  assert.equal(
    got,
    "ambiguous: 'next dev' could be claude's argument or an origin of its own.\n" +
      "  Say which with quotes:\n" +
      "    npx tunneld 'next dev' claude    two origins\n" +
      "    npx tunneld 'claude \"next dev\"'  one origin",
  );
});

// A stand-in for the binary, doing what the real one does for the launcher.
// It registers as <key>.pid in $CACHE, held open on a descriptor its children
// do not get, and removes it on the way out while it still names its pid. It
// writes its argv to stderr, then two addresses to stdout with their origins
// on stderr. With TUNNELD_NOTIFY_PID naming its parent, it says where its log
// is, moves stdout and stderr to <key>.log and sends its parent SIGUSR2;
// otherwise it prints the binary's stop hint. Then it waits. SIGINT is its
// Ctrl-C: it takes a moment, as a real teardown does, leaves a mark in
// $MARKER so a test can tell a teardown from a kill, and exits 0. Coming up
// and going down are appended to $EVENTS. A first argument of fail makes it
// fail before any of it, with the second as its status.
const STAND_IN = `#!/bin/sh
echo "argv: $*" >&2
if [ "$1" = fail ]; then echo "no origin" >&2; exit "$2"; fi
key=$(printf %s "$*" | tr -c 'A-Za-z0-9' _)
mkdir -p "$CACHE"
exec 9>"$CACHE/$key.pid"
echo $$ >&9
echo "up $$" >> "$EVENTS"
trap 'sleep 0.2 9>&-; echo "down $$" >> "$EVENTS"; echo torn down > "$MARKER"; [ "$(cat "$CACHE/$key.pid")" = "$$" ] && rm -f "$CACHE/$key.pid"; exit 0' INT
echo "https://t.example/?0"
echo "  -> $1" >&2
echo "https://t.example/?1"
echo "  -> $2" >&2
if [ -n "$TUNNELD_NOTIFY_PID" ] && [ "$TUNNELD_NOTIFY_PID" = "$PPID" ]; then
  echo "tunneld: detached; its log is $CACHE/$key.log" >&2
  exec >"$CACHE/$key.log" 2>&1
  kill -USR2 "$PPID"
else
  echo "Press Ctrl+C to stop the tunnel..." >&2
fi
while :; do sleep 0.05 9>&-; done
`;

// pkg lays out a package the way npx sees one: package.json, v1/v1.cjs, and
// dist/tunneld-<platform>-<arch>. It returns functions running the launcher
// in it, with $HOME and $XDG_CACHE_HOME inside the tree, so the cache
// directory is too.
function pkg(t) {
  const root = tempDir(t);
  fs.mkdirSync(path.join(root, "v1"));
  fs.mkdirSync(path.join(root, "dist"));
  fs.writeFileSync(path.join(root, "package.json"), JSON.stringify({ name: "tunneld" }));
  fs.copyFileSync(path.join(__dirname, "v1.cjs"), path.join(root, "v1", "v1.cjs"));
  const bin = path.join(root, "dist", `tunneld-${process.platform}-${process.arch}`);
  fs.writeFileSync(bin, STAND_IN, { mode: 0o755 });

  const home = path.join(root, "home");
  const env = {
    ...process.env,
    HOME: home,
    XDG_CACHE_HOME: path.join(home, ".cache"),
    MARKER: path.join(root, "marker"),
    EVENTS: path.join(root, "events"),
  };
  delete env.TUNNELD_NOTIFY_PID;
  env.CACHE =
    process.platform === "darwin" ? path.join(home, "Library", "Caches", "tunneld") : path.join(home, ".cache", "tunneld");

  const launch = (...args) =>
    spawnSync(process.execPath, [path.join(root, "v1", "v1.cjs"), ...args], {
      env,
      encoding: "utf8",
      timeout: 20000,
    });
  const start = (...args) => spawn(process.execPath, [path.join(root, "v1", "v1.cjs"), ...args], { env });
  const events = () => fs.readFileSync(env.EVENTS, "utf8").trim().split("\n");
  return { root, bin, env, cache: env.CACHE, marker: env.MARKER, events, launch, start };
}

const ls = (dir) => (fs.existsSync(dir) ? fs.readdirSync(dir).sort() : []);
const pids = (dir) => ls(dir).filter((n) => n.endsWith(".pid"));

async function until(ok) {
  while (!ok()) {
    await new Promise((r) => setTimeout(r, 20));
  }
}

test("a plain run hands the binary every word and relays its status", posixOnly, (t) => {
  const { launch } = pkg(t);
  const got = launch("fail", "7", "-d");
  assert.equal(got.status, 7);
  assert.match(got.stderr, /argv: fail 7 -d\n/);
});

test("-d hands the console back once the run signals, and -k tears it down", posixOnly, (t) => {
  const { launch, cache, marker } = pkg(t);

  // spawnSync reads the launcher's streams to the end, the way $(…) does: it
  // returns only if the run has let go of them too.
  const up = launch("-d", ":3000", "bash");
  assert.equal(up.status, 0, up.stderr);
  assert.equal(up.stdout, "https://t.example/?0\nhttps://t.example/?1\n");
  assert.match(
    up.stderr,
    /^argv: :3000 bash\n  -> :3000\n  -> bash\ntunneld: detached; its log is .*_3000_bash\.log\ntunneld: detached as pid \d+; npx tunneld -k ends it, and every other run\n$/,
  );
  const pid = Number(up.stderr.match(/detached as pid (\d+)/)[1]);
  t.after(() => {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  });
  process.kill(pid, 0);
  assert.deepEqual(pids(cache), ["_3000_bash.pid"]);

  const down = launch("-k");
  assert.equal(down.status, 0, down.stderr);
  assert.equal(down.stderr, `tunneld: stopped pid ${pid} (_3000_bash)\n`);
  assert.equal(fs.readFileSync(marker, "utf8"), "torn down\n", "SIGINT, not a kill");
  assert.deepEqual(pids(cache), []);
});

test("-k ends a run in the foreground too", posixOnly, async (t) => {
  const { launch, start, cache, marker } = pkg(t);
  const fg = start(":3000");
  t.after(() => fg.kill("SIGKILL"));
  await until(() => pids(cache).length === 1);

  const down = launch("-k");
  assert.equal(down.status, 0, down.stderr);
  assert.match(down.stderr, /^tunneld: stopped pid \d+ \(_3000\)\n$/);
  const status = await new Promise((r) => fg.on("exit", (code) => r(code)));
  assert.equal(status, 0, "the foreground launcher passes on the run's own exit");
  assert.equal(fs.readFileSync(marker, "utf8"), "torn down\n");
});

for (const flag of ["-kd", "-dk"]) {
  test(`${flag} ends every run, then starts this one detached`, posixOnly, async (t) => {
    const { launch, start, cache, marker, events } = pkg(t);
    const fg = start(":3000");
    t.after(() => fg.kill("SIGKILL"));
    await until(() => pids(cache).length === 1);

    const up = launch(flag, ":4000");
    assert.equal(up.status, 0, up.stderr);
    assert.match(up.stderr, /^tunneld: stopped pid (\d+) \(_3000\)$/m);
    assert.equal(fs.readFileSync(marker, "utf8"), "torn down\n", "the old run tore down");
    assert.equal(up.stdout, "https://t.example/?0\nhttps://t.example/?1\n");
    const old = Number(up.stderr.match(/stopped pid (\d+)/)[1]);
    const pid = Number(up.stderr.match(/detached as pid (\d+)/)[1]);
    t.after(() => {
      try {
        process.kill(pid, "SIGKILL");
      } catch {}
    });
    assert.deepEqual(pids(cache), ["_4000.pid"], "the new run is the only one");
    assert.deepEqual(events(), [`up ${old}`, `down ${old}`, `up ${pid}`], "the new run started only once the old one was down");
    assert.equal(launch("-k").status, 0);
  });
}

test("-kd refuses an ambiguous line before ending anything", posixOnly, async (t) => {
  const { launch, start, cache, env } = pkg(t);
  const fg = start(":3000");
  t.after(() => fg.kill("SIGKILL"));
  await until(() => pids(cache).length === 1);
  const bin = tempDir(t);
  for (const name of ["claude", "next"]) {
    fs.writeFileSync(path.join(bin, name), "", { mode: 0o755 });
  }
  env.PATH = `${bin}${path.delimiter}${env.PATH}`;
  const got = launch("-kd", "claude", "next dev");
  assert.equal(got.status, 1);
  assert.match(got.stderr, /ambiguous/);
  assert.match(got.stderr, /npx tunneld -kd 'next dev' claude/);
  assert.deepEqual(pids(cache), ["_3000.pid"], "nothing was ended");
  assert.equal(launch("-k").status, 0);
});

test("-d relays a run that ends before its addresses", posixOnly, (t) => {
  const { launch } = pkg(t);
  const got = launch("-d", "fail", "3");
  assert.equal(got.status, 3);
  assert.equal(got.stdout, "");
  assert.match(got.stderr, /no origin\n/);
});

test("Ctrl-C while -d waits ends the run", posixOnly, async (t) => {
  const { bin, start, marker, events } = pkg(t);
  // A run that never says it is up.
  fs.writeFileSync(bin, STAND_IN.replace('kill -USR2 "$PPID"', ":"), { mode: 0o755 });
  const child = start("-d", ":3000");
  let stderr = "";
  child.stderr.on("data", (d) => (stderr += d));
  // Once it is up, the trap is set and the launcher is waiting.
  await until(() => stderr.includes("detached; its log is"));
  child.kill("SIGINT");
  const status = await new Promise((r) => child.on("exit", (code) => r(code)));
  assert.equal(status, 130, stderr);
  assert.equal(fs.readFileSync(marker, "utf8"), "torn down\n");
  assert.equal(events().at(-1).split(" ")[0], "down");
});

test("-k signals only a process holding its file", posixOnly, (t) => {
  const { launch, cache } = pkg(t);
  fs.mkdirSync(cache, { recursive: true });
  // This process's pid, alive but holding nothing: a pid handed on after its
  // run died without removing the file. A SIGINT would end this test runner.
  fs.writeFileSync(path.join(cache, "gone.pid"), `${process.pid}\n`);
  const got = launch("-k");
  assert.equal(got.status, 0, got.stderr);
  assert.equal(got.stderr, "tunneld: no runs\n");
  assert.deepEqual(pids(cache), []);
  process.kill(process.pid, 0);
});

test("an ambiguous line is refused before the binary runs", posixOnly, (t) => {
  const { launch, env } = pkg(t);
  const bin = tempDir(t);
  for (const name of ["claude", "next"]) {
    fs.writeFileSync(path.join(bin, name), "", { mode: 0o755 });
  }
  env.PATH = `${bin}${path.delimiter}${env.PATH}`;
  for (const args of [["claude", "next dev"], ["-d", "claude", "next dev"]]) {
    const got = launch(...args);
    assert.equal(got.status, 1);
    assert.match(got.stderr, /^tunneld: ambiguous: 'next dev' could be claude's argument/);
    assert.doesNotMatch(got.stderr, /argv:/, "the binary ran");
  }
});

test("-k takes no arguments", posixOnly, (t) => {
  const { launch } = pkg(t);
  const got = launch("-k", ":3000");
  assert.equal(got.status, 1);
  assert.match(got.stderr, /-k takes no arguments: it ends every run\. To end them and start this one detached: npx tunneld -kd/);
});

test("-d and -k are refused on Windows", { skip: !win && "Windows only" }, () => {
  for (const flag of ["-d", "-k"]) {
    const got = spawnSync(process.execPath, [path.join(__dirname, "v1.cjs"), flag], { encoding: "utf8" });
    assert.equal(got.status, 1);
    assert.match(got.stderr, new RegExp(`${flag} is not supported on Windows`));
  }
});
