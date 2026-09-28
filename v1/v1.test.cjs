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

// A stand-in for the binary: it writes its argv to stderr, two addresses to
// stdout with their origins on stderr between them, the binary's stop hint,
// and then waits. The
// pause between the two is longer than the launcher's poll, so a -d that took
// the first address for all of them would be caught. SIGINT is its Ctrl-C: it leaves a mark in $MARKER, so a test can tell
// a teardown from a kill, and exits 0. Its first argument can instead ask it
// to fail before any address, with that status.
const STAND_IN = `#!/bin/sh
echo "argv: $*" >&2
if [ "$1" = fail ]; then echo "no origin" >&2; exit "$2"; fi
trap 'echo torn down > "$MARKER"; exit 0' INT
echo "https://t.example/?0"
echo "  -> $1" >&2
sleep 0.1
echo "https://t.example/?1"
echo "  -> $2" >&2
echo "Press Ctrl+C to stop the tunnel..." >&2
while :; do sleep 0.05; done
`;

// pkg lays out a package the way npx sees one: package.json, v1/v1.cjs, and
// dist/tunneld-<platform>-<arch>. It returns a function running the launcher
// in it, with $HOME and $XDG_CACHE_HOME inside the tree.
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
  };
  const detached =
    process.platform === "darwin"
      ? path.join(home, "Library", "Caches", "tunneld", "detached")
      : path.join(home, ".cache", "tunneld", "detached");

  const launch = (...args) =>
    spawnSync(process.execPath, [path.join(root, "v1", "v1.cjs"), ...args], {
      env,
      encoding: "utf8",
      timeout: 20000,
    });
  return { root, bin, env, detached, marker: env.MARKER, launch };
}

test("a plain run hands the binary every word and relays its status", posixOnly, (t) => {
  const { launch } = pkg(t);
  const got = launch("fail", "7", "-d");
  assert.equal(got.status, 7);
  assert.match(got.stderr, /argv: fail 7 -d\n/);
});

test("-d prints the addresses, leaves the run going, and -k tears it down", posixOnly, (t) => {
  const { launch, detached, marker } = pkg(t);

  const up = launch("-d", ":3000", "bash");
  assert.equal(up.status, 0, up.stderr);
  assert.equal(up.stdout, "https://t.example/?0\nhttps://t.example/?1\n");
  assert.match(up.stderr, /argv: :3000 bash\n  -> :3000\n  -> bash\ntunneld: detached as pid/);
  const pid = Number(up.stderr.match(/detached as pid (\d+)/)[1]);
  t.after(() => {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  });
  process.kill(pid, 0);
  assert.deepEqual(fs.readdirSync(detached).sort(), [`${pid}.json`, `${pid}.log`, `${pid}.out`]);

  const down = launch("-k");
  assert.equal(down.status, 0, down.stderr);
  assert.match(down.stderr, new RegExp(`stopped pid ${pid} https://t.example/\\?0 https://t.example/\\?1`));
  assert.equal(fs.readFileSync(marker, "utf8"), "torn down\n", "SIGINT, not a kill");
  assert.deepEqual(fs.readdirSync(detached), []);
});

test("-d relays a run that ends before its addresses", posixOnly, (t) => {
  const { launch, detached } = pkg(t);
  const got = launch("-d", "fail", "3");
  assert.equal(got.status, 3);
  assert.equal(got.stdout, "");
  assert.match(got.stderr, /no origin\n/);
  assert.deepEqual(fs.readdirSync(detached), []);
});

test("Ctrl-C while -d waits ends the run", posixOnly, async (t) => {
  const { root, env, marker, detached } = pkg(t);
  // Stdout that never settles: an address without its newline.
  fs.writeFileSync(
    path.join(root, "dist", `tunneld-${process.platform}-${process.arch}`),
    STAND_IN.replace('echo "https://t.example/?0"', 'printf "https://t.example/?0"'),
    { mode: 0o755 },
  );
  const child = spawn(process.execPath, [path.join(root, "v1", "v1.cjs"), "-d", ":3000"], { env });
  let stderr = "";
  child.stderr.on("data", (d) => (stderr += d));
  // Once the address is out, the trap is set and the launcher is waiting.
  const out = () =>
    fs.existsSync(detached) && fs.readdirSync(detached).some((n) => /^\d+\.out$/.test(n) && fs.statSync(path.join(detached, n)).size > 0);
  while (!out()) {
    await new Promise((r) => setTimeout(r, 20));
  }
  child.kill("SIGINT");
  const status = await new Promise((r) => child.on("exit", (code) => r(code)));
  assert.equal(status, 130, stderr);
  assert.equal(fs.readFileSync(marker, "utf8"), "torn down\n");
  assert.deepEqual(fs.readdirSync(detached), []);
});

test("-k clears a record whose run is gone", posixOnly, (t) => {
  const { launch, detached, bin } = pkg(t);
  fs.mkdirSync(detached, { recursive: true });
  // A pid nothing holds, and this process's own, which is not the binary.
  const dead = spawnSync("true").pid;
  for (const pid of [dead, process.pid]) {
    fs.writeFileSync(path.join(detached, `${pid}.json`), JSON.stringify({ pid, bin }));
    fs.writeFileSync(path.join(detached, `${pid}.out`), "https://gone.example/\n");
  }
  const got = launch("-k");
  assert.equal(got.status, 0, got.stderr);
  assert.match(got.stderr, /no detached runs/);
  assert.deepEqual(fs.readdirSync(detached), []);
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
  assert.match(got.stderr, /-k takes no arguments/);
});

test("-d and -k are refused on Windows", { skip: !win && "Windows only" }, () => {
  for (const flag of ["-d", "-k"]) {
    const got = spawnSync(process.execPath, [path.join(__dirname, "v1.cjs"), flag], { encoding: "utf8" });
    assert.equal(got.status, 1);
    assert.match(got.stderr, new RegExp(`${flag} is not supported on Windows`));
  }
});
