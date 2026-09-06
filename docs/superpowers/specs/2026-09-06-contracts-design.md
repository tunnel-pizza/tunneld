# One contract per collaborator, one `XImpl` each, one way to configure any of them

Design for [#38](https://github.com/tunnel-pizza/tunneld/issues/38). Written
2026-09-06, against tunneld v0.0.16 (29e979c, `feat: stop when the edge
disowns the tunnel`).

## The problem

`v1.Builder` ↔ `v1alpha1.BuilderImpl` follows the template's contract/impl
split: an interface names what a caller depends on, an `XImpl` carries it,
`New` constructs it. Nothing added since follows that shape, and each thing
`run` composes has ended up with a different one — package-level functions
(`multiview`, `env`), a concrete exported struct (`Counter`), an interface with
an impl named for something else (`attach.Target` ← `docker.Attacher`), and two
package-level variables standing in for seams (`openURL`, `openTarget`).

The cost is coverage. `run` in `v1alpha1/tunnel.go` is the function that
composes all of them and it has no unit test, because nothing lets a test hand
it a fake edge, a fake cache or a fake daemon. Everything up to the mint is
tested; the composition is not.

A second, smaller inconsistency sits underneath: the builder is configured by
fluent `With*` methods that return the `v1.Builder` interface, so any knob on
the concrete type that the interface does not name — a fake engine for a test,
a threshold on the counter — has to be chained *before* the interface's own
setters, or it is unreachable. libtunnel documents that rule for its backend;
this design removes the need for it.

## Decisions

Each was chosen deliberately; the alternative is named so a later reader can
tell a decision from an accident.

**The contracts are internal.** They live in `v1alpha1`, not `v1`. Only a
contributor swapping an implementation or a test faking one has any use for
them; an embedding program builds a `*cobra.Command` and runs it. Promoting
them to `v1` would grow the stable surface by six interfaces and pull
`k8s.io/cri-streaming` into the one package that promises not to change.
Rejected: `v1` placement, and a mixed split with some public. This is the
"surface/engine split" CONTRIBUTING already describes, made concrete.

**Every implementation is named `XImpl`.** `cache.CacheImpl`,
`panel.PanelImpl`, `docker.TargetsImpl`. One rule regardless of where the
type lives, greppable, and the same rule `BuilderImpl` already follows.
Rejected: libtunnel's bare package-qualified name in subpackages
(`cloudflare.Backend`), which is a second rule keyed on location; and keeping
domain names (`Attacher`), which is the inconsistency being removed.

**Configuration is functional options, and there is one option type.**
`v1.Option[T any]` is `func(T)`; every package aliases it to its own
`Option` and exposes `With*` constructors returning that alias; every `New`
takes `opts ...Option`. The builder's knobs, the contract injectors and each
implementation's own tunables all go through the same door. Rejected: the
fluent setters, whose interface-typed return is what created the chain-order
wart; a non-generic `Option` per package, which is the same idea declared
six times; and options for wiring only with the fluent `With*` kept for flag
seeds, which is two mechanisms where one does.

**`v1.Builder` shrinks to what a caller calls.** `Build` and `Name`. The
`With*` methods leave the interface because they are construction-time
configuration, and construction now happens in `New`. The interface is still
the seam a host program declares a field with. Rejected: keeping the `With*`
methods as well, which would leave two ways to set every knob.

**Every `New` has the same signature, knobs or not.** `cache.New(opts
...Option) *CacheImpl` exists today with no `With*` to pass it. The alias
costs a line, the variadic costs nothing, and a tunable added later does not
change a signature anybody calls. Rejected: `New()` for the knobless
packages, which is a second constructor shape to remember and a signature
change waiting to happen.

**Defaults are options, applied first.** `New` applies its own defaults, then
the caller's options, in one pass. What a reader sees in `New` is the list of
defaults in the same vocabulary the caller uses to override them, and a
caller's option wins by coming later. That is the whole precedence rule, and
it composes with the two tiers that already exist: default → caller option →
environment (`applyEnv`) → flag (argv), each overwriting the last.

**Flag-settled configuration rides as method arguments.** `--cache-dir`,
`--multiview` and `--provider` land on `BuilderImpl` fields at parse time, after
`New` has already seeded the collaborators. Passing `dirs`, `enabled` and
`provider` per call keeps every implementation but `Counter` a stateless
singleton constructed once in `New`. Rejected: constructor arguments, which
would force `run` to construct collaborators late and `BuilderImpl` to hold
factories — the same shape as the package variables this removes, one level
down.

**One contract per collaborator, not one bundle.** Six fields and six options
rather than a `Deps` struct. A test that wants to fake the edge should not have
to fill in a daemon, and the option a fake lands on should say which seam it
is.

**The pure transforms stay functions.** `parseOrigins`, `PublicURL`,
`wsMarked`, `Version`, `VersionLine`, `EnvBool`, `EnvDuration`, `Logger`,
`splitEnvList`, `stripFraming`. The governing rule, to be written into
CONTRIBUTING: a contract exists for each thing `run` composes that owns an
external effect — the edge, the disk, the daemon, the browser, an HTTP probe.
A function that maps a value to a value gets no interface.

**`attach.Target` stays where it is.** `attach` consumes it and `docker`
provides it, so the consumer owns the interface — the Go idiom, and already
the case. What moves into the root is the contract for *obtaining* a Target
from a reference, which is what `run` actually depends on.

**`attach.Server` stays concrete.** One implementation, exercised directly
with a fake `Target` on a loopback listener. An interface over it would have
one impl and no fake.

**Behaviour through `New` and the command line does not change.** The public
URLs, the report, the cache file, the open-in-browser probe, the
credential-rejected retry, the gone-verdict exit — every line of `run` keeps
its effect. This is a structural change verified by the existing tests staying
green and by the new `run` table asserting what the old code did. The one
deliberate exception is a `BuilderImpl{}` assembled as a bare struct: today it
runs with a counter that never trips, after this it returns an error naming
what `New` would have seeded. `New` is the documented entry point and the bare
struct was never a supported one; failing it clearly beats a tunnel that
silently loses its gone-verdict.

**Backwards compatibility is not a constraint.** Confirmed for this change.
The embedder's call changes shape, from `New().WithURL(u).Build()` to
`New(WithURL(u)).Build()`; `v1.Builder` loses nine methods;
`docker.Attacher`, `docker.Open`, the `env` and `multiview` packages,
`Counter` and `NewCounter` are renamed or removed without deprecation shims.

## Contracts

All six in `v1alpha1/v1alpha1.go`, beside `BuilderImpl`. `v1.Logger` (the
`*slog.Logger` alias) throughout.

```go
// Engine mints or replays the tunnel run drives. spec is a cached envelope to
// replay, "" to mint; provider is the quick-tunnel host, "" for the default.
type Engine interface {
	Tunnel(spec, provider string) libtunnel.TunnelV1
}

// Cache persists a tunnel's spec between runs, in the directories
// --cache-dir settled on.
type Cache interface {
	Cached(dirs []string, log v1.Logger) string
	Save(dirs []string, log v1.Logger)
	Discard(dirs []string, log v1.Logger)
}

// Panel serves several origins as one page on the tunnel's bare address.
type Panel interface {
	Wanted(enabled bool, origins []*url.URL) bool
	URL(public *url.URL) string
	Interceptors(origins []*url.URL, log v1.Logger) []libtunnel.Interceptor
}

// Opener puts a public address in front of a person once the edge serves it.
type Opener interface {
	Open(ctx context.Context, addr string, stderr io.Writer, log v1.Logger)
}

// Counter folds tunnel events into a verdict: has the edge disowned it.
type Counter interface {
	Count(e libtunnel.Event)
	IsGone() bool
}

// Targets opens a container reference as something attach can serve.
type Targets interface {
	Open(ctx context.Context, ref string, log v1.Logger) (attach.Target, error)
}
```

Notes on the shapes:

- `Engine.Tunnel` absorbs the `os.Setenv(CloudflareProviderEnv, …)` side
  effect the current `engine()` method performs; that stays an implementation
  detail of `EngineImpl`.
- `Opener.Open` folds `awaitReachable` and `openInBrowser` into one call. The
  probe window (`reachableWithin`, 10s) and its cadence become the
  implementation's own, tunable by its options. `stderr` is there so the
  implementation can point `pkg/browser`'s process globals at it before the
  child can write a word — the existing behaviour.
- `Panel.Interceptors` returns the panel interceptor and the unframe
  interceptor together, in that order, so `run` registers them with a loop and
  the ordering invariant (panel priority 1, unframe priority 2) is the
  implementation's to keep and test.
- `Counter.Count` returns nothing. Today it returns the concrete counter so
  `Count(e).IsGone()` chains; a contract cannot offer that, because Go
  requires an implementation's return type to be *identical* to the
  interface's, and a subpackage cannot name the root's `Counter` without
  importing the root. Two statements in `events` instead of one — the
  verdict is a separate question anyway.
- `Targets.Open` returns `attach.Target`, the interface, exactly — a method
  returning `*TargetImpl` would not satisfy it. The concrete is reachable
  through an unexported `open` for the package's own tests.

## The option type

In `v1/v1.go`, beside `Builder`, because it is part of how a caller obtains
one:

```go
// Option configures a value of type T while it is being constructed. Every
// New in v1alpha1 and its subpackages takes a list of them, applies its own
// defaults first and the caller's after, so the caller's always wins. A
// package aliases the instantiated type to its own Option and exposes With*
// constructors returning that alias.
type Option[T any] func(T)

// Apply runs opts against t in order and returns t, so a constructor is one
// expression: Apply(&XImpl{}, defaults...) and then the caller's options.
func Apply[T any](t T, opts ...Option[T]) T

// Builder assembles the tunneld command. Obtain one from v1alpha1.New,
// configured by its options, and call the terminal Build.
type Builder interface {
	Build() *cobra.Command
	Name() string
}
```

`Option` and `Apply` are `v1` rather than `v1alpha1` for the same reason the
sentinels are: the subpackages cannot import the root, and every one of them
needs the type. `Apply` returning `T` is what lets `New` be a return
statement; the variadic is what lets defaults and caller options be two calls
with nothing between them.

Per package, one alias and the constructors:

```go
type Option = v1.Option[*BuilderImpl]

func WithName(name string) Option
func WithURL(urls ...string) Option
func WithProvider(host string) Option
func WithCacheDir(dirs ...string) Option
func WithLogLevel(level string) Option
func WithOpen(open bool) Option
func WithMultiview(multiview bool) Option
func WithStdout(w io.Writer) Option
func WithStderr(w io.Writer) Option

func WithEngine(e Engine) Option
func WithCache(c Cache) Option
func WithPanel(p Panel) Option
func WithOpener(o Opener) Option
func WithCounter(c Counter) Option
func WithTargets(t Targets) Option
```

The first nine are the current `v1.Builder` methods, with their documentation
moved onto the constructors word for word: what each seeds, that a flag
overrides it, that `WithURL` appends across calls and a `--url` replaces the
whole set. The semantics are the setters' semantics; only the spelling at the
call site changes. `WithCacheDir` keeps its boolean-entry and
absolute-path logic inside the closure.

An option is a plain function, so it can be applied anywhere a setter used to
be called: `cacheDirValue.Set` becomes `WithCacheDir(s)(v.b)`, and the seed
in `command` becomes `WithCacheDir("")(b)`. No unexported twin of any option
is needed.

## Layout

```
v1/v1.go                  CHANGED Builder shrinks to Build + Name; Option and
                                  Apply added; Err* and constants unchanged
v1alpha1/v1alpha1.go              New, BuilderImpl, Option alias, the six
                                  contracts, the six contract options, and the
                                  compile-time assertion block
v1alpha1/builder.go               the nine builder options, Build, command,
                                  cacheDirValue, defaultCacheDir, boolish
v1alpha1/tunnel.go                run, events, report, PublicURL, parseOrigins,
                                  wsMarked, logger
v1alpha1/origins.go               bindOrigins(ctx, targets, display, log), bound
v1alpha1/env.go                   unchanged — EnvBool, EnvDuration, Logger,
                                  flagEnv, newEnv, applyEnv
v1alpha1/version.go               unchanged
v1alpha1/engine/          NEW     EngineImpl, Option, New — was the engine() method
v1alpha1/counter/         MOVE    CounterImpl, Option, New, WithMaxGone,
                                  DefaultMaxGone — was counters.go
v1alpha1/cache/cache.go   RENAME  CacheImpl, Option, New, File — was env/env.go
v1alpha1/panel/           RENAME  PanelImpl, Option, New — was multiview/; shell()
                                  and unframe() unexported; index.html moves with it
v1alpha1/browser/         NEW     OpenerImpl, Option, New, WithLaunch, WithWindow —
                                  the probe and the launch
v1alpha1/attach/                  unchanged — Target, Server, Serve
v1alpha1/attach/docker/           TargetsImpl, Option, New (was Open);
                                  TargetImpl (was Attacher)
```

The root holds the builder, the contracts and `run`; every implementation
lives in a subpackage with a `New` of its own. No exceptions, so the rule can
be stated in one line.

The import graph stays acyclic and keeps its direction: the root imports every
subpackage to seed defaults, as it does today; no subpackage imports the root.
`docker` newly imports `attach` for its return type; `attach` still knows
nothing about `docker`. Every subpackage now imports `v1` for `Option` and
`Apply`; `docker` and `cache` already did. The `browser` package imports
`github.com/pkg/browser` under the alias `pkgbrowser`.

`TUNNEL.env` keeps its filename; only the package around it is renamed, so
`.gitignore`, `make clean` and the CONTRIBUTING warning about it are untouched.

The `env` → `cache` rename also ends the collision between the `env` package
(spec cache) and `env.go` (environment-variable helpers).

## Wiring

`BuilderImpl` gains six fields, grouped and documented as the collaborators
`run` composes:

```go
	engine  Engine
	cache   Cache
	panel   Panel
	opener  Opener
	counter Counter
	targets Targets
```

`New` is its defaults, then the caller:

```go
func New(opts ...Option) *BuilderImpl {
	b := v1.Apply(&BuilderImpl{},
		WithOpen(v1.DefaultOpen),
		WithMultiview(v1.DefaultMultiview),
		WithEngine(engine.New()),
		WithCache(cache.New()),
		WithPanel(panel.New()),
		WithOpener(browser.New()),
		WithCounter(counter.New()),
		WithTargets(docker.New()),
	)
	return v1.Apply(b, opts...)
}
```

Two calls rather than one `append`, so the two tiers read as two tiers. The
booleans are seeded here for the reason the current `New` gives — a bool
field cannot say "unset" — and now in the same vocabulary as everything else.

`counter.New()` comes back armed: its threshold is `counter.DefaultMaxGone`
(3), not the never-reached ceiling `NewCounter` seeds today. The default
belongs to the counter, so it is declared there, and the builder no longer has
to know a number to wire one in — which is what the `TODO: var-ify this` was
asking for. `counter.WithMaxGone` stays for a caller that wants a different
threshold, and a value of zero or less still disables the counter by restoring
the ceiling. It is not a `v1` constant: there is no flag or variable for it,
so it is not part of the operator-facing contract.

`BuilderImpl` keeps two exported methods, `Build` and `Name`, and its
unexported `run`, `events`, `logger` and `command`. The nine `With*` methods
become the nine options in `builder.go`.

Compile-time assertions, in `v1alpha1.go`, so a default that drifts from its
contract fails the build rather than the first run:

```go
var (
	_ v1.Builder = (*BuilderImpl)(nil)
	_ Engine     = (*engine.EngineImpl)(nil)
	_ Cache      = (*cache.CacheImpl)(nil)
	_ Panel      = (*panel.PanelImpl)(nil)
	_ Opener     = (*browser.OpenerImpl)(nil)
	_ Counter    = (*counter.CounterImpl)(nil)
	_ Targets    = (*docker.TargetsImpl)(nil)
)
```

and `var _ attach.Target = (*TargetImpl)(nil)` in `docker.go`.

`run` changes are substitutions, each preserving the call's effect:

| Today | After |
|---|---|
| `b.engine(spec)` | `b.engine.Tunnel(spec, b.provider)` |
| `env.Cached(b.cacheDirs, log)` etc. | `b.cache.Cached(b.cacheDirs, log)` etc. |
| `multiview.Wanted(b.multiview, origins)` | `b.panel.Wanted(b.multiview, origins)` |
| `multiview.URL(public)` | `b.panel.URL(public)` |
| `tun.WithInterceptor(multiview.Panel(…)); tun.WithInterceptor(multiview.Unframe())` | `for _, ic := range b.panel.Interceptors(origins, log) { tun.WithInterceptor(ic) }` |
| `awaitReachable(ctx, target, reachableWithin, log); openInBrowser(target, stderr, log)` | `b.opener.Open(ctx, target, stderr, log)` |
| `bindOrigins(ctx, origins, log)` | `bindOrigins(ctx, b.targets, origins, log)` |
| `b.counter.Count(e).IsGone()` | `b.counter.Count(e)` then `b.counter.IsGone()` |

`run` opens with one check that every collaborator is non-nil, returning an
error naming the missing one for a `BuilderImpl` assembled as a bare struct
rather than through `New`. That replaces the nil guard `events` carries today
for `counter` alone: one check at the top, in the function that already
returns errors, instead of a guard in a callback that cannot. The package
variables `openURL` and `openTarget` are deleted, along with `awaitReachable`,
`openInBrowser`, the `reachable*` constants and the `engine` method, all of
which move into their implementations.

Implementation shapes, each with `type Option = v1.Option[*XImpl]` and
`New(opts ...Option) *XImpl`:

- **`engine.EngineImpl`**: empty struct, no options. `Tunnel` is the current
  `engine` method verbatim with `provider` as a parameter.
- **`counter.CounterImpl`**: the current `Counter` struct renamed and moved.
  `New` applies `WithMaxGone(DefaultMaxGone)` then the caller's, so an
  unconfigured counter trips. `WithMaxGone` becomes an option carrying the
  same zero-or-less-disables logic. `Count` no longer returns the counter
  (see the contract notes). The bare-struct `IsGone` guard stays, and its test
  with it.
- **`cache.CacheImpl`**: empty struct, no options; the three functions become
  methods. `File` stays exported. The package doc is rewritten for the new
  name and keeps its argument for why a file of variables is the whole cache.
- **`panel.PanelImpl`**: empty struct, no options. `Wanted` and `URL` become
  methods unchanged. `Interceptors` returns `[]libtunnel.Interceptor{shell(origins,
  log), unframe()}` where `shell` and `unframe` are the current `Panel` and
  `Unframe` made unexported, so the existing tests of each keep their shape.
  `shell` rather than `panel`, because `panel.panel` would name the package
  and the function with one word, and the template it serves is already
  called `shell`.
- **`browser.OpenerImpl`**: two fields, `launch func(string) error` and
  `window time.Duration`, seeded by `New` with `WithLaunch(pkgbrowser.OpenURL)`
  and `WithWindow(10 * time.Second)`. `Open` is `awaitReachable` followed by
  `openInBrowser`, both moved in as unexported. The two options are the test
  seams, and the reason a bare `OpenerImpl{}` is not a supported construction.
- **`docker.TargetsImpl`**: empty struct, no options. `Open` calls the
  unexported `open`, which is the current `Open` function returning
  `*TargetImpl`, and returns it as `attach.Target`. `TargetImpl` is the
  current `Attacher` struct renamed; its methods are untouched.

## Tests

One test file per source file, as CONTRIBUTING requires. Files follow their
sources through renames.

| File | Change |
|---|---|
| `v1/v1_test.go` (external) | Adds `TestApplyOrder`: options run in order, later wins, `Apply` returns its argument. |
| `v1alpha1/builder_test.go` (external) | Every `New().WithX(…)` becomes `New(WithX(…))` — about twenty sites. `TestWithersChain` becomes `TestOptionsLand`: each option is observable after `Build`, through the flag default or `Name`. Every other case keeps its assertion. |
| `v1alpha1/tunnel_test.go` (internal) | Loses `TestOpenInBrowser`, `swapOpener` and `TestAwaitReachable`. Gains `TestRun`, a table driving `run` end to end with fakes for all six contracts. |
| `v1alpha1/env_test.go` (internal) | Four `New()` call sites take the option form. |
| `v1alpha1/origins_test.go` (internal) | The `openTarget` swap becomes a fake `Targets` passed to `bindOrigins`. `stubTarget` already exists. |
| `v1alpha1/v1alpha1_test.go` (external) | Adds: `New` wires every collaborator non-nil; each contract option lands where `run` reads it; a caller's option beats the default. |
| `v1alpha1/example_test.go` (external) | The three godoc examples rewritten in the option form — they render on pkg.go.dev and are the first thing an embedder copies. |
| `v1alpha1/engine/engine_test.go` NEW (external) | `Tunnel("", "")` returns a tunnel; `Tunnel("", host)` lands `host` in `CloudflareProviderEnv`; `Tunnel("not json", "")` returns a tunnel whose `Err` is already set — libtunnel cancels an unparsable spec at construction, so no network is touched. |
| `v1alpha1/counter/counter_test.go` MOVE (external) | `NewCounter().WithMaxGone(n)` becomes `New(WithMaxGone(n))`. The "an unconfigured counter" case inverts: it now trips at `DefaultMaxGone`, and a case pins that number. The disable cases (−1, 0) and the bare-struct case are kept. |
| `v1alpha1/cache/cache_test.go` RENAME (external) | `env.Save` → `cache.New().Save`, and so on. Cases unchanged. |
| `v1alpha1/panel/panel_test.go` RENAME (internal) | `Wanted`/`URL` through `New()`; `Panel`/`Unframe` → `shell`/`unframe`. Adds a case pinning `Interceptors` order: shell first, and its priority ahead of unframe's. |
| `v1alpha1/browser/browser_test.go` NEW (internal) | The two moved tests, adapted to `New(WithLaunch(fn), WithWindow(d))`. Adds one case that `Open` probes before it launches. |
| `v1alpha1/attach/docker/docker_test.go` (internal) | `Open(` → `open(` at every call. Adds a case that `New().Open` returns an `attach.Target` for a running container, under the same daemon-and-alpine skip as its neighbours. |
| `v1alpha1/attach/attach_test.go` | Unchanged. |
| `e2e/` | Unchanged. The command line, its help, its environment and its output are identical, and the examples' `--help` still answers. |

**`TestRun`** is the coverage this change exists to make possible. The fake
tunnel embeds `libtunnel.TunnelV1` and overrides the eight methods `run`
touches — `URL`, `Err`, `Done`, `WithLogger`, `WithContext`,
`WithEventListener`, `WithLocalURL`, `WithInterceptor` — recording what it was
given; an unexpected call panics through the nil embed, which is the right
outcome in a test. The fake `Engine` returns it and records `spec` and
`provider`. Fake `Cache`, `Panel`, `Opener` and `Targets` record their calls;
the fake `Counter` is a real `counter.New(counter.WithMaxGone(1))`, since the
verdict logic is the thing under test. Cases, each asserting the effect the
current code has:

- a mint reports every public address to stderr and saves the cache after the
  URL is live, never before;
- a cached spec is passed to the engine as the replay;
- a replay the edge refuses (`ErrCredentialRejected`) discards the cache and
  mints once more;
- any other replay failure returns its cause and leaves the cache alone;
- a tunnel with no URL and no error returns `ErrNotReady`;
- with the panel wanted, the panel address is what is reported and opened,
  and both interceptors are registered in order;
- with `--no-open`, the opener is never called;
- one origin, panel enabled: no panel, no interceptors, plain URL;
- a cancelled context after the URL is live returns nil;
- a tunnel that fails on its own returns its `Err`;
- enough gone verdicts return `ErrTunnelGone` naming the hostname, and only
  once;
- a `BuilderImpl{}` never passed through `New` returns the wiring error and
  touches nothing.

## Surface changes

No flag, variable, output line or exit code changes. The Go surface does:

- **`v1/v1.go`** — `Builder` loses its nine `With*` methods and keeps `Build`
  and `Name`; `Option` and `Apply` are added. The package doc's example and
  the `Builder` doc move to the option form. The per-method documentation
  moves, word for word, onto the option constructors in `v1alpha1`.
- **README** — the embedding snippet under "Embed it" and the "API at a
  glance" block move to the option form; the block lists the nine builder
  options and names the six contract options as a contributor's concern,
  pointing at CONTRIBUTING. The Layout tree gains one line for the
  `v1alpha1/<name>` subpackages.
- **Examples** — `basic`, `multi-origin` and `attach` construct with
  options. `docker-compose` never builds a command and `main.go` passes no
  options; both are untouched.
- **CONTRIBUTING** — "Adding a flag" step 1 becomes a `With*` option in
  `v1alpha1/builder.go` rather than a method on the interface, and step 3
  follows. "Seeds are defaults, not settings" says option where it said
  setter. The file map gains rows for `v1alpha1.go`'s contracts, `engine/`,
  `counter/`, `cache/`, `browser/`, `attach/` and `attach/docker/`; the last
  three are missing today. "Surface/engine split" is rewritten as the
  concrete rule with the six named, and a new paragraph states the option
  convention. "Container origins" is updated for `TargetImpl` and `Targets`.
  A new "Adding a collaborator" checklist parallels "Adding a flag": contract
  in `v1alpha1.go`, `XImpl` in its subpackage, default in `New`, option,
  assertion, fake in `tunnel_test.go`, file-map row.
- **CLAUDE.md** — the read-first list gains `v1alpha1/v1alpha1.go`.
- **Package docs** — `cache`, `browser`, `docker` and `panel` rewritten
  for their new names; the `v1alpha1` package doc drops its stale sentence
  about a façade in `lib` that no longer exists.
- **Every documentation path** to `v1alpha1/multiview` — three in
  CONTRIBUTING — becomes `v1alpha1/panel`. The `--multiview` flag,
  `TUNNELD_MULTIVIEW`, `WithMultiview` and `DefaultMultiview` keep their
  names: they are the operator's and embedder's surface, and this change
  promises not to touch it. The package is named for what it serves; the flag
  for what the operator asks for.

## Verification

Per the repository's standing rule, claims are backed by running the thing:

- `make all` and `make race` green — `make e2e` inside that drives every
  example's `--help`, which is what proves the option-form examples still
  build and answer.
- The `docker` lane run locally if a daemon and the `alpine` image are
  present; reported as skipped otherwise, not as passed.
- One live run, `go run . --url :3000 --url :4000` against two local servers,
  with the report, the browser open, and the cache write compared against a
  run from `main`.
- Three mutations, each confirmed to fail its test and then restored: the
  wiring check removed from `run`; the order of `Interceptors` swapped; the
  two `Apply` calls in `New` swapped so defaults beat the caller.

## Out of scope

- The pending `Ready[T]` refactor, which would fold the edge probe into the
  tunnel's own readiness. The `Opener` contract is compatible with it either
  way — an implementation that no longer needs to probe drops the probe and
  the contract does not change.
- Keeping `docker` and `moby` out of an embedder's import graph. The root
  seeds `docker.New()` as the default `Targets`, which is what it imports
  today. A later change could move that default to `main.go` and the examples;
  it is a separate decision.
- Any interface on `attach.Server`, `bindOrigins` or `report`. One
  implementation each and no fake wanted.
- Any option on `attach.Serve`. It is called from one place with three
  arguments that are all required; there is nothing optional to configure.
