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
| Builder options, `Command` (flags, env binding, the tunnel run), `Origins`, `flagEnv`, `publicURL` | [`v1alpha1/builder.go`](./v1alpha1/builder.go) |
| Version resolution + build banner              | [`v1alpha1/version.go`](./v1alpha1/version.go)                   |
| `CacheDirs` contract's implementation: the --cache-dir list and its pflag value | [`v1alpha1/cachedir/`](./v1alpha1/cachedir) |
| Tunnel engine (`Engine` ← libtunnel)           | [`v1alpha1/engine/`](./v1alpha1/engine)                          |
| Gone-verdict counter (`Counter`)               | [`v1alpha1/counter/`](./v1alpha1/counter)                        |
| Spec cache, `TUNNEL.env` (`Cache`)             | [`v1alpha1/cache/`](./v1alpha1/cache)                            |
| Browser launch, multiview panel, framing headers, template (`Browser`) | [`v1alpha1/browser/`](./v1alpha1/browser) |
| `Target`, `Targets`, `Server`, the terminal frame, and the `Binder` implementation | [`v1alpha1/attach/`](./v1alpha1/attach) |
| Docker provider of `Target` and `Targets`      | [`v1alpha1/attach/docker/`](./v1alpha1/attach/docker)            |
| Local-program provider, `Resolve`, pty settings | [`v1alpha1/attach/shell/`](./v1alpha1/attach/shell)             |
| Ring of tunneld's own log lines (`attach.Logs`) | [`v1alpha1/logs/`](./v1alpha1/logs)                             |
| Drawing a served terminal on the local console  | [`v1alpha1/console/`](./v1alpha1/console)                        |
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
`Cache`, `Browser`, `Counter`, `Binder`, implemented respectively by
`cachedir`, `engine`, `cache`, `browser`, `counter`, `attach`. Each
has one implementation, named `XImpl`, in its own `v1alpha1/<name>`
subpackage, seeded by `New` and replaceable with the matching `With*` option.
A function that maps a value to a value (`publicURL`, `Version`) gets no
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
`WithMultiview(v1.DefaultMultiview)` goes through the same `v1.Apply` path as
a caller's own `WithMultiview(false)`.

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
unconditionally — `cachedir`, `engine`, `cache`, `browser`, `counter`,
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
example *name*, which is a bare word, so it works — `make run basic --provider x`
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
  `publicURL` in [`v1alpha1/builder.go`](./v1alpha1/builder.go).
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
  route between and keeps the plain URL — which is why `publicURL` takes the
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
`v1alpha1/attach/docker/` is the provider behind it. The split is
load-bearing: `attach` knows HTTP and the `v4.channel.k8s.io` stream protocol
and nothing about Docker, and `docker` is the reverse. Another provider
implements `attach.Target` — five methods, four of its own plus the embedded
`remotecommand.Attacher`'s `AttachContainer` — and `attach` does not change.

`docker.TargetImpl` is that provider, and `shell.TargetImpl` is the second:
a local program run on a pseudo-terminal. What opens one by reference —
`TargetsImpl.Open` in either — sits behind `attach`'s own `Targets` contract,
which is how `attach.BinderImpl.Bind` is tested with a stub and no daemon.

**One provider, one scheme.** `Targets.Scheme()` is the whole of the dispatch:
`WithTargets` is variadic and keys the providers by what each one says it
answers, so there are no keys to keep in step with values, and `Bind` looks a
scheme up rather than branching on it. A scheme no provider claims and that is
not `http`/`https` is an error there — dialing `file://htop` as an address
would mint a hostname in front of nothing. `Target.Scheme()` is the other half:
the frame reconstructs the origin as typed from `Scheme()` and `Name()`, which
is why `dockerd://` is not spelled anywhere in `frame.go`.

A served origin's reference is its authority **or** its path, never both, and
`Host + Path` is how everything downstream reads it — `Bind`, the multiview
tile, the frame. A container is always an authority (`dockerd://api`); a
program is either (`file://htop`, `file:///usr/bin/htop`), and an absolute path
has to be the URL's path because `url.URL` percent-escapes the separators of a
host. The parser resolves a bare word to its absolute path for exactly that
reason: the origin then names the same program on any machine that reads it.

The parser has to agree about which schemes are served, and `servedSchemes` in
[`v1alpha1/builder.go`](./v1alpha1/builder.go) is that list. A scheme added to
one and not the other is an origin the parser drops before the binder ever sees
it, or one the binder refuses after the parser waved it through.

A provider does not read its own stream. `attach.CopyOutput` does, and it is
where the wire format is agreed: raw with a terminal, and Docker's 8-byte
multiplexing header without one, since a provider that has no terminal has two
streams to carry over one. It is also where the several ways a transport
spells "the stream ended" — EOF, a closed socket, a closed file, `EIO` from a
pseudo-terminal — become the nil that says an attach finished normally.

The assertion that a provider satisfies `Target` and `Targets` lives in the
provider, never beside the interfaces: both providers import `attach`, so
naming one from there is an import cycle.

Two things there will bite if you change them without knowing why:

- **The page builds its socket URL as `"/attach" + location.search`.** A
  websocket handshake carries no `Referer`, so libtunnel cannot route it as a
  subresource and would fall back to the sticky `libtunnel-origin` cookie,
  which is last-write-wins across tabs. Drop the suffix and two container tiles
  fight over one socket.
- **`attach.BinderImpl.Bind` keeps the dialable list the same length and order
  as the displayed one.** Index *n* means origin *n* for `?n` routing,
  `publicURL`, the reported map and the multiview tiles. Reordering or
  filtering either list breaks all four at once.
- **One attach per `Server`, not per page.** `attach.session` opens the target
  once, so a refresh is not an event the container can see. Every byte goes
  through a terminal emulator, and a viewer arriving late renders the screen —
  cells, cursor, scrollback — rather than replaying the bytes that once
  produced it. Replaying bytes into a fresh terminal is what used to leave the
  app and the browser disagreeing about where the cursor was, so the app's next
  redraw landed at the wrong origin and drew over the restored screen.
- **One frame per viewer, one emulator between them.**
  [`attach/frame.go`](./v1alpha1/attach/frame.go) is a Bubble Tea model
  rendering the emulator that [`session.go`](./v1alpha1/attach/session.go)
  feeds. Per viewer, because command mode is per viewer — a shared model would
  put everyone into it when one person opened it — and because a frame
  that is new renders a whole screen, which is what a late joiner needs anyway.
  The split is worth keeping: `session.go` is locks, pipes and goroutines,
  `frame.go` is a value type with none of them.
- **A frame has no terminal to measure.** Its output is a websocket, so the
  renderer's first size report is zero, and a renderer that believes it has no
  rows draws none. The frame waits `sizeGrace` before answering, because the
  session's settled size is a guess about *somebody else's* window: answered at
  once, a joining viewer's first frame is a box of the wrong width with the
  cursor somewhere inside it, redrawn as soon as the page says how big it
  actually is. Nothing is the better first frame, and the renderer paints
  nothing at zero on its own. The answer still has to come as a message rather
  than a size pushed in from outside — whichever landed second would win, and
  when that is the zero nothing is ever drawn again.
- **The frame composes into a buffer, and everything is copied in.** Neither
  `vt.Emulator.Draw` nor `uv.StyledString.Draw` clips to the area it is handed
  — both clip to the *screen* — so drawn straight into the frame's buffer,
  anything larger than its area paints over the border and out of the window.
  That is the ordinary path, not a corner case: a viewer whose window shrinks
  renders once with the new pane and the old emulator. Both go through a buffer
  of their own size and are blitted, which is also what makes a label truncate
  instead of erasing the border to its right.
- **The public address arrives after the servers do, by assertion.** A
  `dockerd://` origin is bound *before* the tunnel is minted — the binding is
  what the tunnel is handed to proxy to — so at the only moment `Bind` could be
  told where it answers from outside, nobody knows. `RunE` asks the closer
  `Bind` returned whether it is an `Announcer` once `public` is known, and
  hands over one address per origin, indexed the way `display` was. It is an
  assertion rather than a method on `Binder` because of the import direction:
  `attach` cannot name a type declared in `v1alpha1`, so a contract mentioning
  one could never be satisfied from there. Same shape as `http.Flusher`.
- **The build line is configuration, the address is an announcement.** Both are
  root knowledge a subpackage cannot work out, but the banner names the command
  — which an embedding program renames — and versions the root resolves from
  build information, and none of it changes while the process runs. So it
  arrives through `attach.WithBanner` at construction, where the address has to
  arrive later through `Announcer`.
- **A hyperlink needs both ends.** The frame marks its address with OSC 8, and
  `index.html` sets xterm's `linkHandler` — without one xterm underlines the
  link and does nothing when it is clicked, which is worse than not marking it.
  The handler opens with `noopener,noreferrer`, because the container's output
  reaches this terminal and an origin that printed its own OSC 8 would
  otherwise be handed a reference to the window.
- **The page needs the WebGL renderer, and it is not an optimization.** xterm's
  DOM renderer draws every cell as text in a clipped row, which a full-screen
  program's box drawing does not survive: at the page's font size U+2502
  carries 16.5px of ink through a 15px row, so a vertical rule loses its ends
  once per row and reads as dashes, and a fractional cell advance puts each
  row's glyphs on different device pixels so the rightmost columns stop lining
  up. The addon draws box-drawing and block characters itself, sized to the
  cell. xterm's own `customGlyphs` option says it "doesn't work with the DOM
  renderer"; VS Code's terminal is the same library making the same choice.
  Loading it is allowed to fail — a context the machine will not give, a GPU
  that takes it back later — and disposing falls back to the DOM renderer,
  which is a worse terminal and still a terminal.
- **A cursor is drawn only when the program wants one.** The emulator answers
  with a position whether or not anything should be shown there, and a
  full-screen program hides the cursor at startup and then leaves the position
  wherever its last write ended. `session.watch` records DECTCEM and the frame
  asks before drawing, or the cursor skates around the screen on every redraw.
- **Everything the terminal says about itself is in the debug log.** The
  `vt.Callbacks` block in `session.watch` logs titles, working directory, bell,
  modes, cursor and colour changes, because the only way to learn what a given
  app sends is to watch one send it — Claude Code, for instance, sets no title
  at its login screen but does once a session is running. `CursorPosition` is
  deliberately absent: it fires on every cursor move and would drown the rest.
  All of them run with the emulator's lock held, so they may only stash a value
  or write a line.
- **A terminal has a title and a subtitle, and they are not the same thing.**
  The window title (OSC 2) is the title; the tab title (OSC 1) is the subtitle.
  A prompt framework sets the first to the running command's whole line and the
  second to its name; an app setting both with one OSC 0 sets them identically.
  The frame joins them when they differ and says one when they do not, and the
  same pair names the browser tab through `View.WindowTitle` — ahead of the
  origin, because a tab loses its end and a row of them all starting
  `dockerd://` would say nothing.
- **A title is arbitrary text from somebody else's program.** It is drawn over
  the top border, so anything in it that measures wide and paints blank —
  control characters, zero-width joiners, a byte that is not a character —
  clears the border and leaves a hole in the box. `frame.title` drops invalid
  UTF-8 and then reduces what is left to printable runes, before any of it is
  measured.
- **`x/vt` ends an OSC string at a `0x9C` byte, which breaks some UTF-8.**
  `0x9C` is the 8-bit string terminator, and it is also the middle byte of
  every three-byte UTF-8 character in `U+27xx`. Claude Code's spinner cycles
  `✳ ✻ ✽ ✢` — all `E2 9C xx` — so its title arrives as the single byte `E2`,
  and the rest, ` Claude Code`, is printed onto the screen. `◐` (`E2 97 90`)
  and `café` (`C3 A9`) are unaffected, so the title is good, then a stray byte,
  then good again, in time with the spinner. `session.setTitle` keeps the last
  usable one rather than taking the stray, which is what stops the label
  flickering; the text landing in the pane is not fixable from this side.
- **The shell's title is caught, not guessed.** `vt.Callbacks{IconName:…}`
  catches the OSC the container already emits — a prompt framework sets it from
  `preexec`, so it carries the running command's name — and the frame shows it
  as it arrives. The tab title (OSC 1) rather than the window title (OSC 2):
  the same fact said shorter, where the window title is the whole command line
  and, at rest, `user@host:~`, which in a frame that already names the host and
  the origin is mostly things said twice. There is no marker separating "a command is running" from "this
  is the prompt", so interpreting it would mean guessing at somebody's shell
  configuration. It is stashed under `titleMu` rather than `mu`, and that is
  not fastidiousness: the callback fires with the *emulator's* lock held, while
  `negotiate` takes `mu` and then reaches for that same lock — the two orders
  that deadlock. Nothing is woken from the callback either, because a title
  only changes as part of output and `sink` wakes everybody when that write
  returns.
- **A paste is a message, not keystrokes.** The frame's renderer turns
  bracketed paste on in the viewer's terminal, so the browser stops sending
  pasted text as a burst of keys and sends it wrapped instead. A model that
  only answers `KeyPressMsg` swallows it and pasting does nothing at all. It
  goes to the container through `em.Paste`, not `SendText`: the emulator read
  the app's own `\x1b[?2004h`, so it is the only thing here that knows whether
  *this* app wants its pastes bracketed, and text written past it arrives as
  though it had been typed — which is a shell running a half-finished command
  off a pasted newline.
- **The frame's key is `Ctrl+K`, and the page is in the way of it.** Not
  because xterm could not encode it — it could — but because the browser claims
  the chord for its address bar, so `index.html` `preventDefault`s it and sends
  the byte itself. The byte is the one a terminal sends, so `frame.go`'s rule
  is about a key rather than about a private signal, and a keystroke that
  reaches xterm some other way still works. It costs the program
  kill-to-end-of-line, which is the cheaper of the two keys on offer: `Ctrl-D`
  reaches the shell and ends a shared session for everyone, and
  `frame.commanded`'s `q` is how to ask for that on purpose.
- **A viewer arriving after the run ended starts the next one.** Only for a
  target that implements `attach.Repeatable` and says yes, which today is a
  program: its origin is a path, so running it again is as well defined as
  running it the first time. `session.stream` builds one run — the pipe, the
  replies goroutine, the `done` channel — and `session.revive` builds another,
  which is why `stdin` and `done` are guarded by `mu` and read through
  `ended()` rather than off the field. The screen is reset with RIS rather than
  replaced, so every viewer keeps drawing the same emulator; a screen carrying
  the last program's output would claim a state the new one was never in. A
  container implements none of this: once PID 1 exits there is nothing to
  attach to, and the frozen final screen is the honest thing to serve.
- **Ctrl-C and Ctrl-D are asked for twice, and only where nothing can come
  back.** A program origin lets them straight through: the origin is a path, so
  the cost of a mistake is opening the page again, and a terminal that argues
  with Ctrl-C is not a terminal. A container cannot come back, so the frame
  holds the first press and says in its border which key is waiting — a key
  that appears to do nothing reads as a key that is broken. `session.recoverable`
  is the split, and it is `attach.Repeatable` answering. The border names the
  key and says to press it again, and nothing about what it will do: most of
  the time these end nothing — Ctrl-C at a shell prompt clears the line — and
  which time this is, is not knowable from here. The arming lets go after
  `armGrace`, and the tick carries the arming it belongs to so a spent one
  cannot disarm the next.
- **Zero origins means `$SHELL` when `--shell-fallback` allows it, resolved
  before it is adopted.** The knob is asked before the variable is read, so a
  run that declines the fallback never consults the environment at all — which
  is the point: it is how a caller says "nothing to expose" and means it, and
  how a test says the same without unsetting a variable the run reads behind
  its back. `e2e` still strips `SHELL` in `strippedEnv` rather than passing the
  flag, because the flag is itself under test and a case proving it works has
  to start from a run that would otherwise fall back. The parse
  loop's fallback for a word it cannot resolve is to read it as an address, so
  an unrunnable `$SHELL` would become a proxy to `http://localhost/bin/nope` —
  a tunnel to nothing that reports no problem. `shell.Resolve` runs first and a
  failure leaves the count at zero, which already has a message naming the
  lever. Argv, `TUNNELD_ORIGINS` and a `WithOrigin` seed all settle above it.
- **The browser decision is derived, and split where the knowledge is.**
  `--no-open` and `TUNNELD_NO_OPEN` are gone, and so is the gate at the call
  site. `Browser.Open` decides, because putting the tunnel in front of a
  person is that package's job and there are two ways to do it: a tab, or the
  console the run was started from. The run hands it `browser.When` — a
  console to draw on if there is one, the caller's own instruction, and
  whether any of the command's streams is a terminal — and everything else it
  needs is knowledge about this machine (`$CI`, ssh, the display variables),
  which is the browser package's own. Facts in, no verdict, so there is no
  "should I" for a caller to get wrong and no second place where this is
  decided. Order inside is load-bearing: `Mirror` comes before `Forced`,
  because a console already showing the thing is a fact about the run rather
  than an opinion about it. Every branch logs its reason at debug, the only
  record of a decision nobody typed.

  `Open` decides but does not draw. `v1alpha1/console` owns what a console
  costs on the way in and out — a log ring that must stop writing through a
  full-screen frame, and a prompt that has to be told the tunnel is still up
  once the frame gives it back — so a package about browsers is not also the
  thing that runs a terminal. `browser` imports `console` and not the other
  way round: a tab and a console are two ways of doing one thing, and the
  package that chooses between them is the one that names the other, so
  `console.Drawer` is declared once rather than twice.

  **Nobody counts origins to find out whether a console can draw.** `Bind`
  answers by carrying the method: a closer satisfies `console.Origin` only
  when it wrapped exactly one server, which is one served origin since nothing
  else gets one. `console.For` asks that question and the stream question
  together and returns nil for either no — nil being load-bearing, because
  `browser` reads it to decide whether a tab is what this run gets instead.
  It returns `console.Drawer` rather than `*ConsoleImpl` for the same reason:
  a nil pointer in an interface field is not nil, and the guard on the other
  side would wave it through.

  The seam moved the tests with it. `browser` owns the decision, so
  `TestOpenDecides` drives `Open` with a recording `WithLaunch` and asserts
  whether the attempt was made. `v1alpha1` owns reporting the facts, so its
  cases read `h.browser.when` — a fake browser never runs the decision, and a
  case there asserting "nothing opened" would be asserting nothing at all.
  `h.run` sets stdin either way, since cobra otherwise falls back to the
  process's own, which is a terminal when the suite is run from one.
- **`RunE` is one line; the run is `Run`.** The body used to be a 357-line
  closure inside `Command`'s struct literal, reachable only by executing a
  cobra command. It is a method now, and `RunE` calls it with `cmd.Context()`.
  Two consequences worth keeping: `Run` assembles the command itself rather
  than taking one, because `Command` is cached and that is where the streams
  and the flag values live — from inside `RunE` it is the same command already
  running. And `applyEnv` is called from both `PersistentPreRunE` and `Run`,
  because nothing runs `PersistentPreRunE` for a direct caller and env has to
  beat code on both paths. It is idempotent: a flag it set is marked `Changed`,
  and a `Changed` flag is skipped.
- **The console is a viewer, and the gate is where the care is.**
  `session.viewLocally` is `AttachContainer` without the two things that exist
  only for a websocket: the stated colour profile and TERM, which a real
  terminal answers for itself, and the resize channel, which is Bubble Tea's
  job from SIGWINCH — `follow` still runs with a nil channel, for the other
  thing it does, which is ending the viewer when the run does. What decides
  whether to draw at all is `mirrorable` in the builder: one origin, a served
  scheme, and a terminal on both of the command's own streams — checked there
  and not on `os.Stdin`/`os.Stdout`, because an embedding program redirects
  them. It is decided before the browser because the browser is told about it:
  a mirrored run opens no tab, `WithOpen(true)` or not. Its first two checks —
  one origin, served scheme — are what the binder already worked out, since it
  stands a server up only for a served origin and `bound.Mirror` refuses
  anything but a list of one. They are said again because the answer is needed
  before the mirror starts: the browser is told, and the log ring is muted.
- **A drawing console mutes the log ring.** stderr writes straight through a
  full-screen frame. `recent.Mute(true)` stops records reaching the handler
  while the ring keeps every line, so nothing is lost and `^K l` is where they
  are read; the mirror unmutes on its way out, so a detached console gets its
  logs back with its prompt.
- **The log ring wraps rather than tees.** `logs.RingImpl.Wrap` sits in front
  of the text handler, so what a terminal shows is what stderr got and neither
  can drift. It asks what it wraps through `Enabled`, so a run at `--log-level`
  silence keeps nothing. It is built in `v1alpha1.New`, before the command
  knows where logs go or at what level, because the binder is constructed there
  too and both need the same one.
- **A keystroke can end the process, and the path is deliberate.** `x` in the
  frame calls `session.endRun`, which closes the Server's `quit`; `bound.Quit`
  fans every origin's into one, because what they are asking for is the
  process and there is only one; and `RunE` selects on it through the
  `Quitter` contract, discovered on the closer the way `Announcer` is. Nothing
  in `attach` acts on it — a frame can end its own viewer, and ending a run is
  the command's, which is what owns the context everything else hangs from.
- **The page asks whether coming back is worth offering.** A socket ending
  says nothing about why: this viewer's own connection dropping leaves a
  terminal still running, and a container whose shell exited leaves nothing at
  all, and both arrive at the overlay identically. So `gone()` fetches
  `/alive`, which is 204 while a run is up or the target can be started again
  and 410 when it cannot, and the button stays hidden until the answer comes.
  `make run attach` is the case: zsh is the image's entrypoint, so Ctrl-D ends
  PID 1 and the container with it.
- **The page says what pressing it will do.** `restart` for a target that can
  be started again, `reconnect` for one that cannot — the template picks from
  `session.recoverable`, the same answer that decides whether a viewer's
  arrival starts anything. Reconnecting to a stopped container gets the last
  screen and nothing else, and a button promising otherwise is a lie the page
  tells once per visit.
- **A run is told its size when it starts, whether or not anything changed.**
  `negotiate` only speaks when the window moves, and the viewer who asks for a
  restart is the size the session already settled on — so a second run would
  sit at whatever `pty.Start` made, which is nothing, and a full-screen program
  with no room draws an empty screen. `stream` pushes `paneOf(s.size)` at every
  run for that reason.
- **A provider stops reading the resize channel when its attach ends.** The
  channel belongs to the session and outlives one run, so a reader that only
  stopped when it closed goes on taking sizes meant for the run after it —
  which is the same blank screen, arrived at from the other side. Both
  providers select on a context cancelled by their own return.
- **`s.Target.AttachContainer`, never `s.AttachContainer`.** A session has an
  `AttachContainer` of its own — the per-viewer one — so the embedded Target's
  is shadowed, and the short spelling has the session attach to itself.
- **Software flow control is off on a pty this package creates.** A pty arrives
  with `IXON` set, so `Ctrl-S` never reaches the program: the line discipline
  eats it and stops the program's writes, which freezes the screen for every
  viewer at once with the session perfectly healthy behind it. Flow control is
  there to stop a sender overrunning a serial line, and there is no serial line
  — the path is a websocket over a tunnel with a pipe and an emulator in it,
  all of which buffer or block on their own — so `shell.unmeter` clears `IXON`,
  `IXOFF` and `IXANY` before the program writes a byte. Only available for a
  program origin: a container's tty belongs to the container.
- **Keys go to the container through the emulator, not around it.**
  `session.sendKey` hands the decoded key to `vt`, which encodes what a
  terminal in the app's current modes would send; bytes written straight to
  stdin would not know whether the app had asked for application cursor keys.
  `asKeyEvent` is exact rather than approximate — Bubble Tea's `Key` and
  ultraviolet's carry the same fields, and Bubble Tea's key codes *are*
  ultraviolet's constants.
- **The emulator's replies have to be drained.** It answers a device-attributes
  query or a cursor-position report the way a real terminal does, and those
  answers go back to the app through the same stdin the viewers type on. Leave
  them unread and the buffer fills, the write that fills it never returns, and
  the terminal stops drawing for everyone — a deadlock, not a slow path.

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
