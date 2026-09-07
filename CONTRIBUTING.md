# Contributing

This document is for everyone working on `tunneld` — humans and AI agents alike.
It covers the layout, the local dev loop, the conventions that bite, and how a
change gets from an issue to a release.

## Where to find things

Deep-link by filename; line numbers will drift.

| Topic                                          | Source                                                           |
| ---------------------------------------------- | ---------------------------------------------------------------- |
| Process shell (signals → context → Execute)    | [`main.go`](./main.go)                                           |
| Stable interface (`Builder`)                   | [`v1/v1.go`](./v1/v1.go)                                         |
| `Err*` sentinels + env / default constants     | [`v1/v1.go`](./v1/v1.go)                                         |
| `New`, `BuilderImpl`, the internal contracts + options | [`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go)                 |
| Builder options, `Command` (flags, env binding, the tunnel run), `flagEnv`, `PublicURL` | [`v1alpha1/builder.go`](./v1alpha1/builder.go) |
| Version resolution + build banner              | [`v1alpha1/version.go`](./v1alpha1/version.go)                   |
| `CacheDirs` contract's implementation: the --cache-dir list and its pflag value | [`v1alpha1/cachedir/`](./v1alpha1/cachedir) |
| Tunnel engine (`Engine` ← libtunnel)           | [`v1alpha1/engine/`](./v1alpha1/engine)                          |
| Gone-verdict counter (`Counter`)               | [`v1alpha1/counter/`](./v1alpha1/counter)                        |
| Spec cache, `TUNNEL.env` (`Cache`)             | [`v1alpha1/cache/`](./v1alpha1/cache)                            |
| Multiview panel, framing headers, template (`Panel`) | [`v1alpha1/panel/`](./v1alpha1/panel)                      |
| Edge probe + browser launch (`Opener`)         | [`v1alpha1/browser/`](./v1alpha1/browser)                        |
| `Target`, `Targets`, `Server`, and the `Binder` implementation | [`v1alpha1/attach/`](./v1alpha1/attach) |
| Docker provider of `Target` and `Targets`      | [`v1alpha1/attach/docker/`](./v1alpha1/attach/docker)            |
| godoc examples                                 | [`v1alpha1/example_test.go`](./v1alpha1/example_test.go)         |
| e2e harness + runner                           | [`e2e/e2e_test.go`](./e2e/e2e_test.go)                           |
| Worked examples                                | [`examples/`](./examples)                                        |
| Sample pages the examples serve                | [`examples/sites`](./examples/sites)                             |
| Build / lint / test commands                   | [`Makefile`](./Makefile)                                         |
| npm launcher: finds the platform binary, hands it the process | [`v1/v1.cjs`](./v1/v1.cjs)                            |
| npm manifest (version stays 0.0.0; the tag is the release) | [`package.json`](./package.json)                          |
| Release + skip release regex                   | [`.github/workflows/ci.yml`](./.github/workflows/ci.yml)         |
| CodeQL scan                                    | [`.github/workflows/codeql.yml`](./.github/workflows/codeql.yml) |
| OpenSSF Scorecard scan                         | [`.github/workflows/scorecard.yml`](./.github/workflows/scorecard.yml) |
| Dependabot config                              | [`.github/dependabot.yml`](./.github/dependabot.yml)             |
| Cosign verification recipe                     | [`SECURITY.md`](./SECURITY.md)                                   |
| Orientation for AI agents                      | [`CLAUDE.md`](./CLAUDE.md)                                       |

## Module layout

The module root is the `tunneld` command; the library tiers sit under it with
stable/alpha versioning:

```
github.com/tunnel-pizza/tunneld           — package main. Signals → context →
                                            Execute, and nothing else.
github.com/tunnel-pizza/tunneld/v1        — stable Builder contract, Err*
                                            sentinels, env / default constants.
github.com/tunnel-pizza/tunneld/v1alpha1  — current implementation: command
                                            assembly, the tunnel it runs, the
                                            version resolution. May change
                                            between alpha revisions.
```

Application code constructs from `v1alpha1` (`v1alpha1.New(opts...)`) and matches
errors against `v1`. There is no façade re-exporting both: a constructor has to
import what it constructs, `v1alpha1` already imports `v1` for the sentinels,
and Go does not allow the cycle.

`main.go` stays thin on purpose. Everything the command *does* — flags, help
text, validation, the tunnel — is assembled by the builder, so another program
can embed tunneld as a subcommand of its own with
`v1alpha1.New(v1alpha1.WithName("expose"))`
and get the identical behaviour. A feature that only works when tunneld is
`os.Args[0]` is a feature in the wrong package.

## Design conventions

Conventions, not machinery — nothing here enforces them.

**Surface/engine split.** `v1.Builder` is only what a caller calls once the
builder exists: `Command` and `Name`. Everything `Command`'s `RunE` composes
that owns an external effect — the edge, the disk, the daemon, the browser, an
HTTP probe — is an internal contract in
[`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go): `CacheDirs`, `Engine`,
`Cache`, `Panel`, `Opener`, `Counter`, `Binder`, implemented respectively by
`cachedir`, `engine`, `cache`, `panel`, `browser`, `counter`, `attach`. Each
has one implementation, named `XImpl`, in its own `v1alpha1/<name>`
subpackage, seeded by `New` and replaceable with the matching `With*` option.
A function that maps a value to a value (`PublicURL`, `Version`) gets no
interface — origin parsing, for instance, sits at the top of `Command`'s
`RunE` rather than behind a contract of its own. The assertion block in
`v1alpha1.go` is where a default that drifts from its contract fails — at
build, not at the first run.

A function with one caller is inlined; the doc comment stays where the body
went.

**One way to configure anything.** `v1.Option[T]` is `func(T)`. Every package
aliases it to its own `Option`, exposes `With*` constructors returning it, and
its `New(opts ...Option)` applies its defaults first and the caller's after —
later wins. That holds for the builder's flag seeds, the contract injectors
and each implementation's tunables alike, and a package with no tunables still
takes the variadic so adding one changes no caller. An option is a plain
function, so it can be applied anywhere a setter used to be called: `New`
seeds its own defaults with the same `With*` options a caller passes —
`WithOpen(v1.DefaultOpen)` goes through the same `v1.Apply` path as a
caller's own `WithOpen(false)`.

**Command assembles once.** `Command` is guarded by `commandOnce` (see
[`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go)) and that is correctness, not
an optimization: the command's flags bind *over* the builder's own fields, so a
second assembly would register a second set of flags against the same storage.
If a knob ever needs the same protection, give it its own `sync.Once` — one per
knob, not one shared "frozen" flag, so an unrelated late configuration is not
silently dropped.

**Seeds are defaults, not settings.** Every `With*` option's value becomes the default
of the flag that binds over it, so an argv value always wins. That is what lets
an embedder supply a working origin (`WithOrigin`) while leaving the user free
to override it. Origins are the arguments rather than a flag, so the same rule
is applied by hand in `RunE`: argv replaces the variable, which replaces the
seed.

**Every implementation is a `v1alpha1/<name>` subpackage.** One per contract,
unconditionally — `cachedir`, `engine`, `cache`, `panel`, `browser`, `counter`,
`attach` — and the `v1alpha1` root stays implementation-agnostic:
`New`, the contracts, the options and `Command`. A second implementation of a
contract gets a subpackage of its own beside the first. The same applies to
anything with a world of its own: [`v1alpha1/panel`](./v1alpha1/panel) is a
panel, a template and header surgery, none of which the root needs to know
about. A `go:embed`ed asset settles it on its own — the directive cannot reach
outside its package, so the template has to live beside the code. This keeps a backend's
dependencies out of the import graph of anyone using a different one, and gives
each backend a natural home for its own `TUNNELD__<IMPL>_<KNOB>` environment
variables (see the naming rules in [`v1/v1.go`](./v1/v1.go)).

## Local development

Requires Go 1.26 or later — the floor comes from `libtunnel`, the tunnel engine
(see the comment in [`go.mod`](./go.mod)).

```sh
git clone https://github.com/tunnel-pizza/tunneld.git
cd tunneld
make test     # unit tests (fast, in-package)
make e2e      # builds the binary and every example, drives their offline paths
make binary   # build ./tunneld for the host
```

The container tests under [`v1alpha1/attach/`](./v1alpha1/attach) skip
themselves unless a Docker daemon answers *and* the `alpine` image is already
local, so `make test` goes green on a machine that has neither — and a skipped
test proves nothing. To actually run them, start Docker and `docker pull
alpine` once; CI's `docker` lane does exactly that.

Run it against a local service:

```sh
go run . http://localhost:3000 http://localhost:4000
```

Or run an example, which seeds its own origins:

```sh
make run basic
make run multi-origin
```

`go run` rather than a make target wherever flags are involved: make reads a
leading `--` as one of its own options and refuses. The `run` target takes an
example *name*, which is a bare word, so it works — `make run basic --no-open`
does not.

## Test layout

Three tiers, each with a distinct job — don't blur them:

- **`*_test.go` next to the code** — unit tests: anything with fabricated
  inputs or fakes, however elaborate. Includes fuzz targets and the godoc
  examples in [`v1alpha1/example_test.go`](./v1alpha1/example_test.go).
- **`examples/`** — real-world, simple-ish API usage written for humans. An
  example demonstrates; it never asserts. Assertion logic belongs in `e2e/`.
- **`e2e/`** — the harness builds the `tunneld` binary and every example
  binary, and drives them. If a check can pass without executing a binary, it
  is a unit test, not e2e.

One tier mints a real tunnel, and only one row in it does. Everything up to the
mint — parse, validate, refuse or proceed — is covered without the public
internet, which is why most e2e cases drive a binary with `--help` or `version`
and assert on what comes back: an example is a network program that would
otherwise mint a hostname and block forever.

The `basic` row is the exception, and it earns the hostname. It starts the
example for real, waits for the address on stdout, fetches it back through the
edge, and interrupts it — the one check that tunneld actually carries traffic
rather than merely reporting that it opened something. A live row is expensive
and can be rate limited by the provider, so it runs for whoever is developing
and on a single CI cell. Two reasons not to mint belong to the machine and are
applied to every live row: `-short`, which is how the race lane opts out, and
Windows, which cannot deliver `os.Interrupt` to a child and so cannot assert
the teardown. Anywhere else is the row's own `skip` function. Its
assertions are a `[]func(t *testing.T, r *runner)` run in order, each reading
what it needs out of the buffered streams, so one can be dropped or reordered
without touching the others.

Test the *thing*, not the incident: name a test after the behaviour it pins
(`TestFlagReplacesSeededURLs`), and let its doc comment say why that behaviour
matters. Internal (`package v1alpha1`) and external (`package v1alpha1_test`)
test files coexist in one directory — reach for the external form by default,
and the internal one only to cover unexported behaviour.

### One test file per source file

A test file mirrors its source file and takes its name: `something.go` is
tested by `something_test.go`, and that is the only test file for it. Don't
mint a second file per topic, per concern, or per test style —
`version_internal_test.go`, `builder_edge_cases_test.go`,
`env_validation_test.go` all name a *use case* rather than a source file, and
none of them should exist.

The pull is real, so it's worth naming why we resist it. A topic-named file
looks tidy the day it's added and stops being findable a month later: the tests
for one symbol scatter across files whose names only their author can predict,
nothing tells you which file a new test belongs in, and two files quietly grow
overlapping coverage of the same function. Pairing with the source file removes
the judgment call — there is exactly one answer, and it's the same answer every
time.

If a test file grows unwieldy, that's a signal about the *source* file, not the
test file. Split the source, and the tests split with it along the same seam.

When a bug sends you looking for somewhere to put its test, the answer is
always the `_test.go` beside the file that had the bug, as another case in the
table that already covers that function. Use cases are what table rows are for;
files are for code.

Two deliberate exceptions:

- **`v1alpha1/example_test.go`** — godoc examples. `example_test.go` is an
  established Go idiom and renders as documentation on pkg.go.dev, so it keeps
  its name.
- **`e2e/`** — a test-only package, so there's no source file to pair with.

One consequence worth knowing: a source file whose tests need both unexported
access and an outside-the-package view still gets one test file, so it is
`package <pkg>` (internal) and the external view is covered elsewhere.
[`v1alpha1/builder_test.go`](./v1alpha1/builder_test.go) is that case — it
reaches `flagEnv` and the fakes behind `Command`'s tunnel run, so the whole
file is internal, and the genuine consumer's view is covered by `e2e`.

## Before you push

- `gofmt -w .`
- `go vet ./...`
- `make test`
- `make e2e`

Or `make all`, which is all of the above plus the Windows cross-build.

CI runs the same on every PR, and adds one lane `make all` leaves out:

- `make race` — every package under the race detector. It sits outside `make
  all` because it is the only target needing a C toolchain: the detector links
  through cgo, so the target overrides the `CGO_ENABLED=0` the rest of the
  Makefile exports. The workflow runs this exact target, so a race CI finds
  reproduces locally with the same command.

## Conventions that bite

Easy to get wrong from the diff alone:

- **Origins are arguments, and they settle in `RunE`.** There is no flag to
  bind them to, so `Command` binds `TUNNELD_ORIGINS` under a key of its own and
  `RunE` applies argv > variable > seed by hand. Each layer replaces the one
  under it; none merges into it.
- **`TUNNELD_ORIGINS` splits on commas** — a single variable has no other way
  to carry a list, and viper does not split one for you. Argv does not split at
  all, which is the mirror's one limitation: an origin carrying a literal comma
  has to arrive as an argument.
- **Flag variables are applied in `PersistentPreRunE`, not `RunE`.** Cobra runs
  that hook *before* `ValidateRequiredFlags`, which is the only reason a
  variable alone can satisfy a required flag. Marking `f.Changed` there is the
  other half. `--cache-dir` is the one that needs it.
- **The `?n` routing parameter must stay bare.** `https://host/?1` routes to
  origin 1; `?1=x` is application data the proxy forwards untouched. See
  `PublicURL` in [`v1alpha1/builder.go`](./v1alpha1/builder.go).
- **The panel answers the tunnel's bare address, and every condition narrowing
  that is load-bearing.** The panel interceptor's `Match` requires path `/`,
  an *empty* query, a top-level document, and no same-host referer. Drop the
  path check and the panel swallows every `/app.js` an origin serves; loosen
  the query check to "no routing index" and an OAuth callback at `/?code=…`
  lands on a page of frames; drop the Sec-Fetch check and it draws itself
  inside its own tiles; drop the referer check and a `fetch` from an origin
  page gets HTML. See [`v1alpha1/panel`](./v1alpha1/panel).
- **The framing-header removal must stay narrowed to the panel's own frames.**
  The unframer drops `X-Frame-Options` and CSP's `frame-ancestors` so a tile
  is not blank, and it runs only behind the second interceptor's `Match`,
  gated on `Sec-Fetch-Dest` being a frame *and* `Sec-Fetch-Site` being
  `same-origin`. Widening either condition would strip a real protection from
  top-level visits or hand another site the ability to frame somebody's
  tunnel. Everything else in a policy is preserved directive by directive;
  the scrub happens in `WriteHeader`, because headers are immutable after the
  first write.
- **The page's CSS is layout only.** Colour, type, borders and radius come
  from Basecoat; the CDN build ships component classes without Tailwind's
  utilities, so the rule of thumb is that a style earns its place only when no
  class can supply it. Both CDN URLs carry a subresource-integrity digest —
  recompute it (`openssl dgst -sha384 -binary <file> | openssl base64 -A`) when
  bumping the version, or the page silently loses its stylesheet.
- **The default origin is `?0`, not a bare URL, whenever there is more than
  one.** Routing falls back to the referring page and then to a sticky cookie,
  so a plain address stops reaching origin 0 once a browser has visited `?1`.
  Only an explicit index clears a previous choice. A lone origin has nothing to
  route between and keeps the plain URL — which is why `PublicURL` takes the
  origin count.
- **A running tunnel splits its report across both streams**: each public
  address goes to stdout, one per line and nothing else, and the origin that
  address reaches goes to stderr beneath it, along with the banner and every
  log line. stdout also carries the help text and `tunneld version`, so all
  three stay pipeable.
- **An address reaches exactly one stream.** stdout carried bare URLs once
  while stderr carried a full map, so every address printed twice wherever both
  streams landed together, and the `os.SameFile` de-duplication that hid it
  could only see one descriptor being literally the other — which a container's
  two pipes are not, so it never fired in the place it mattered. Printing an
  address on one stream and its origin on the other is what replaced that;
  don't put an address back on stderr without solving the duplication first.
- **`examples/` is intentionally duplicated, except for the pages.** Each
  `main.go` is a copy-pasteable starter and stays standalone — don't refactor
  the wiring into a shared helper. The sample HTML in
  [`examples/sites`](./examples/sites) is the deliberate exception: nobody
  copies those, and one of each means a fix to the scrolling or the sticky
  header lands everywhere instead of drifting between examples.
- **e2e builds its binaries at runtime**, so the test cache can't see source
  changes — `make e2e` passes `-count=1` to force a rebuild.
- **Skip-release token must be line-anchored.** The regex in
  [`ci.yml`](./.github/workflows/ci.yml) (`resolve tag` step) is
  `^[[:space:]]*\[skip release\][[:space:]]*$`. Inline prose mentions are safe;
  a standalone line in the commit body opts out.
- **Cosign / Scorecard tags are annotated.** `ossf/scorecard-action` publishes
  annotated tags; pinning the tag-object SHA fails Sigstore verification
  ("imposter commit"). Pin to the commit underneath (see existing entries in
  [`scorecard.yml`](./.github/workflows/scorecard.yml)).

### Container origins

`v1alpha1/attach/` serves a `dockerd://` origin as a browser terminal, and
`v1alpha1/attach/docker/` is the one provider behind it. The split is
load-bearing: `attach` knows HTTP and the `v4.channel.k8s.io` stream protocol
and nothing about Docker, and `docker` is the reverse. A second provider
implements `attach.Target` — five methods, four of its own plus the embedded
`remotecommand.Attacher`'s `AttachContainer` — and `attach` does not change.

`docker.TargetImpl` is that provider. What opens one by reference —
`docker.TargetsImpl.Open` — sits behind `attach`'s own `Targets` contract,
which is how `attach.BinderImpl.Bind` is tested with a stub and no daemon.

Two things there will bite if you change them without knowing why:

- **The page builds its socket URL as `"/attach" + location.search`.** A
  websocket handshake carries no `Referer`, so libtunnel cannot route it as a
  subresource and would fall back to the sticky `libtunnel-origin` cookie,
  which is last-write-wins across tabs. Drop the suffix and two container tiles
  fight over one socket.
- **`attach.BinderImpl.Bind` keeps the dialable list the same length and order
  as the displayed one.** Index *n* means origin *n* for `?n` routing,
  `PublicURL`, the reported map and the multiview tiles. Reordering or
  filtering either list breaks all four at once.

## Adding an example

Examples live in `./examples/<name>/main.go`. Keep each example self-contained
(there's no shared internal package — the duplication is intentional, so each
example is copy-pasteable on its own).

An example starts the origins it exposes, so someone can run it against a
clean machine and see a tunnel work. That holds even when the origin isn't an
HTTP server: `examples/attach` creates and starts the container it attaches
to, and pulls the image if it has to, rather than telling a reader to go run
`docker` first. An example that needs setup done for it has moved the
interesting part into a README nobody reads. The pages an example does serve
come from
[`examples/sites`](./examples/sites), shared across examples; add one there
rather than inlining HTML in a `main.go`. Hang startup on the built command's
`PreRunE`, not on plain code before `ExecuteContext`: cobra answers `--help`
before it reaches that hook, which is what keeps `--help` from binding a port.
`Command` returns an ordinary `*cobra.Command`, so the hook is free — but
`PersistentPreRunE` is already taken by the environment binding, so use the
non-persistent one.

Every example opens a tunnel and then blocks, so most are checked through their
own `--help`. Give each a configuration that shows up there — a seeded origin
list, a flag default it flips — then add a row to the `cases` table in
`e2e/e2e_test.go` (name + a substring unique to that example's help) and to the
README's example table. A substring that would also match another example is a
case that can pass against the wrong binary.

A row can instead carry `assert`, a list of checks run in order against the
example running for real, and `skip`, which decides where minting a public
hostname is worth it. `basic` is the one row that does; adding a second means
a second tunnel per test run, so it wants a reason the help text cannot give.
A row with no `assert` is never gated — `--help` costs nothing and runs
everywhere, including the lanes that mint no tunnel.

## Adding a flag

The flag surface is deliberately small: `--provider`, `--log-level`,
`--cache-dir`. Origins are not among them — they are the arguments.
Everything else the tunnel engine can do is reachable through `libtunnel`'s own
`LIBTUNNEL_*` environment variables, which pass straight through — reach for
those before adding a flag.

When a flag really is warranted, five things move together:

1. a `With*` option in [`v1alpha1/builder.go`](./v1alpha1/builder.go), so an
   embedder can seed it — with the doc that says what it seeds and that the
   flag overrides it;
2. its environment mirror, as a `TUNNELD_<KNOB>` constant in `v1/v1.go` — the
   one registry for operator-facing strings;
3. the field on `BuilderImpl` and the `cmd.Flags()` binding in `v1alpha1` — the
   binding's default is the seeded field, never a literal — plus a row in
   `flagEnv` in [`v1alpha1/builder.go`](./v1alpha1/builder.go) pairing the flag with
   the constant;
4. a case in the table in `v1alpha1/builder_test.go`, plus a row in
   `e2e/e2e_test.go` if the flag has a refusable value; and
5. the **Flags** and **Environment** tables in the README.

Step 3's `flagEnv` row is the one that is easy to forget, and
`TestFlagEnvRegistryIsComplete` in `v1alpha1/builder_test.go` fails without it: a
flag with no mirror works on the command line and is silently unreachable from
a container's environment.

## Adding a collaborator

A new thing `Command`'s `RunE` composes that has an external effect gets a
contract, not a function. Seven things move together, and
`TestNewWiresEveryCollaborator` plus the assertion block catch the ones that
are easy to forget:

1. the interface in [`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go), beside
   the others, with flag-settled configuration as method arguments rather
   than constructor state;
2. `v1alpha1/<name>/<name>.go` with `XImpl`, `type Option =
   v1.Option[*XImpl]`, `New(opts ...Option) *XImpl`, and a `With*` per
   tunable;
3. the field on `BuilderImpl`, in the collaborators group;
4. `With<Name>(x <Name>) Option` in `v1alpha1.go`, and the default in `New`;
5. a line in the assertion block, one in the wiring check at the top of
   `Command`, and a row in `TestNewWiresEveryCollaborator`;
6. a fake in `v1alpha1/builder_test.go` and a case in `TestRun` for what
   `Command`'s `RunE` does with it;
7. a row in the file map above.

## Branch / PR flow

**Every change starts with an issue** — no exceptions, including retroactive
cleanups. The PR body always carries a `Closes #<n>` line so the merge
auto-closes the tracking issue and leaves a paper trail.

```sh
gh issue create --title "…" --body "…"                    # 1. issue first
git switch -c <type>/<topic>                              # 2. branch
# ... edits, commit ...
git push -u origin <type>/<topic>
gh pr create --title "<type>: …" --body "Closes #<n>. …"  # 3. PR refs the issue
# CI green ⇒
gh pr merge <pr#> --squash --delete-branch
```

`main` is protected (`ci` required; no force-push). Don't push directly to it
for routine work — PR flow gives CI + auto-release a clean audit trail. Pushing
to `main` auto-bumps a patch tag and signs the release (see Releasing below).

Don't commit secrets. [`.gitignore`](./.gitignore) covers `.env*`, `.claude/`,
`*.local`, etc. Keep the `*.local` line broad rather than narrowing it to
names.

`TUNNEL.env` needs its own entry, because it is not a `*.local`. It is the
cached tunnel spec — credentials. The default cache directory is a per-project
one under the user's cache directory, never the checkout, so a plain run
writes nothing here; but `--cache-dir .` or `TUNNELD_CACHE_DIR` can point at
the checkout, and the entry is what keeps that spec out of a commit. A rename
of that file has to update `.gitignore` in the same change, or the next
`git add -A` commits a credential. `make clean` removes it, along with the
compose example's volume and the local image.

## Pull requests

- Keep PRs focused. One feature or fix per PR.
- Include test coverage for behavior changes — unit tests beside the code
  (`something.go` → `something_test.go`) for library changes, e2e tests
  (`e2e/e2e_test.go`) for anything visible at the command line.
- **Keep the README in sync with the surface.** The README mirrors both the
  flag surface and the public API, so any change to either must update it in
  the same PR:
  - a new/changed/removed flag → update the **Flags** table and, if it is
    user-facing enough, the **Quick Start**;
  - a new/changed/removed method on `Builder` (or the `v1` surface) → update
    the **API at a glance** block;
  - a renamed package/version tier → update the **Layout** tree.
  Treat the README's code blocks as documentation that must compile against the
  current API — stale snippets are a review blocker.
- Signed commits preferred. The repo enables commit signing locally; CI does
  not enforce signatures.

## Commit messages

Short subject (≤ 72 chars), imperative mood ("Add X", not "Added X").
Wrap body at ~72 cols. Explain the *why*; the diff covers the *what*.

## Releasing

Patch releases are automatic. Every push to `main` runs the `Release`
workflow, which bumps the patch component of the latest `v*` tag,
re-runs `go vet`, `go build`, `make test`, and `make e2e` against that
ref, then:

- pushes the new tag,
- creates a GitHub Release with auto-generated notes,
- builds and pushes `ghcr.io/tunnel-pizza/tunneld` for `linux/amd64` and
  `linux/arm64`, tagged with the release and `latest`,
- warms `proxy.golang.org` so [pkg.go.dev](https://pkg.go.dev/github.com/tunnel-pizza/tunneld)
  surfaces the new version without manual prodding, and
- publishes `tunneld` to npm with provenance, through npm's trusted
  publisher binding for this repo and workflow (no token). `make binaries`
  builds the six platform binaries into `dist/`, stamped with the tag through
  `VERSION`, and `package.json` is rewritten to the tag for that publish only.
  A release publishes to `latest`. For a `beta` instead, merge with
  `[skip release]`, then run the CI workflow by hand from `main` with the
  dist-tag input set to `beta`; that cuts the release and publishes it there.
  Promote later without rebuilding: `npm dist-tag add tunneld@<version> latest`.

Source archives and the image are both signed with cosign in keyless mode. The
image is signed **by digest**, not by tag: a tag can be moved to point at other
bytes, and a signature that followed it would vouch for whatever it moved to.

The image is multi-arch without QEMU — the [`Dockerfile`](./Dockerfile) builds
on `BUILDPLATFORM` and lets Go cross-compile to `TARGETARCH`, so both arches
are native compiles. `make image` builds it for the host platform from the same
file, which is how you catch a break before a tag does.

To opt a commit out of the auto-bump, put `[skip release]` on its own
line in the commit body. (It must be the only thing on its line, so
prose mentioning the token inline doesn't accidentally suppress.)

For a minor or major bump, tag locally and push the tag — the workflow
treats a manual tag as the version of record and skips the bump:

```sh
git tag v0.2.0
git push --tags
```

Tags must follow `vMAJOR.MINOR.PATCH` (Go module semver).

## License

By contributing you agree your contributions are licensed under the
[Functional Source License, FSL-1.1-MIT](./LICENSE.md).
