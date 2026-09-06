# One contract per collaborator — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every collaborator `run` composes has an internal contract in `v1alpha1`, one `XImpl` in its own subpackage, and one way to configure any of them — `v1.Option[T]` — so `run` is unit-testable with fakes and the tree reads the same way everywhere.

**Architecture:** Six interfaces (`Engine`, `Cache`, `Panel`, `Opener`, `Counter`, `Targets`) live beside `BuilderImpl` in `v1alpha1/v1alpha1.go`. Each has one implementation in `v1alpha1/<name>`, seeded by `New` and replaceable through a `With*` option. `v1.Builder` shrinks to `Build` + `Name`; every knob in the tree — builder flag seeds, contract injectors, per-implementation tunables — is a `v1.Option[T]` applied by `v1.Apply`, defaults first, caller after. Behaviour through `New` and the command line does not change.

**Tech Stack:** Go 1.26 · `github.com/cnuss/libtunnel` v0.0.66 · cobra/pflag/viper · `k8s.io/cri-streaming` · `github.com/moby/moby` · `github.com/pkg/browser`

**Spec:** [`docs/superpowers/specs/2026-09-06-contracts-design.md`](../specs/2026-09-06-contracts-design.md)

## Global Constraints

Copied from the spec and from CONTRIBUTING. Every task's requirements implicitly include these.

- **Branch `refactor/contracts`, issue [#38](https://github.com/tunnel-pizza/tunneld/issues/38).** Already checked out with the spec committed. Never push to `main`. Do not push until Task 11 says so. The PR body carries `Closes #38`.
- **One test file per source file.** `something.go` → `something_test.go`, and the test file follows its source through a `git mv`. Never a file named after a scenario. The only exceptions are `v1alpha1/example_test.go` and `e2e/`.
- **`main.go` stays thin.** Nothing in this plan touches it — `v1alpha1.New().Build()` compiles unchanged because `New` is variadic.
- **Go module floor is 1.26** (`go.mod`); do not raise it. No new dependencies.
- **No flag, environment variable, output line or exit code changes.** `--multiview`, `TUNNELD_MULTIVIEW`, `WithMultiview` and `DefaultMultiview` keep their names even though the package becomes `panel`.
- **`TUNNEL.env` keeps its filename** through the `env` → `cache` package rename; `.gitignore` and `make clean` are untouched.
- **Naming:** every implementation is `XImpl`; every package has `type Option = v1.Option[*XImpl]` and `func New(opts ...Option) *XImpl`, whether or not it has any `With*` yet.
- **`Count` returns nothing.** Go requires identical return types for interface satisfaction, and a subpackage cannot name the root's `Counter`.
- **Never forward a typed nil as an interface.** `docker.TargetsImpl.Open` checks `err` and returns a literal `nil` — see Task 8.
- **Comments explain why, not what**, in the voice of the surrounding files. Move existing doc comments with their code word for word; write new ones to argue for the decision.
- **`git mv` for every move or rename** so history follows the file.
- **Every task ends green:** `gofmt -l . ; go vet ./... ; go build ./... ; go test ./...` all clean. Commit after every task with the trailer below.

Commit trailer for every commit in this plan:

```
Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01AUFeC4pwfsCUZBUd52MNdQ
```

**Verified facts** the implementation depends on — established by reading the dependency sources, do not re-derive:

- `libtunnel.TunnelV1` (= `v1.Tunnel`) has 22 methods. `run` touches eight: `URL`, `Err`, `Done`, `WithLogger`, `WithContext`, `WithEventListener`, `WithLocalURL`, `WithInterceptor`.
- `TunnelImpl.Err()` is `context.Cause(t.ctx)` — it never blocks. `Done()` is `t.ctx.Done()`.
- `libtunnel.From(spec)` with a spec that is neither a file nor JSON returns `Failed(err)`: a tunnel whose context is already canceled with that cause. No network is touched.
- `libtunnel.New(backend)` is lazy — nothing dials until `URL`/`Listener`/`TunnelReady`. It does start one goroutine parked on the tunnel's context.
- `TunnelImpl.WithContext(ctx)` with `ctx.Err() != nil` cancels the tunnel synchronously, which retires that goroutine.
- `v1alpha1/attach/docker/docker_test.go` has `withDaemon(t) *client.Client` (skips without a daemon and a local `alpine`) and `startContainer(t, cli, tty, stdin bool) string` returning the container id.
- `v1alpha1/origins_test.go` is `package v1alpha1` and defines `stubTarget`, which `tunnel_test.go` in the same package can reuse.
- `pflag`'s `stringArray` replaces the default on the first `--url` and appends after; `cacheDirValue` mirrors that. Neither changes here.

## File Structure

| File | After this plan |
|---|---|
| `v1/v1.go` | `Option[T]`, `Apply`, `Builder{Build, Name}`, sentinels, constants |
| `v1alpha1/v1alpha1.go` | `Option` alias, `New`, `BuilderImpl`, six contracts, six contract options, assertion block |
| `v1alpha1/builder.go` | nine builder options, `Name`, `Build`, `command`, `versionCommand`, `cacheDirValue`, `defaultCacheDir`, `boolish` |
| `v1alpha1/tunnel.go` | `run`, `wired`, `events`, `report`, `PublicURL`, `parseOrigins`, `wsMarked`, `logger` |
| `v1alpha1/origins.go` | `bound`, `bindOrigins(ctx, targets, display, log)` |
| `v1alpha1/env.go`, `version.go` | unchanged |
| `v1alpha1/engine/engine.go` | `EngineImpl`, `Option`, `New`, `Tunnel` |
| `v1alpha1/counter/counter.go` | `CounterImpl`, `Option`, `New`, `WithMaxGone`, `DefaultMaxGone`, `Count`, `IsGone` |
| `v1alpha1/cache/cache.go` | `CacheImpl`, `Option`, `New`, `File`, `Cached`, `Save`, `Discard` |
| `v1alpha1/panel/panel.go` + `index.html` | `PanelImpl`, `Option`, `New`, `Wanted`, `URL`, `Interceptors`; `shell`, `unframe` unexported |
| `v1alpha1/browser/browser.go` | `OpenerImpl`, `Option`, `New`, `WithLaunch`, `WithWindow`, `Open` |
| `v1alpha1/attach/attach.go` | unchanged |
| `v1alpha1/attach/docker/docker.go` | `TargetsImpl`, `Option`, `New`, `Open`; `TargetImpl`; `open` unexported |

Task order: the option type first (additive), then the builder conversion while nothing else has moved, then one subpackage per task, then the wiring check and `TestRun` once every fake exists, then docs, then verification and the PR.

---

### Task 1: `v1.Option` and `v1.Apply`

**Files:**
- Modify: `v1/v1.go` (after the `Logger` alias, before the sentinel block)
- Test: `v1/v1_test.go`

**Interfaces:**
- Produces: `type Option[T any] func(T)`; `func Apply[T any](t T, opts ...Option[T]) T`. Every later task uses both.

- [ ] **Step 1: Write the failing test**

Append to `v1/v1_test.go`, and add `"slices"` to its imports:

```go
// TestApplyOrder pins the one rule every constructor in the tree relies on:
// options run in the order given, so a later one wins, and Apply hands back
// what it was given so a New can be a return statement.
func TestApplyOrder(t *testing.T) {
	type knobs struct {
		name string
		seen []string
	}
	set := func(v string) v1.Option[*knobs] {
		return func(k *knobs) { k.name = v; k.seen = append(k.seen, v) }
	}

	k := &knobs{}
	if got := v1.Apply(k, set("default"), set("caller")); got != k {
		t.Fatal("Apply returned a different value than it was given")
	}
	if k.name != "caller" {
		t.Errorf("name = %q, want the later option to win", k.name)
	}
	if want := []string{"default", "caller"}; !slices.Equal(k.seen, want) {
		t.Errorf("applied in order %v, want %v", k.seen, want)
	}
	if got := v1.Apply(&knobs{}); got.name != "" || got.seen != nil {
		t.Error("Apply with no options changed its argument")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./v1/ -run TestApplyOrder`
Expected: FAIL — `undefined: v1.Option` / `undefined: v1.Apply`.

- [ ] **Step 3: Add the type and the helper**

In `v1/v1.go`, directly after the `type Logger = *slog.Logger` declaration:

```go
// Option configures a value of type T while it is being constructed. Every
// New in v1alpha1 and its subpackages takes a list of them, applies its own
// defaults first and the caller's after, so the caller's always wins. A
// package aliases the instantiated type to its own Option and exposes With*
// constructors returning that alias:
//
//	cmd := v1alpha1.New(v1alpha1.WithURL("http://localhost:3000")).Build()
//
// One generic type rather than an Option per package, so the rule for how
// options are applied is declared once, in Apply, and every constructor in
// the tree reads the same way. It lives here rather than in v1alpha1 for the
// same reason the sentinels do: the subpackages cannot import the root, and
// every one of them needs it.
type Option[T any] func(T)

// Apply runs opts against t in order and returns t, so a constructor is one
// expression per tier — its defaults, then the caller's. Later options
// overwrite earlier ones, which is the whole precedence rule.
func Apply[T any](t T, opts ...Option[T]) T {
	for _, opt := range opts {
		opt(t)
	}
	return t
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./v1/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add v1/v1.go v1/v1_test.go
git commit -m "feat(v1): Option and Apply, the one way every New is configured

One generic Option[T] declared beside Builder rather than a func type
per package, so the rule for how options apply — in order, later
wins — is written once in Apply and every constructor in the tree
reads the same way. v1 rather than v1alpha1 because the subpackages
cannot import the root and all of them need it.

Refs #38."
```

---

### Task 2: The builder takes options; `v1.Builder` shrinks to what a caller calls

**Files:**
- Modify: `v1/v1.go` (package doc example, `Builder` doc and interface, `DefaultOpen` doc is fine)
- Modify: `v1alpha1/v1alpha1.go` (package doc, `Option` alias, `New`)
- Modify: `v1alpha1/builder.go` (nine methods → nine options; `cacheDirValue`; `command`)
- Modify: `v1alpha1/builder_test.go`, `v1alpha1/v1alpha1_test.go`, `v1alpha1/env_test.go` (one site), `v1alpha1/example_test.go`
- Modify: `examples/basic/main.go`, `examples/multi-origin/main.go`, `examples/attach/main.go`
- Modify: `README.md` (Embedding snippet, API at a glance)

**Interfaces:**
- Consumes: `v1.Option`, `v1.Apply` from Task 1.
- Produces: `type Option = v1.Option[*BuilderImpl]`; `func New(opts ...Option) *BuilderImpl`; `WithName`, `WithURL`, `WithProvider`, `WithCacheDir`, `WithLogLevel`, `WithOpen`, `WithMultiview`, `WithStdout`, `WithStderr`, each `func(...) Option`. `v1.Builder` is `Build() *cobra.Command; Name() string`.

- [ ] **Step 1: Rewrite the tests to the option form**

`v1alpha1/builder_test.go`. Add `"io"` to the imports. Replace `TestWithersChain` (the whole function and its doc) with:

```go
// TestOptionsLand pins that every builder option reaches the field the flag
// binds over, observed the only way an outsider can: through the built
// command. Name through Name, the writers through cobra, the rest through
// the flag defaults.
func TestOptionsLand(t *testing.T) {
	var sink bytes.Buffer
	b := v1alpha1.New(
		v1alpha1.WithName("expose"),
		v1alpha1.WithURL("http://localhost:3000"),
		v1alpha1.WithURL("http://localhost:4000"),
		v1alpha1.WithProvider("example.test"),
		v1alpha1.WithLogLevel("warn"),
		v1alpha1.WithStdout(&sink),
		v1alpha1.WithStderr(&sink),
	)

	if got, want := b.Name(), "expose"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	cmd := b.Build()
	if got, want := cmd.Name(), "expose"; got != want {
		t.Errorf("built command Name() = %q, want %q", got, want)
	}
	for flag, want := range map[string]string{
		"url":       "[http://localhost:3000,http://localhost:4000]",
		"provider":  "example.test",
		"log-level": "warn",
	} {
		if got := cmd.Flags().Lookup(flag).DefValue; got != want {
			t.Errorf("--%s default = %q, want %q", flag, got, want)
		}
	}
	if cmd.OutOrStdout() != io.Writer(&sink) || cmd.ErrOrStderr() != io.Writer(&sink) {
		t.Error("WithStdout/WithStderr did not reach the built command")
	}
}
```

Then these call sites, each an exact replacement:

| Test | Before | After |
|---|---|---|
| `TestBuildIsIdempotent` | `v1alpha1.New().WithURL("http://localhost:3000")` | `v1alpha1.New(v1alpha1.WithURL("http://localhost:3000"))` |
| `TestURLOptionalWhenSeeded` | `v1alpha1.New().WithURL("http://localhost:3000").WithLogLevel("loud")` | `v1alpha1.New(v1alpha1.WithURL("http://localhost:3000"), v1alpha1.WithLogLevel("loud"))` |
| `TestFlagReplacesSeededURLs` | `v1alpha1.New().WithURL("ftp://seeded.invalid")` | `v1alpha1.New(v1alpha1.WithURL("ftp://seeded.invalid"))` |
| `TestHelpNamesTheCommand` | `v1alpha1.New().WithName("expose")` | `v1alpha1.New(v1alpha1.WithName("expose"))` |
| `TestOpenDefaultsOn` | `v1alpha1.New().WithURL("http://localhost:3000")` | `v1alpha1.New(v1alpha1.WithURL("http://localhost:3000"))` |
| `TestWithOpenSeedsTheDefault` | `v1alpha1.New().WithURL("http://localhost:3000").WithOpen(false)` | `v1alpha1.New(v1alpha1.WithURL("http://localhost:3000"), v1alpha1.WithOpen(false))` |
| `TestCacheDir` (the `probe`) | `v1alpha1.New().WithURL("http://localhost:3000")` | `v1alpha1.New(v1alpha1.WithURL("http://localhost:3000"))` |
| `TestMultiviewDefaultsOn` | `v1alpha1.New().WithURL("http://localhost:3000", "http://localhost:4000")` | `v1alpha1.New(v1alpha1.WithURL("http://localhost:3000", "http://localhost:4000"))` |
| `TestWithMultiviewSeedsTheDefault` | `v1alpha1.New().WithURL("http://localhost:3000").WithMultiview(false)` | `v1alpha1.New(v1alpha1.WithURL("http://localhost:3000"), v1alpha1.WithMultiview(false))` |
| `TestDefaultCacheDir` (inside `dflt`) | `v1alpha1.New().WithURL("http://localhost:3000")` | `v1alpha1.New(v1alpha1.WithURL("http://localhost:3000"))` |

And in `TestCacheDir`, the seeded builder:

```go
			opts := []v1alpha1.Option{v1alpha1.WithURL("http://localhost:3000")}
			if len(tc.seed) > 0 {
				opts = append(opts, v1alpha1.WithCacheDir(resolveAll(tc.seed)...))
			}
			b := v1alpha1.New(opts...)
```

replacing

```go
			b := v1alpha1.New().WithURL("http://localhost:3000")
			if len(tc.seed) > 0 {
				b.WithCacheDir(resolveAll(tc.seed)...)
			}
```

`v1alpha1/v1alpha1_test.go` — replace `TestNewIsUnconfigured` and add one test:

```go
// TestNewIsUnconfigured pins that New carries no state of its own: two
// builders are independent, so configuring one never leaks into another.
func TestNewIsUnconfigured(t *testing.T) {
	first, second := v1alpha1.New(v1alpha1.WithName("expose")), v1alpha1.New()

	if got, want := first.Name(), "expose"; got != want {
		t.Errorf("first builder Name() = %q, want %q", got, want)
	}
	if got, want := second.Name(), v1.CommandName; got != want {
		t.Errorf("second builder Name() = %q, want the untouched default %q", got, want)
	}
}

// TestCallerOptionBeatsDefault pins the two tiers in New: the defaults are
// applied first and the caller's options after, so WithOpen(false) is not
// overwritten by the seeded DefaultOpen. Swapping the two Apply calls in New
// fails this.
func TestCallerOptionBeatsDefault(t *testing.T) {
	b := v1alpha1.New(v1alpha1.WithURL("http://localhost:3000"), v1alpha1.WithOpen(false))
	if got := b.Build().Flags().Lookup("no-open").DefValue; got != "true" {
		t.Errorf("--no-open default = %q, want %q (WithOpen(false) lost to the default)", got, "true")
	}
}
```

`v1alpha1/env_test.go`, in `TestApplyEnvSeededDefault`: `New().WithURL("http://seeded:1").Build()` → `New(WithURL("http://seeded:1")).Build()`. The other three `New()` calls in that file are unchanged.

`v1alpha1/example_test.go` — replace the three examples:

```go
// New returns a Builder configured by its options; finalize with Build, which
// yields a *cobra.Command ready to Execute.
func ExampleNew() {
	cmd := v1alpha1.New(v1alpha1.WithURL("http://localhost:3000")).Build()

	fmt.Println(cmd.Name())
	// Output: tunneld
}

// Several --url values share one public hostname: the first is the default
// origin and each later one answers on a bare ?n parameter.
func ExampleNew_multipleOrigins() {
	cmd := v1alpha1.New(
		v1alpha1.WithURL("http://localhost:3000", "http://localhost:4000"),
	).Build()

	fmt.Println(cmd.Flags().Lookup("url").DefValue)
	// Output: [http://localhost:3000,http://localhost:4000]
}

// WithName mounts tunneld under another program's verb, so an embedding CLI
// documents it as its own subcommand.
func ExampleNew_embedded() {
	cmd := v1alpha1.New(
		v1alpha1.WithName("expose"),
		v1alpha1.WithURL("http://localhost:3000"),
	).Build()

	fmt.Println(cmd.Name())
	// Output: expose
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./v1alpha1/ 2>&1 | head`
Expected: build failure — `undefined: v1alpha1.WithURL` and friends, `too many arguments in call to v1alpha1.New`.

- [ ] **Step 3: Shrink `v1.Builder` and update its docs**

In `v1/v1.go`:

The package doc's example line `cmd := v1alpha1.New().WithURL("http://localhost:3000").Build()` becomes `cmd := v1alpha1.New(v1alpha1.WithURL("http://localhost:3000")).Build()`.

The paragraph beginning `// The builder assembles the `tunneld` command: a fluent chain of With* setters` becomes:

```go
// The builder assembles the `tunneld` command: a New that takes options,
// finalized by Build, which returns a *cobra.Command ready to Execute. That
// shape is what lets tunneld be both a binary and an embeddable subcommand —
// a host program builds the command, renames it, seeds its origins, redirects
// its streams, and hangs it off its own root without reimplementing anything.
```

Replace the `Builder` doc comment and interface (from `// Builder assembles the tunneld command.` through the closing `}`) with:

```go
// Builder assembles the tunneld command. Obtain one from v1alpha1.New,
// configured by that package's options, and call the terminal Build to
// produce a *cobra.Command.
//
//	cmd := v1alpha1.New(v1alpha1.WithURL("http://localhost:3000")).Build()
//	err := cmd.ExecuteContext(ctx)
//
// Every option's value is a default, not a fixed setting: the command's flags
// bind over the same fields, so an argv value wins. Seeding an origin with
// WithURL therefore makes --url optional rather than forbidden, which is what
// an embedding program wants — a working default the user can still override.
//
// The command's context is its shutdown handle. Run it with ExecuteContext and
// cancel that context (a signal, in the binary's case) to tear the tunnel
// down, during startup as well as after it is live.
//
// The interface is only what a caller calls once the builder exists. The
// With* configuration is construction-time and lives on the constructor,
// which is why it is not here: a setter returning this interface would make
// any knob the interface does not name unreachable after it.
type Builder interface {
	// Build assembles the configured command and returns it. It is the
	// terminal step; calling it more than once returns the same command.
	Build() *cobra.Command
	// Name returns the configured command name (CommandName if WithName was
	// never given).
	Name() string
}
```

`"io"` is now unused in `v1/v1.go`; remove it from the imports.

- [ ] **Step 4: Convert the nine setters to options**

In `v1alpha1/builder.go`, replace each `func (b *BuilderImpl) WithX(...) v1.Builder { ...; return b }` with an option constructor. The doc comments are the ones currently on the `v1.Builder` methods, moved here word for word (the builder.go one-liners were shorter; the interface's are the real docs):

```go
// WithName sets the built command's name — the verb in usage strings and
// what cobra matches when the command is mounted under another root. Unset,
// the name is v1.CommandName.
func WithName(name string) Option {
	return func(b *BuilderImpl) { b.name = name }
}

// WithURL seeds the local origins to expose, in order: the first is the
// default origin and each later one answers on a bare ?n parameter. Repeated
// options append. A --url flag on the command line replaces the whole seeded
// set rather than adding to it.
//
// A missing scheme implies http and a missing host implies localhost, so
// ":8000", "localhost:8000" and "http://localhost:8000" name one origin.
func WithURL(urls ...string) Option {
	return func(b *BuilderImpl) { b.urls = append(b.urls, urls...) }
}

// WithProvider sets the quick-tunnel provider host to mint against. Unset,
// the provider is v1.DefaultProvider.
func WithProvider(host string) Option {
	return func(b *BuilderImpl) { b.provider = host }
}

// WithCacheDir adds directories to cache tunnel specs in, in order,
// appending across options.
//
// (keep the existing WithCacheDir doc from here on, unchanged)
func WithCacheDir(dirs ...string) Option {
	return func(b *BuilderImpl) {
		// A list that exists and is empty is one a false entry emptied, and
		// nothing refills it: off holds until a later source sets the field
		// back to nil and starts over.
		if b.cacheDirs != nil && len(b.cacheDirs) == 0 {
			return
		}
		for _, dir := range dirs {
			if on, ok := boolish(dir); ok {
				if !on {
					b.cacheDirs = []string{}
					return
				}
				dir = ""
			}
			if dir == "" {
				dir = defaultCacheDir()
			}
			if dir == "" {
				continue
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				continue
			}
			if slices.Contains(b.cacheDirs, abs) {
				continue
			}
			b.cacheDirs = append(b.cacheDirs, abs)
		}
	}
}

// WithLogLevel sets the tunnel's log level (debug|info|warn|error) on
// stderr. Unset, the level comes from v1.LogEnv, and silence if that is
// unset too.
func WithLogLevel(level string) Option {
	return func(b *BuilderImpl) { b.logLevel = level }
}

// WithOpen sets whether a public URL is opened in a browser once the tunnel
// is live — the multiview panel when there is one, otherwise the default
// origin. Exactly one page is opened either way, since a fan of tabs is
// rarely what anyone wanted. Unset, the behaviour is v1.DefaultOpen.
//
// It seeds the default of --no-open, which reads inverted: WithOpen(false)
// makes --no-open default to true.
func WithOpen(open bool) Option {
	return func(b *BuilderImpl) { b.open = open }
}

// WithMultiview sets whether the tunnel's own address answers with a panel
// framing every origin. It does nothing with a single origin, which has
// nothing to sit beside and keeps the bare address for itself. Unset, the
// behaviour is v1.DefaultMultiview.
func WithMultiview(multiview bool) Option {
	return func(b *BuilderImpl) { b.multiview = multiview }
}

// WithStdout redirects the help text and the version banner. Build passes it
// to the command's SetOut, so calling SetOut on the built command overrides
// this. Unset, output goes to the process's stdout.
//
// A running tunnel writes nothing there: its addresses go to stderr with the
// rest of what a person reads.
func WithStdout(w io.Writer) Option {
	return func(b *BuilderImpl) { b.stdout = w }
}

// WithStderr redirects the banner, the origin map, and the tunnel's logs.
// Build passes it to the command's SetErr, so calling SetErr on the built
// command overrides this. Unset, output goes to the process's stderr.
func WithStderr(w io.Writer) Option {
	return func(b *BuilderImpl) { b.stderr = w }
}
```

`Name`, `Build`, `command` and `versionCommand` keep their bodies. Three call sites in the same file change because an option is a function that can be applied anywhere the setter used to be called:

- in `command`: `b.WithCacheDir("")` → `WithCacheDir("")(b)`
- in `cacheDirValue.Set`: `v.b.WithCacheDir(s)` → `WithCacheDir(s)(v.b)`
- in `cacheDirValue.Replace`: `v.b.WithCacheDir(dirs...)` → `WithCacheDir(dirs...)(v.b)`

The `v1` import in `builder.go` stays (constants). Remove any `v1.Builder` return that no longer exists.

- [ ] **Step 5: `New` takes options; fix the package doc**

In `v1alpha1/v1alpha1.go`, replace the package doc and `New`:

```go
// Package v1alpha1 is the current implementation behind the v1.Builder
// interface: the command builder, the tunnel it runs, and the version
// resolution behind the build banner. Application code constructs from here
// (New, configured by this package's With* options) and matches errors
// against v1. Anything here may change between alpha revisions — depend on
// the v1 contract, not these internals.
package v1alpha1
```

```go
// Option configures a BuilderImpl at construction. The nine builder options
// in builder.go seed what the command's flags default to; the contract
// options in this file replace a collaborator run composes.
type Option = v1.Option[*BuilderImpl]

// New returns a BuilderImpl carrying its defaults, then configured by opts.
// It is the entry point for application code, and satisfies v1.Builder.
//
// Two tiers, two calls: the defaults go first, in the same vocabulary a
// caller uses to override them, and the caller's options after so a later
// one wins. Only the booleans need seeding here — their defaults are on, and
// a bool field cannot express "unset" separately from "off". Setting them
// here rather than at the flag binding keeps one rule for every knob: a
// flag's default is always the field it binds over, so WithOpen(false) is
// honoured exactly like every other seed.
func New(opts ...Option) *BuilderImpl {
	b := v1.Apply(&BuilderImpl{counter: NewCounter().WithMaxGone(3)},
		WithOpen(v1.DefaultOpen),
		WithMultiview(v1.DefaultMultiview),
	)
	return v1.Apply(b, opts...)
}
```

(The counter stays a struct-literal seed for now; Task 3 turns it into an option.)

- [ ] **Step 6: Convert the examples**

`examples/basic/main.go`:

```go
	cmd := v1alpha1.New(
		v1alpha1.WithURL("http://localhost:3000"),
	).Build()
```

`examples/multi-origin/main.go`:

```go
	cmd := v1alpha1.New(
		v1alpha1.WithURL("http://localhost:3000", "http://localhost:4000"),
		// Only the default origin would open, and this example is about
		// seeing both. Off, so it reports the whole map and opens nothing.
		v1alpha1.WithOpen(false),
	).Build()
```

`examples/attach/main.go`:

```go
	cmd := v1alpha1.New(
		v1alpha1.WithURL("dockerd://" + name),
	).Build()
```

- [ ] **Step 7: Update the README's embedding snippet and API block**

In `README.md`, the Embedding snippet's `main`:

```go
func main() {
	cmd := v1alpha1.New(
		v1alpha1.WithName("expose"),               // mount under your own verb
		v1alpha1.WithURL("http://localhost:3000"), // a default the user can override
	).Build()

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
```

The sentence after it: `Every `With*` value is a *default*` → `Every option's value is a *default*`.

Replace the "API at a glance" section's first two code blocks (the `v1alpha1` one and the `v1` one, up to and including the `Builder` interface) with:

```go
func New(opts ...Option) *BuilderImpl // defaults, then opts; satisfies v1.Builder
func Version() string                 // the release this build is
func VersionLine() string             // the human-facing build banner

// The builder's options. Each seeds a flag's default, so argv still wins.
func WithName(name string) Option        // command name; default "tunneld"
func WithURL(urls ...string) Option      // origins, in order; appends across options
func WithProvider(host string) Option    // quick-tunnel host; default tunnel.pizza
func WithCacheDir(dirs ...string) Option // spec cache directories; true/false are instructions
func WithLogLevel(level string) Option   // debug|info|warn|error on stderr
func WithOpen(open bool) Option          // open a browser when live; default true
func WithMultiview(mv bool) Option       // frame the origins together; default true
func WithStdout(w io.Writer) Option      // help text and the version banner
func WithStderr(w io.Writer) Option      // banner, origin map, logs
```

and

```go
// Option configures a value while it is constructed; Apply runs a list of
// them in order, so a later one wins.
type Option[T any] func(T)
func Apply[T any](t T, opts ...Option[T]) T

// Builder assembles the tunneld command: what a caller calls once New has
// configured it.
type Builder interface {
    Build() *cobra.Command // terminal: assembles and returns
    Name() string          // configured command name
}
```

The `Err*` and `const` lines below stay.

- [ ] **Step 8: Build, vet, test, e2e**

Run: `gofmt -l . && go vet ./... && go build ./... && go test ./... && make e2e`
Expected: no gofmt output; everything PASS. `make e2e` proves the three converted examples still build and answer `--help`.

- [ ] **Step 9: Commit**

```bash
git add v1 v1alpha1 examples README.md
git commit -m "refactor: the builder takes options; v1.Builder is Build and Name

The fluent With* setters returned the v1.Builder interface, which made
any knob the interface did not name unreachable once one had been
called — the reason libtunnel has to document 'chain these first'.
Every knob is now a v1alpha1.With* option passed to New, which applies
its defaults and then the caller's; the interface keeps only what a
caller calls after that. Examples, godoc examples and the README follow.

Refs #38."
```

---

### Task 3: `counter` subpackage and the `Counter` contract

**Files:**
- Move: `v1alpha1/counters.go` → `v1alpha1/counter/counter.go`; `v1alpha1/counters_test.go` → `v1alpha1/counter/counter_test.go`
- Modify: `v1alpha1/v1alpha1.go` (contract, field, option, default, assertion)
- Modify: `v1alpha1/tunnel.go` (`events`)

**Interfaces:**
- Consumes: `v1.Option`, `v1.Apply`; `Option`, `New` from Task 2.
- Produces: root `type Counter interface { Count(e libtunnel.Event); IsGone() bool }`; `func WithCounter(c Counter) Option`; `counter.New(opts ...counter.Option) *counter.CounterImpl`; `counter.WithMaxGone(max int64) counter.Option`; `counter.DefaultMaxGone = 3`.

- [ ] **Step 1: Move the files**

```bash
mkdir -p v1alpha1/counter
git mv v1alpha1/counters.go v1alpha1/counter/counter.go
git mv v1alpha1/counters_test.go v1alpha1/counter/counter_test.go
```

- [ ] **Step 2: Rewrite the test for the new package**

`v1alpha1/counter/counter_test.go`. Header comment, package and imports:

```go
// The tests for counter.go. `package counter_test` is the outside-the-package
// view: New, WithMaxGone, Count and IsGone are the whole surface, and what
// the counter concludes from a stream of events is the contract worth pinning.
package counter_test

import (
	"math"
	"sync"
	"testing"

	"github.com/cnuss/libtunnel"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
)
```

`count` takes the new type: `func count(c *counter.CounterImpl, stream []libtunnel.EventKind) bool` — body unchanged.

In `TestCountRuns`: `v1alpha1.NewCounter().WithMaxGone(tt.max)` → `counter.New(counter.WithMaxGone(tt.max))`.

`TestMaxGoneDisables`: delete the `"an unconfigured counter"` row — it now trips. The remaining rows:

```go
	for _, tt := range []struct {
		name    string
		counter func() *counter.CounterImpl
	}{
		{
			name:    "a negative maximum",
			counter: func() *counter.CounterImpl { return counter.New(counter.WithMaxGone(-1)) },
		},
		{
			// Zero has no other coherent reading: a run is never shorter than
			// none, so a zero threshold would answer true before anything
			// happened.
			name:    "a zero maximum",
			counter: func() *counter.CounterImpl { return counter.New(counter.WithMaxGone(0)) },
		},
		{
			// Not reachable through the constructor, but CounterImpl is
			// exported and a bare struct starts at zero. Without the guard in
			// IsGone this one answers true before a single event arrives.
			name:    "a bare struct that never saw New",
			counter: func() *counter.CounterImpl { return new(counter.CounterImpl) },
		},
	} {
```

Its doc: "the three ways a counter reports nothing" — leave as is, there are still three.

Add after it:

```go
// TestNewIsArmed pins that an unconfigured counter trips, at exactly
// DefaultMaxGone. The builder wires one in without naming a number, so this
// is what keeps the default from silently becoming "never".
func TestNewIsArmed(t *testing.T) {
	c := counter.New()
	for i := range counter.DefaultMaxGone {
		if c.IsGone() {
			t.Fatalf("IsGone() = true after %d verdicts, want %d", i, counter.DefaultMaxGone)
		}
		c.Count(libtunnel.Event{Kind: gone})
	}
	if !c.IsGone() {
		t.Errorf("IsGone() = false after %d verdicts, want true", counter.DefaultMaxGone)
	}
}
```

`TestMaxGoneCeiling`: `counter.New(counter.WithMaxGone(math.MaxInt64))`. `TestCountIsConcurrent`: `counter.New(counter.WithMaxGone(1))`.

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./v1alpha1/counter/ 2>&1 | head`
Expected: build failure — `package counter` vs the file's `package v1alpha1`, `undefined: counter.New`.

- [ ] **Step 4: Rewrite the source**

`v1alpha1/counter/counter.go` in full. The `Count`, `WithMaxGone` and `IsGone` doc comments are the existing ones, moved; only the constructor references change:

```go
// Package counter decides when the edge has disowned a tunnel: it folds the
// tunnel's lifecycle events into a run of consecutive gone verdicts and
// reports when that run reaches a threshold. The tunnel engine keeps
// retrying a reaped tunnel indefinitely — that is cloudflared's behaviour
// and libtunnel leaves it alone — so this verdict is what turns a hostname
// that resolves nowhere into an exit code a supervisor can act on.
package counter

import (
	"math"
	"sync/atomic"

	"github.com/cnuss/libtunnel"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// DefaultMaxGone is how many consecutive gone verdicts New arms a counter
// with. Three: the one measured reap delivered four verdicts among fourteen
// disconnects, so three is reached with one to spare, and it is more than a
// single verdict from a probe that may have lost a DNS race.
const DefaultMaxGone = 3

// Option configures a CounterImpl at construction.
type Option = v1.Option[*CounterImpl]

// CounterImpl is the default counter. It is safe for concurrent use: the
// tunnel engine runs listeners from whichever goroutine produced the event.
type CounterImpl struct {
	gone    atomic.Uint64
	goneMax uint64
}

// New returns a counter armed at DefaultMaxGone, then configured by opts —
// so WithMaxGone in opts replaces the default rather than adding to it.
func New(opts ...Option) *CounterImpl {
	c := v1.Apply(&CounterImpl{}, WithMaxGone(DefaultMaxGone))
	return v1.Apply(c, opts...)
}

// WithMaxGone sets how many consecutive gone verdicts arm IsGone.
//
// A negative maximum disables the counter — the tunnel is then left to keep
// retrying however long it takes, which is what cloudflared does unattended.
// Zero disables it too, having no other coherent reading: a run of verdicts is
// never shorter than none, so a threshold of zero would answer true before
// anything had happened. Both land on the largest ceiling a uint64 holds,
// which nothing reaches.
func WithMaxGone(max int64) Option {
	return func(c *CounterImpl) {
		if max <= 0 {
			c.goneMax = math.MaxUint64
			return
		}
		c.goneMax = uint64(max)
	}
}

// Count folds one event into the run of consecutive gone verdicts.
//
// Only a connection that came back clears the run. A reap is a storm of
// disconnects around the probe that finds it — one measured reap delivered
// fourteen of them against four gone verdicts, never two gone in a row — so
// resetting on every kind that is not gone held the count at one and no
// threshold above it could ever be reached.
//
// Disconnected says an edge connection ended, which is equally true while a
// tunnel is being reaped and while it is merely being cycled. It is the
// question, not the answer. EventGone is the answer: libtunnel emits it only
// after probing, when the hostname has stopped resolving or the edge has
// refused a fresh registration outright.
func (c *CounterImpl) Count(e libtunnel.Event) {
	switch e.Kind {
	case libtunnel.EventGone:
		c.gone.Add(1)
	case libtunnel.EventConnected, libtunnel.EventReconnected:
		c.gone.Store(0)
	}
}

// IsGone reports whether the run of gone verdicts has reached the threshold.
//
// The zero guard is for a CounterImpl built as a bare struct rather than
// through New, which WithMaxGone cannot produce: without it such a counter
// would answer true before a single event arrived.
func (c *CounterImpl) IsGone() bool {
	return c.goneMax > 0 && c.gone.Load() >= c.goneMax
}
```

- [ ] **Step 5: Run the counter tests**

Run: `go test ./v1alpha1/counter/ -race`
Expected: PASS. (`-race` because `TestCountIsConcurrent` is only meaningful under it.)

- [ ] **Step 6: Wire the root**

`v1alpha1/v1alpha1.go`. Add `"github.com/cnuss/libtunnel"` and `"github.com/tunnel-pizza/tunneld/v1alpha1/counter"` to the imports. Directly after the `Option` alias, start the contracts section:

```go
// The contracts run composes. Each is something with an external effect —
// the edge, the disk, the daemon, the browser, an HTTP probe — implemented
// once in a v1alpha1/<name> subpackage, seeded by New, and replaceable with
// the matching With* option below. A function that maps a value to a value
// gets no contract; see CONTRIBUTING.

// Counter folds tunnel events into a verdict: has the edge disowned it.
type Counter interface {
	Count(e libtunnel.Event)
	IsGone() bool
}

// WithCounter replaces the counter that decides when the edge has disowned
// the tunnel. The default is counter.New(), armed at counter.DefaultMaxGone.
func WithCounter(c Counter) Option {
	return func(b *BuilderImpl) { b.counter = c }
}

// The defaults satisfy their contracts, checked here so a drift fails the
// build rather than the first run.
var (
	_ v1.Builder = (*BuilderImpl)(nil)
	_ Counter    = (*counter.CounterImpl)(nil)
)
```

In `BuilderImpl`, replace

```go
	// TODO Doc
	counter *Counter
```

with

```go
	// The collaborators run composes, each behind a contract declared above.
	// Seeded by New; a test or a contributor swaps one with its With* option.
	counter Counter
```

In `New`, the struct literal loses its counter and the defaults gain an option:

```go
	b := v1.Apply(&BuilderImpl{},
		WithOpen(v1.DefaultOpen),
		WithMultiview(v1.DefaultMultiview),
		WithCounter(counter.New()),
	)
```

`v1alpha1/tunnel.go`, in `events`, the verdict no longer chains:

```go
		b.counter.Count(e)
		if b.counter.IsGone() {
```

The `b.counter == nil` guard above it stays until Task 9 replaces it.

- [ ] **Step 7: Build, vet, test**

Run: `gofmt -l . && go vet ./... && go build ./... && go test ./...`
Expected: clean and PASS. `ls v1alpha1/counters*.go` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add -A v1alpha1
git commit -m "refactor: counter is a subpackage behind a Counter contract

New() comes back armed at counter.DefaultMaxGone rather than at a
ceiling nothing reaches, which is what the builder's TODO was asking
for: the number belongs to the counter, and the builder wires one in
without knowing it. Count no longer returns the counter — a contract
cannot promise that, since Go needs identical return types and the
subpackage cannot name the root's interface.

Refs #38."
```

---

### Task 4: `engine` subpackage and the `Engine` contract

**Files:**
- Create: `v1alpha1/engine/engine.go`, `v1alpha1/engine/engine_test.go`
- Modify: `v1alpha1/v1alpha1.go` (contract, field, option, default, assertion)
- Modify: `v1alpha1/tunnel.go` (delete the `engine` method; `run` calls the contract)

**Interfaces:**
- Consumes: `v1.Option`, `v1.Apply`; root `Option`, `New`.
- Produces: root `type Engine interface { Tunnel(spec, provider string) libtunnel.TunnelV1 }`; `func WithEngine(e Engine) Option`; `engine.New(opts ...engine.Option) *engine.EngineImpl`.

- [ ] **Step 1: Write the failing tests**

`v1alpha1/engine/engine_test.go`:

```go
// The tests for engine.go. `package engine_test` is the outside view: Tunnel
// is the whole surface. Nothing here dials — New is lazy and From on a bad
// spec is already canceled — so the package's tests stay offline like the
// rest of the suite.
package engine_test

import (
	"context"
	"os"
	"testing"

	"github.com/cnuss/libtunnel"
	ltv1 "github.com/cnuss/libtunnel/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/engine"
)

// stop cancels a tunnel that was never started, so the goroutine libtunnel
// parks on its context retires with the test rather than outliving it.
func stop(t *testing.T, tun libtunnel.TunnelV1) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tun.WithContext(ctx)
}

// TestTunnelMints pins that an empty spec asks for a fresh mint: a live
// tunnel with no error yet, since nothing has been dialed.
func TestTunnelMints(t *testing.T) {
	tun := engine.New().Tunnel("", "")
	if tun == nil {
		t.Fatal("Tunnel() = nil")
	}
	defer stop(t, tun)
	if err := tun.Err(); err != nil {
		t.Errorf("Err() = %v before anything was dialed, want nil", err)
	}
}

// TestTunnelSetsProvider pins the one side effect: the provider host travels
// by environment, because a replayed spec builds its own backend and the
// variable is the knob both paths read.
func TestTunnelSetsProvider(t *testing.T) {
	t.Setenv(ltv1.CloudflareProviderEnv, "")
	stop(t, engine.New().Tunnel("", "example.test"))
	if got := os.Getenv(ltv1.CloudflareProviderEnv); got != "example.test" {
		t.Errorf("%s = %q, want %q", ltv1.CloudflareProviderEnv, got, "example.test")
	}
}

// TestTunnelReplaysBadSpec pins that a spec libtunnel cannot parse comes back
// as a tunnel that has already failed — libtunnel's no-error contract — so
// run reports it through Err rather than hanging on URL.
func TestTunnelReplaysBadSpec(t *testing.T) {
	tun := engine.New().Tunnel("not a spec", "")
	if err := tun.Err(); err == nil {
		t.Fatal("Err() = nil for an unparsable spec, want the failure")
	}
	select {
	case <-tun.Done():
	default:
		t.Error("Done() is open for a tunnel that already failed")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./v1alpha1/engine/ 2>&1 | head`
Expected: build failure — no such package.

- [ ] **Step 3: Write the package**

`v1alpha1/engine/engine.go`. The `Tunnel` doc is the current `engine` method's doc, moved:

```go
// Package engine mints or replays the tunnel tunneld runs, through
// github.com/cnuss/libtunnel driving Cloudflare's edge in process — no
// cloudflared binary, no account, no DNS to configure. Its own subpackage so
// the root holds the builder and the contracts and nothing that speaks to
// the edge.
package engine

import (
	"os"

	"github.com/cnuss/libtunnel"
	ltv1 "github.com/cnuss/libtunnel/v1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Option configures an EngineImpl at construction. There are none yet; the
// signature exists so a knob added later changes no caller.
type Option = v1.Option[*EngineImpl]

// EngineImpl is the default engine: libtunnel's Cloudflare backend.
type EngineImpl struct{}

// New returns the default engine, configured by opts.
func New(opts ...Option) *EngineImpl {
	return v1.Apply(&EngineImpl{}, opts...)
}

// Tunnel returns the unstarted tunnel to run: a replay of spec when there is
// one, otherwise a fresh mint against provider, or the default provider when
// that is empty.
//
// libtunnel.From rather than the LIBTUNNEL_SPEC variable, because From is the
// path that asks. A replayed spec's identity rides the mint request, so a
// tunnel reaped since it was cached still comes back on the same hostname when
// the provider can still give that name out — which is the whole point of
// keeping the spec. When it cannot, the mint has already happened and its
// hostname is adopted rather than refused, so a lapsed reservation costs the
// name and nothing else. The variable is the parent-to-child channel, where
// the tunnel is live by construction and no question needs asking.
//
// From builds its own backend, so the provider host travels by environment
// rather than through WithProvider. It is the same knob either way: libtunnel
// reads that variable over a code-set host.
func (*EngineImpl) Tunnel(spec, provider string) libtunnel.TunnelV1 {
	if provider != "" {
		// Best effort: a provider that cannot be set falls back to the
		// default, which is where an unset one would have gone anyway.
		_ = os.Setenv(ltv1.CloudflareProviderEnv, provider)
	}
	if spec != "" {
		return libtunnel.From(spec)
	}
	backend := libtunnel.Cloudflare()
	if provider != "" {
		backend = backend.WithProvider(provider)
	}
	return libtunnel.New(backend)
}
```

- [ ] **Step 4: Run the engine tests**

Run: `go test ./v1alpha1/engine/`
Expected: PASS, in well under a second — nothing dialed.

- [ ] **Step 5: Wire the root**

`v1alpha1/v1alpha1.go`: import `"github.com/tunnel-pizza/tunneld/v1alpha1/engine"`. Add to the contracts section, before `Counter`:

```go
// Engine mints or replays the tunnel run drives. spec is a cached envelope to
// replay, "" to mint; provider is the quick-tunnel host, "" for the default.
type Engine interface {
	Tunnel(spec, provider string) libtunnel.TunnelV1
}

// WithEngine replaces what mints or replays the tunnel. The default is
// engine.New(), libtunnel's Cloudflare backend; a test hands in a fake.
func WithEngine(e Engine) Option {
	return func(b *BuilderImpl) { b.engine = e }
}
```

Field, first in the collaborators group: `engine Engine`. Default in `New`, first among the contract options: `WithEngine(engine.New()),`. Assertion: `_ Engine = (*engine.EngineImpl)(nil),`.

`v1alpha1/tunnel.go`: in `run`, `tun := b.engine(spec).` → `tun := b.engine.Tunnel(spec, b.provider).`. Delete the `engine` method and its doc entirely. Remove the now-unused `ltv1 "github.com/cnuss/libtunnel/v1"` import (`os` stays — `logger` uses it).

- [ ] **Step 6: Build, vet, test**

Run: `gofmt -l . && go vet ./... && go build ./... && go test ./...`
Expected: clean and PASS.

- [ ] **Step 7: Commit**

```bash
git add v1alpha1
git commit -m "refactor: the tunnel engine is a subpackage behind an Engine contract

What used to be a method on the builder is the one seam that lets run
be driven without the network: a fake Engine hands back a fake tunnel.
The provider rides as an argument because it settles at flag time,
after New has already seeded the engine.

Refs #38."
```

---

### Task 5: `env` becomes `cache`, behind the `Cache` contract

**Files:**
- Move: `v1alpha1/env/` → `v1alpha1/cache/`; `env.go` → `cache.go`; `env_test.go` → `cache_test.go`
- Modify: `v1alpha1/v1alpha1.go` (contract, field, option, default, assertion)
- Modify: `v1alpha1/tunnel.go` (`run` calls the contract; import)

**Interfaces:**
- Consumes: `v1.Option`, `v1.Apply`; root `Option`, `New`.
- Produces: root `type Cache interface { Cached(dirs []string, log v1.Logger) string; Save(dirs []string, log v1.Logger); Discard(dirs []string, log v1.Logger) }`; `func WithCache(c Cache) Option`; `cache.New(opts ...cache.Option) *cache.CacheImpl`; `cache.File`.

- [ ] **Step 1: Move the package**

```bash
git mv v1alpha1/env v1alpha1/cache
git mv v1alpha1/cache/env.go v1alpha1/cache/cache.go
git mv v1alpha1/cache/env_test.go v1alpha1/cache/cache_test.go
```

- [ ] **Step 2: Rewrite the test for the new surface**

In `v1alpha1/cache/cache_test.go`, mechanically (macOS sed; on Linux drop the `''`):

```bash
sed -i '' \
  -e 's/^package env_test$/package cache_test/' \
  -e 's#"github.com/tunnel-pizza/tunneld/v1alpha1/env"#"github.com/tunnel-pizza/tunneld/v1alpha1/cache"#' \
  -e 's/env\.Save(/cache.New().Save(/g' \
  -e 's/env\.Cached(/cache.New().Cached(/g' \
  -e 's/env\.Discard(/cache.New().Discard(/g' \
  -e 's/env\.File/cache.File/g' \
  v1alpha1/cache/cache_test.go
```

(No `\b` — BSD sed does not know it. Nothing else in that file ends in `env` before a dot, so the bare patterns are exact.)

Then the header comment by hand:

```go
// The tests for cache.go. `package cache_test` is the outside-the-package
// view: Cached, Save and Discard are the whole surface, and the file they
// exchange is the contract worth pinning rather than anything unexported.
```

Confirm nothing was missed: `grep -n 'env\.' v1alpha1/cache/cache_test.go` prints nothing.

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./v1alpha1/cache/ 2>&1 | head`
Expected: build failure — `package cache` vs `package env`, `undefined: cache.New`.

- [ ] **Step 4: Rewrite the source's surface**

In `v1alpha1/cache/cache.go`:

The package clause and doc:

```go
// Package cache carries a tunnel's identity between runs.
//
// libtunnel mints a fresh hostname on every start unless it is handed the spec
// of a tunnel it already has, and that handoff channel is an environment
// variable. So persisting one is all a cache has to be: a file of variables,
// written when a tunnel comes up and read back into the environment before the
// next one is built.
//
// The library used to keep this file itself and no longer does
// (cnuss/libtunnel#167). Where it lands is a deployment's decision rather than
// a library's — a volume in a container, a working directory on a laptop —
// which is why the directories arrive from --cache-dir rather than being
// derived here.
package cache
```

After `saved`, add the type and constructor:

```go
// Option configures a CacheImpl at construction. There are none yet; the
// signature exists so a knob added later changes no caller.
type Option = v1.Option[*CacheImpl]

// CacheImpl is the default cache: TUNNEL.env in each directory it is given.
type CacheImpl struct{}

// New returns the default cache, configured by opts.
func New(opts ...Option) *CacheImpl {
	return v1.Apply(&CacheImpl{}, opts...)
}
```

Then the three functions become methods — only the signature line of each changes:

- `func Cached(cacheDirs []string, log v1.Logger) string` → `func (*CacheImpl) Cached(cacheDirs []string, log v1.Logger) string`
- `func Discard(cacheDirs []string, log v1.Logger)` → `func (*CacheImpl) Discard(cacheDirs []string, log v1.Logger)`
- `func Save(cacheDirs []string, log v1.Logger)` → `func (*CacheImpl) Save(cacheDirs []string, log v1.Logger)`

One stale word in `Save`'s doc: `where Load reads the first and stops` → `where Cached reads the first and stops`.

- [ ] **Step 5: Run the cache tests**

Run: `go test ./v1alpha1/cache/`
Expected: PASS.

- [ ] **Step 6: Wire the root**

`v1alpha1/v1alpha1.go`: import `"github.com/tunnel-pizza/tunneld/v1alpha1/cache"`. Add to the contracts section, after `Engine`:

```go
// Cache persists a tunnel's spec between runs, in the directories
// --cache-dir settled on.
type Cache interface {
	Cached(dirs []string, log v1.Logger) string
	Save(dirs []string, log v1.Logger)
	Discard(dirs []string, log v1.Logger)
}

// WithCache replaces where a tunnel's spec is kept between runs. The default
// is cache.New(), a TUNNEL.env in each directory.
func WithCache(c Cache) Option {
	return func(b *BuilderImpl) { b.cache = c }
}
```

Field, after `engine`: `cache Cache`. Default in `New`, after `WithEngine`: `WithCache(cache.New()),`. Assertion: `_ Cache = (*cache.CacheImpl)(nil),`.

`v1alpha1/tunnel.go`: replace the `env` import with nothing (the root reaches the cache through the field), and the three calls:

- `cached = env.Cached(b.cacheDirs, log)` → `cached = b.cache.Cached(b.cacheDirs, log)`
- `env.Discard(b.cacheDirs, log)` → `b.cache.Discard(b.cacheDirs, log)`
- `env.Save(b.cacheDirs, log)` → `b.cache.Save(b.cacheDirs, log)`

- [ ] **Step 7: Build, vet, test**

Run: `gofmt -l . && go vet ./... && go build ./... && go test ./... && grep -rn 'v1alpha1/env"' --include='*.go' .`
Expected: clean, PASS, and the grep prints nothing.

- [ ] **Step 8: Commit**

```bash
git add -A v1alpha1
git commit -m "refactor: the spec cache is package cache, behind a Cache contract

Renamed from env, which shared its name with env.go — the
environment-variable helpers — two unrelated things called env. The
file it writes is still TUNNEL.env, so .gitignore and make clean are
untouched. The directories ride as an argument because --cache-dir
settles at flag time, after New has seeded the cache.

Refs #38."
```

---

### Task 6: `multiview` becomes `panel`, behind the `Panel` contract

**Files:**
- Move: `v1alpha1/multiview/` → `v1alpha1/panel/` (`index.html` travels with the directory); `multiview.go` → `panel.go`; `multiview_test.go` → `panel_test.go`
- Modify: `v1alpha1/v1alpha1.go` (contract, field, option, default, assertion)
- Modify: `v1alpha1/tunnel.go` (`run` calls the contract; import)
- Modify: `v1alpha1/tunnel_test.go` (one call site; import)

**Interfaces:**
- Consumes: `v1.Option`, `v1.Apply`; root `Option`, `New`.
- Produces: root `type Panel interface { Wanted(enabled bool, origins []*url.URL) bool; URL(public *url.URL) string; Interceptors(origins []*url.URL, log v1.Logger) []libtunnel.Interceptor }`; `func WithPanel(p Panel) Option`; `panel.New(opts ...panel.Option) *panel.PanelImpl`.

- [ ] **Step 1: Move the package**

```bash
git mv v1alpha1/multiview v1alpha1/panel
git mv v1alpha1/panel/multiview.go v1alpha1/panel/panel.go
git mv v1alpha1/panel/multiview_test.go v1alpha1/panel/panel_test.go
ls v1alpha1/panel   # index.html  panel.go  panel_test.go
```

- [ ] **Step 2: Rewrite the test call sites and add the order case**

In `v1alpha1/panel/panel_test.go`: `package multiview` → `package panel`. Then:

| Test | Before | After |
|---|---|---|
| `TestWanted` | `Wanted(tc.enabled, tc.origins)` | `New().Wanted(tc.enabled, tc.origins)` |
| `TestURL` | `URL(public)` | `New().URL(public)` |
| `TestPanelInterceptorServesTheShell` | `Panel(origins, slog.New(slog.DiscardHandler))` | `shell(origins, slog.New(slog.DiscardHandler))` |
| `TestUnframeIsBehindThePanel` | `Panel(nil, slog.New(slog.DiscardHandler)).Priority` | `shell(nil, slog.New(slog.DiscardHandler)).Priority` |
| `TestUnframeIsBehindThePanel` | `Unframe()` (twice) | `unframe()` |

Add after `TestUnframeIsBehindThePanel`:

```go
// TestInterceptorsOrder pins what run relies on: the shell comes first and
// outranks the unframer, so the one request that must never reach an origin
// is answered before anything looks at framing. Swapping the two fails this.
func TestInterceptorsOrder(t *testing.T) {
	got := New().Interceptors(nil, slog.New(slog.DiscardHandler))
	if len(got) != 2 {
		t.Fatalf("Interceptors() returned %d, want 2", len(got))
	}
	if !got[0].Match(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("Interceptors()[0] does not match the panel request, want the shell first")
	}
	if got[0].Priority >= got[1].Priority {
		t.Errorf("shell Priority = %d, unframe = %d; want the shell ahead", got[0].Priority, got[1].Priority)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./v1alpha1/panel/ 2>&1 | head`
Expected: build failure — `undefined: New`, `undefined: shell`.

- [ ] **Step 4: Rewrite the source's surface**

In `v1alpha1/panel/panel.go`:

Package clause and doc:

```go
// Package panel serves the page that frames every origin behind a tunnel,
// and the header surgery that lets those frames render.
//
// It is an implementation subpackage: the v1alpha1 root stays plumbing, and
// anything with a world of its own lives beside it. index.html travels with
// the code because go:embed cannot reach outside its own package directory.
package panel
```

Add `v1 "github.com/tunnel-pizza/tunneld/v1"` to the imports.

The template variable is renamed so the interceptor builder can take its name — `var shell = template.Must(...)` → `var shellTmpl = template.Must(...)`, with `template.New("multiview")` left as is (a template name, not an identifier), and `shell.Execute(&page, data)` in `serveShell` → `shellTmpl.Execute(&page, data)`.

After `shellOrigin`, add the type and constructor:

```go
// Option configures a PanelImpl at construction. There are none yet; the
// signature exists so a knob added later changes no caller.
type Option = v1.Option[*PanelImpl]

// PanelImpl is the default panel: one page of frames, and the unframer that
// lets the frames render.
type PanelImpl struct{}

// New returns the default panel, configured by opts.
func New(opts ...Option) *PanelImpl {
	return v1.Apply(&PanelImpl{}, opts...)
}

// Interceptors is what the tunnel registers when the panel is wanted: the
// shell first, at the highest priority there is, and the unframer behind it.
// The order is the contract — run registers them in a loop and never looks
// at a priority itself.
func (*PanelImpl) Interceptors(origins []*url.URL, log v1.Logger) []libtunnel.Interceptor {
	return []libtunnel.Interceptor{shell(origins, log), unframe()}
}
```

Then four renames, docs otherwise untouched:

- `func Panel(origins []*url.URL, log *slog.Logger) libtunnel.Interceptor` → `func shell(origins []*url.URL, log *slog.Logger) libtunnel.Interceptor`. Its doc's first sentence becomes `// shell builds the interceptor that serves the panel.` and drop the sentence about being "a constructor returning an Interceptor, the shape the tunnel library expects" — `Interceptors` is that shape now.
- `func Unframe() libtunnel.Interceptor` → `func unframe() libtunnel.Interceptor`; doc first word `Unframe` → `unframe`.
- `func URL(public *url.URL) string` → `func (*PanelImpl) URL(public *url.URL) string`.
- `func Wanted(enabled bool, origins []*url.URL) bool` → `func (*PanelImpl) Wanted(enabled bool, origins []*url.URL) bool`.

- [ ] **Step 5: Run the panel tests**

Run: `go test ./v1alpha1/panel/`
Expected: PASS.

- [ ] **Step 6: Wire the root**

`v1alpha1/v1alpha1.go`: import `"github.com/tunnel-pizza/tunneld/v1alpha1/panel"` and `"net/url"`. Add to the contracts section, after `Cache`:

```go
// Panel serves several origins as one page on the tunnel's bare address.
type Panel interface {
	Wanted(enabled bool, origins []*url.URL) bool
	URL(public *url.URL) string
	Interceptors(origins []*url.URL, log v1.Logger) []libtunnel.Interceptor
}

// WithPanel replaces what answers the tunnel's bare address when there is
// more than one origin. The default is panel.New().
func WithPanel(p Panel) Option {
	return func(b *BuilderImpl) { b.panel = p }
}
```

Field, after `cache`: `panel Panel`. Default in `New`, after `WithCache`: `WithPanel(panel.New()),`. Assertion: `_ Panel = (*panel.PanelImpl)(nil),`.

`v1alpha1/tunnel.go`: drop the `multiview` import. In `run`:

```go
		// Served in front of the origin proxy, so the panel needs no port of
		// its own and no origin ever sees the request.
		if b.panel.Wanted(b.multiview, origins) {
			for _, ic := range b.panel.Interceptors(origins, log) {
				tun.WithInterceptor(ic)
			}
		}
```

replaces the block that called `multiview.Wanted`, `multiview.Panel` and `multiview.Unframe`; and

```go
	if b.panel.Wanted(b.multiview, origins) {
		view = b.panel.URL(public)
	}
```

replaces the second `multiview.Wanted` / `multiview.URL` pair.

`v1alpha1/tunnel_test.go`: the import `"github.com/tunnel-pizza/tunneld/v1alpha1/multiview"` → `"github.com/tunnel-pizza/tunneld/v1alpha1/panel"`, and in `TestReportNamesTheMultiviewPanel`, `multiview.URL(public)` → `panel.New().URL(public)`.

- [ ] **Step 7: Build, vet, test**

Run: `gofmt -l . && go vet ./... && go build ./... && go test ./... && grep -rn 'multiview"' --include='*.go' .`
Expected: clean, PASS, grep prints nothing. (`--multiview`, `WithMultiview`, `MultiviewEnv`, `TestMultiviewDefaultsOn` all still exist and should — the grep is for the import path only.)

- [ ] **Step 8: Commit**

```bash
git add -A v1alpha1
git commit -m "refactor: multiview is package panel, behind a Panel contract

The package is named for what it serves; the flag, the variable and
WithMultiview keep their names because they are the operator's
surface. Interceptors returns the shell and the unframer together, in
order, so run registers them in a loop and the ordering invariant
lives with the code that knows why it matters.

Refs #38."
```

---

### Task 7: `browser` subpackage and the `Opener` contract

**Files:**
- Create: `v1alpha1/browser/browser.go`, `v1alpha1/browser/browser_test.go`
- Modify: `v1alpha1/v1alpha1.go` (contract, field, option, default, assertion)
- Modify: `v1alpha1/tunnel.go` (delete `openURL`, the `reachable*` constants, `awaitReachable`, `openInBrowser`; `run` calls the contract)
- Modify: `v1alpha1/tunnel_test.go` (delete `TestOpenInBrowser`, `swapOpener`, `TestAwaitReachable`)

**Interfaces:**
- Consumes: `v1.Option`, `v1.Apply`; root `Option`, `New`.
- Produces: root `type Opener interface { Open(ctx context.Context, addr string, stderr io.Writer, log v1.Logger) }`; `func WithOpener(o Opener) Option`; `browser.New(opts ...browser.Option) *browser.OpenerImpl`; `browser.WithLaunch(func(string) error) browser.Option`; `browser.WithWindow(time.Duration) browser.Option`.

- [ ] **Step 1: Write the failing tests**

`v1alpha1/browser/browser_test.go`. `TestAwaitReachable` is the current one from `tunnel_test.go`, moved verbatim; `TestOpenInBrowser` is the current one with the launcher passed in rather than swapped through a package variable; `TestOpen` is new.

```go
package browser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pkgbrowser "github.com/pkg/browser"
)

// TestOpen pins the order the two halves run in: the probe first, then the
// launch, so a browser is never pointed at an address the edge has not
// answered for. The server counts probes; the launcher must see the count
// already settled.
func TestOpen(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var launched string
	var seen int32
	o := New(
		WithLaunch(func(addr string) error {
			launched, seen = addr, probes.Load()
			return nil
		}),
		WithWindow(5*time.Second),
	)
	o.Open(t.Context(), srv.URL, io.Discard, slog.New(slog.DiscardHandler))

	if launched != srv.URL {
		t.Errorf("launched %q, want %q", launched, srv.URL)
	}
	if seen < 3 {
		t.Errorf("launched after %d probes, want the probe to have succeeded first", seen)
	}
}

// TestOpenInBrowser pins the two things the launcher promises. It never writes
// to stdout — the spawned process inherits writers from pkg/browser's package
// globals, which default to os.Stdout, and that stream carries nothing but
// what a script reads. And a failure to open is a debug line, not an error:
// the tunnel is up and serving either way, and a headless host is a normal
// place to run this, not a broken one.
func TestOpenInBrowser(t *testing.T) {
	t.Run("opens the address and leaves stdout alone", func(t *testing.T) {
		var opened string
		launch := func(addr string) error {
			opened = addr
			return nil
		}

		var stderr bytes.Buffer
		openInBrowser(launch, "https://foo.tunneled.pizza/", &stderr, slog.New(slog.DiscardHandler))

		if want := "https://foo.tunneled.pizza/"; opened != want {
			t.Errorf("opened %q, want %q", opened, want)
		}
		if pkgbrowser.Stdout != io.Writer(&stderr) {
			t.Error("browser.Stdout was left pointing elsewhere, want the stderr writer")
		}
		if pkgbrowser.Stderr != io.Writer(&stderr) {
			t.Error("browser.Stderr was left pointing elsewhere, want the stderr writer")
		}
	})

	// A headless host — a server, a container, CI — is a normal place to run
	// this, not a broken one, so the failure must not reach an operator who
	// never asked about it. Both halves matter: silent at warn, and still
	// there for somebody debugging a browser that did not appear.
	t.Run("a failure is quiet outside the debug log", func(t *testing.T) {
		launch := func(string) error { return errors.New("no browser here") }

		var quiet bytes.Buffer
		openInBrowser(launch, "https://foo.tunneled.pizza/", io.Discard,
			slog.New(slog.NewTextHandler(&quiet, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if quiet.Len() != 0 {
			t.Errorf("log = %q, want nothing at warn level", quiet.String())
		}

		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
		openInBrowser(launch, "https://foo.tunneled.pizza/", io.Discard, log)

		if !strings.Contains(logged.String(), "could not open a browser") {
			t.Errorf("log = %q, want the failure in the debug log", logged.String())
		}
	})
}

// TestAwaitReachable pins the wait that stands between a ready tunnel and a
// browser. The edge answers 530 for a moment after TunnelReady fires, and a
// tab opened into that window shows an error page for a tunnel that works.
func TestAwaitReachable(t *testing.T) {
	t.Run("returns once the edge stops failing", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) <= 3 {
				w.WriteHeader(http.StatusBadGateway) // the edge, not the origin
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		awaitReachable(t.Context(), srv.URL, 5*time.Second, slog.New(slog.DiscardHandler))

		if got := calls.Load(); got < 4 {
			t.Errorf("gave up after %d probes, want it to keep trying until the edge answered", got)
		}
	})

	t.Run("an origin's own error still counts as reachable", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			// The route is live; the app simply has nothing at this path.
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		awaitReachable(t.Context(), srv.URL, 5*time.Second, slog.New(slog.DiscardHandler))

		if got := calls.Load(); got != 1 {
			t.Errorf("probed %d times, want it to stop at the first answer from the origin", got)
		}
	})

	t.Run("gives up rather than never opening", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

		start := time.Now()
		awaitReachable(t.Context(), srv.URL, 600*time.Millisecond, log)
		elapsed := time.Since(start)

		if elapsed > 3*time.Second {
			t.Errorf("waited %v, want it bounded by the timeout it was given", elapsed)
		}
		if !strings.Contains(logged.String(), "before the edge answered") {
			t.Errorf("log = %q, want the debug log to say it opened anyway", logged.String())
		}
	})

	t.Run("a cancelled context ends the wait", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			awaitReachable(ctx, srv.URL, time.Minute, slog.New(slog.DiscardHandler))
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("awaitReachable ignored a cancelled context; Ctrl-C would hang")
		}
	})
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./v1alpha1/browser/ 2>&1 | head`
Expected: build failure — no such package.

- [ ] **Step 3: Write the package**

`v1alpha1/browser/browser.go`. The constants and the two unexported functions are the current ones from `tunnel.go`, moved; `openInBrowser` takes the launcher as a parameter instead of reading a package variable.

```go
// Package browser puts a public address in front of a person: it waits for
// the edge to actually serve the address, then launches whatever browser the
// host has. Its own subpackage because pkg/browser and an HTTP probe are a
// world the root has no reason to see.
package browser

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	pkgbrowser "github.com/pkg/browser"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// The window between a tunnel being ready and the edge serving it. Ten seconds
// is far longer than the gap has been observed to be — under a second — and it
// only ever costs that much when something is wrong, in which case the browser
// opens anyway rather than never.
const (
	reachableWithin = 10 * time.Second
	reachableEvery  = 250 * time.Millisecond
	reachableProbe  = 5 * time.Second
)

// Option configures an OpenerImpl at construction.
type Option = v1.Option[*OpenerImpl]

// OpenerImpl is the default opener. Both fields are seeded by New; a bare
// OpenerImpl{} has no launcher and is not a supported construction.
type OpenerImpl struct {
	launch func(string) error
	window time.Duration
}

// New returns an opener that probes for ten seconds and launches the host's
// browser, then configured by opts.
func New(opts ...Option) *OpenerImpl {
	o := v1.Apply(&OpenerImpl{}, WithLaunch(pkgbrowser.OpenURL), WithWindow(reachableWithin))
	return v1.Apply(o, opts...)
}

// WithLaunch replaces the browser launcher, so a test can observe the call
// without a window appearing on whoever is running it.
func WithLaunch(launch func(string) error) Option {
	return func(o *OpenerImpl) { o.launch = launch }
}

// WithWindow bounds how long Open waits for the edge before launching anyway.
func WithWindow(d time.Duration) Option {
	return func(o *OpenerImpl) { o.window = d }
}

// Open waits for the edge to serve addr, then launches a browser on it. The
// wait is bounded by the window; the launch happens either way, since a page
// that may work beats no page at all.
func (o *OpenerImpl) Open(ctx context.Context, addr string, stderr io.Writer, log v1.Logger) {
	awaitReachable(ctx, addr, o.window, log)
	openInBrowser(o.launch, addr, stderr, log)
}

// awaitReachable waits for the edge to actually serve addr before a browser is
// pointed at it.
//
// TunnelReady, which URL has already waited on, means the connection is up and
// the hostname resolves. It does not mean the edge has finished registering
// the route: for a moment after that it answers 530, and a browser opened into
// that window shows an error page for a tunnel that is about to work. Measured
// at roughly half a second, which is exactly long enough to be the first thing
// somebody sees.
//
// Anything the origin itself produced ends the wait, 404 and 401 included —
// the question is whether the route is live, not whether the app is happy. A
// 5xx is the edge saying it still cannot reach the tunnel. Giving up opens the
// browser regardless: a page that may work beats no page at all, and the
// warning says which happened.
func awaitReachable(ctx context.Context, addr string, within time.Duration, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()

	client := &http.Client{Timeout: reachableProbe}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, addr, nil)
		if err != nil {
			return // a URL this far in is well-formed; nothing to retry
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < http.StatusInternalServerError {
				return
			}
			log.Debug("edge not serving yet", "url", addr, "status", resp.StatusCode)
		}

		select {
		case <-ctx.Done():
			log.Debug("opening a browser before the edge answered", "url", addr)
			return
		case <-time.After(reachableEvery):
		}
	}
}

// openInBrowser launches a browser on addr, reporting a failure to the debug
// log and nowhere else: the tunnel is up and serving either way, and a
// headless host — a server, a container, CI — is a normal place to run this,
// not a broken one. A warning on stderr told those runs, every time, about a
// thing they were never going to do. --no-open (or v1.NoOpenEnv) skips the
// attempt entirely, and --log-level=debug is where to look when a browser was
// wanted and none appeared.
//
// pkg/browser wires the spawned process's output to its package-level Stdout,
// which defaults to os.Stdout — the one stream tunneld promises carries
// nothing a running tunnel writes. Both are pointed at stderr before the
// child can write a word. They are package globals, so this is process-wide;
// tunneld owns its process, and an embedding program gets the same guarantee
// it wants anyway.
func openInBrowser(launch func(string) error, addr string, stderr io.Writer, log *slog.Logger) {
	pkgbrowser.Stdout, pkgbrowser.Stderr = stderr, stderr
	if err := launch(addr); err != nil {
		log.Debug("could not open a browser", "url", addr, "error", err)
	}
}
```

- [ ] **Step 4: Run the browser tests**

Run: `go test ./v1alpha1/browser/`
Expected: PASS.

- [ ] **Step 5: Wire the root and remove the old code**

`v1alpha1/v1alpha1.go`: import `"github.com/tunnel-pizza/tunneld/v1alpha1/browser"`, `"context"` and `"io"` (if not already). Add to the contracts section, after `Panel`:

```go
// Opener puts a public address in front of a person once the edge serves it.
type Opener interface {
	Open(ctx context.Context, addr string, stderr io.Writer, log v1.Logger)
}

// WithOpener replaces what opens the public address once the tunnel is live.
// The default is browser.New(): probe the edge, then the host's browser.
func WithOpener(o Opener) Option {
	return func(b *BuilderImpl) { b.opener = o }
}
```

Field, after `panel`: `opener Opener`. Default in `New`, after `WithPanel`: `WithOpener(browser.New()),`. Assertion: `_ Opener = (*browser.OpenerImpl)(nil),`.

`v1alpha1/tunnel.go`:

- Delete `var openURL = browser.OpenURL` and its doc.
- Delete the `reachableWithin`/`reachableEvery`/`reachableProbe` const block and its doc.
- Delete `awaitReachable` and `openInBrowser` with their docs.
- In `run`, replace the two calls with one:

```go
	if !b.noOpen {
		// One page, never a fan of tabs: the panel when there is one, since it
		// reaches every origin, and otherwise the default origin itself.
		target := cmp.Or(view, PublicURL(public, 0, len(origins)))
		b.opener.Open(ctx, target, stderr, log)
	}
```

- Remove the imports the compiler now reports unused: `"net/http"`, `"time"`, `"github.com/pkg/browser"`.

`v1alpha1/tunnel_test.go`: delete `TestOpenInBrowser`, `swapOpener` and `TestAwaitReachable` with their docs. Remove the imports the compiler reports unused — expect `"context"`, `"io"`, `"net/http"`, `"net/http/httptest"`, `"sync/atomic"`, `"time"` and `"github.com/pkg/browser"`; keep whatever the remaining tests still use.

- [ ] **Step 6: Build, vet, test**

Run: `gofmt -l . && go vet ./... && go build ./... && go test ./... && grep -n 'openURL\|awaitReachable\|openInBrowser' v1alpha1/*.go`
Expected: clean, PASS, grep prints nothing.

- [ ] **Step 7: Commit**

```bash
git add v1alpha1
git commit -m "refactor: the edge probe and browser launch are package browser

Behind an Opener contract, so run never sees pkg/browser and a test
hands in an opener that records instead of opening. The probe window
and the launcher are options on the implementation, which retires the
package-level openURL variable that was standing in for a seam.

Refs #38."
```

---

### Task 8: `docker.TargetsImpl` and `TargetImpl`, behind the `Targets` contract

**Files:**
- Modify: `v1alpha1/attach/docker/docker.go` (`Attacher` → `TargetImpl`; `Open` → `open`; add `Option`, `TargetsImpl`, `New`, `Open`; assertion)
- Modify: `v1alpha1/attach/docker/docker_test.go` (call sites; two new cases)
- Modify: `v1alpha1/v1alpha1.go` (contract, field, option, default, assertion)
- Modify: `v1alpha1/origins.go` (delete `openTarget`; `bindOrigins` takes a `Targets`)
- Modify: `v1alpha1/origins_test.go` (fake `Targets`)
- Modify: `v1alpha1/tunnel.go` (`run` passes `b.targets`)

**Interfaces:**
- Consumes: `attach.Target`; `v1.Option`, `v1.Apply`; root `Option`, `New`.
- Produces: root `type Targets interface { Open(ctx context.Context, ref string, log v1.Logger) (attach.Target, error) }`; `func WithTargets(t Targets) Option`; `docker.New(opts ...docker.Option) *docker.TargetsImpl`; `bindOrigins(ctx context.Context, targets Targets, display []*url.URL, log *slog.Logger) (*bound, error)`. Task 9 reuses `stubTargets` from `origins_test.go`.

- [ ] **Step 1: Write the failing docker tests**

In `v1alpha1/attach/docker/docker_test.go`, every `:= Open(` becomes `:= open(` — eleven sites:

```bash
sed -i '' -e 's/:= Open(/:= open(/g' v1alpha1/attach/docker/docker_test.go
grep -c ':= open(' v1alpha1/attach/docker/docker_test.go   # 11
```

Then add, after `TestOpenWithoutDaemon`:

```go
// TestOpenFailureIsNil pins the nil-interface trap. open returns a typed
// nil on failure, and forwarding that as attach.Target would produce an
// interface value that is not nil and panics on first use. bindOrigins checks
// err first, so this would only surface in a caller that checked the target —
// which is exactly the caller nobody tests.
func TestOpenFailureIsNil(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	target, err := New().Open(ctx, "api", discard())
	if err == nil {
		_ = target.Close()
		t.Fatal("Open with no daemon succeeded, want an error")
	}
	if target != nil {
		t.Errorf("Open returned a non-nil Target alongside the error: %#v", target)
	}
}
```

and, after `TestAttach`:

```go
// TestNewOpensTarget pins the contract the root consumes: New().Open hands
// back an attach.Target for a running container, named by the reference it
// was given, and closing it releases the client.
func TestNewOpensTarget(t *testing.T) {
	cli := withDaemon(t)
	id := startContainer(t, cli, true, true)

	target, err := New().Open(t.Context(), id, discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if target.Name() != id {
		t.Errorf("Name() = %q, want the reference %q", target.Name(), id)
	}
	if err := target.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./v1alpha1/attach/docker/ 2>&1 | head`
Expected: build failure — `undefined: open`, `undefined: New`.

- [ ] **Step 3: Rewrite the docker surface**

In `v1alpha1/attach/docker/docker.go`:

Package doc, first sentence: `// Package docker attaches to a running container over the Docker Engine API, implementing the Target the attach package serves.` → keep, and add after the existing paragraph:

```go
//
// Two types, because the root needs two things: TargetsImpl opens a
// reference — a name, an id, a Compose service — and TargetImpl is the
// container it found. The first is behind the root's Targets contract; the
// second is behind attach.Target.
```

Add `"github.com/tunnel-pizza/tunneld/v1alpha1/attach"` to the imports.

Rename the struct: `type Attacher struct {` → `type TargetImpl struct {`, doc `// Attacher is one container, resolved and inspected.` → `// TargetImpl is one container, resolved and inspected.`. Every method receiver `(a *Attacher)` → `(a *TargetImpl)`. The return in what is now `open`: `return &Attacher{` → `return &TargetImpl{`.

Rename the function: `func Open(ctx context.Context, ref string, log *slog.Logger) (*Attacher, error)` → `func open(ctx context.Context, ref string, log *slog.Logger) (*TargetImpl, error)`; its doc's first word `Open` → `open`. Body unchanged.

Add, before `open`:

```go
// TargetsImpl is the default source of targets: the daemon named by the
// environment, $DOCKER_HOST and friends.
type TargetsImpl struct{}

// Option configures a TargetsImpl at construction. There are none yet; the
// signature exists so a knob added later changes no caller.
type Option = v1.Option[*TargetsImpl]

// New returns the default source of targets, configured by opts.
func New(opts ...Option) *TargetsImpl {
	return v1.Apply(&TargetsImpl{}, opts...)
}

// Open resolves ref against the daemon and hands back the container as an
// attach.Target. It is open with the concrete type erased — and the erasure
// is why this is not `return open(ctx, ref, log)`: a nil *TargetImpl
// forwarded into an interface is an interface that is not nil, and a caller
// checking the target rather than the error would use it and panic.
func (*TargetsImpl) Open(ctx context.Context, ref string, log v1.Logger) (attach.Target, error) {
	target, err := open(ctx, ref, log)
	if err != nil {
		return nil, err
	}
	return target, nil
}

// TargetImpl is what attach serves; a drift in either package fails here.
var _ attach.Target = (*TargetImpl)(nil)
```

- [ ] **Step 4: Run the docker tests**

Run: `go test ./v1alpha1/attach/docker/ -v 2>&1 | grep -E '^(=== RUN|--- (PASS|FAIL|SKIP)|ok|FAIL)'`
Expected: `TestOpenWithoutDaemon`, `TestOpenFailureIsNil`, `TestSelfIDs`, `TestOpenScopesByMountinfo` PASS; the daemon-backed ones PASS with Docker and `alpine` present, SKIP otherwise. Say which happened.

- [ ] **Step 5: Rewrite the origins fake**

`v1alpha1/origins_test.go`. Add `"github.com/tunnel-pizza/tunneld/v1"` (as `v1`) to the imports and drop `"log/slog"` only if nothing else uses it (the `bindOrigins` calls still pass a logger — keep it). Replace `withStubOpener` with a type:

```go
// stubTargets stands in for the daemon: it records every reference it was
// asked for and answers with a stubTarget. failOn, when positive, makes that
// call fail instead — the unwinding case needs one success before one
// failure.
type stubTargets struct {
	asked  []string
	failOn int
	opened []*stubTarget
}

func (s *stubTargets) Open(_ context.Context, ref string, _ v1.Logger) (attach.Target, error) {
	s.asked = append(s.asked, ref)
	if s.failOn > 0 && len(s.asked) == s.failOn {
		return nil, errors.New("no such container")
	}
	target := &stubTarget{name: ref}
	s.opened = append(s.opened, target)
	return target, nil
}
```

Then the three tests:

```go
func TestBindOriginsKeepsOrder(t *testing.T) {
	targets := &stubTargets{}
	display := mustURLs(t, "http://localhost:3000", "dockerd://api", "http://localhost:4000", "dockerd://db")

	bound, err := bindOrigins(t.Context(), targets, display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("bindOrigins: %v", err)
	}
	defer bound.Close()

	if len(bound.dialable) != len(display) {
		t.Fatalf("dialable has %d entries, want %d", len(bound.dialable), len(display))
	}
	if got := bound.dialable[0].String(); got != "http://localhost:3000" {
		t.Errorf("dialable[0] = %q, want the http origin unchanged", got)
	}
	if got := bound.dialable[2].String(); got != "http://localhost:4000" {
		t.Errorf("dialable[2] = %q, want the http origin unchanged", got)
	}
	for _, i := range []int{1, 3} {
		if !strings.HasPrefix(bound.dialable[i].Host, "127.0.0.1:") {
			t.Errorf("dialable[%d] = %q, want a loopback origin", i, bound.dialable[i])
		}
	}
	if want := []string{"api", "db"}; !slices.Equal(targets.asked, want) {
		t.Errorf("opened %q, want %q", targets.asked, want)
	}
}

func TestBindOriginsWithoutContainers(t *testing.T) {
	targets := &stubTargets{}
	display := mustURLs(t, "http://localhost:3000", "http://localhost:4000")

	bound, err := bindOrigins(t.Context(), targets, display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("bindOrigins: %v", err)
	}
	defer bound.Close()

	if len(targets.asked) != 0 {
		t.Errorf("opened %q, want nothing", targets.asked)
	}
	for i, u := range bound.dialable {
		if u != display[i] {
			t.Errorf("dialable[%d] = %q, want the original origin", i, u)
		}
	}
}

func TestBindOriginsUnwindsOnFailure(t *testing.T) {
	targets := &stubTargets{failOn: 2}
	display := mustURLs(t, "dockerd://api", "dockerd://missing")

	if _, err := bindOrigins(t.Context(), targets, display, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("bindOrigins succeeded, want an error")
	}
	if len(targets.opened) != 1 {
		t.Fatalf("opened %d targets, want 1", len(targets.opened))
	}
	if !targets.opened[0].closed {
		t.Error("the first container's target was left open")
	}
}
```

The doc comments on the three tests stay as they are. Add `"slices"` to the imports.

- [ ] **Step 6: Wire the root**

`v1alpha1/origins.go`: delete `openTarget` and its doc, and the `docker` import. `bindOrigins` takes the source of targets:

```go
func bindOrigins(ctx context.Context, targets Targets, display []*url.URL, log *slog.Logger) (*bound, error) {
```

and inside, `target, err := openTarget(ctx, origin.Host, log)` → `target, err := targets.Open(ctx, origin.Host, log)`. Its doc gains one sentence after the first paragraph: `// targets is what turns a container reference into something attach can serve; the default is the Docker daemon, and a test passes a stub.`

`v1alpha1/v1alpha1.go`: import `"github.com/tunnel-pizza/tunneld/v1alpha1/attach"` and `"github.com/tunnel-pizza/tunneld/v1alpha1/attach/docker"`. Add to the contracts section, after `Counter`:

```go
// Targets opens a container reference as something attach can serve.
type Targets interface {
	Open(ctx context.Context, ref string, log v1.Logger) (attach.Target, error)
}

// WithTargets replaces what a dockerd:// origin is resolved against. The
// default is docker.New(), the daemon $DOCKER_HOST names; a test hands in a
// stub and never touches a daemon.
func WithTargets(t Targets) Option {
	return func(b *BuilderImpl) { b.targets = t }
}
```

Field, last in the group: `targets Targets`. Default in `New`, last: `WithTargets(docker.New()),`. Assertion: `_ Targets = (*docker.TargetsImpl)(nil),`.

`v1alpha1/tunnel.go`, in `run`: `bound, err := bindOrigins(ctx, origins, log)` → `bound, err := bindOrigins(ctx, b.targets, origins, log)`.

- [ ] **Step 7: Build, vet, test**

Run: `gofmt -l . && go vet ./... && go build ./... && go test ./... && grep -rn 'openTarget\|docker\.Open\b\|docker\.Attacher' --include='*.go' --include='*.md' . | grep -v docs/superpowers`
Expected: clean, PASS. The grep will still list CONTRIBUTING.md until Task 10; nothing in `.go`.

- [ ] **Step 8: Commit**

```bash
git add v1alpha1
git commit -m "refactor: docker provides Targets and TargetImpl behind the root's contract

Attacher was an impl named for something other than the interface it
satisfies; it is TargetImpl now, and the function that opened one is
TargetsImpl.Open behind a Targets contract, which retires the
package-level openTarget variable. Open erases the concrete type by
hand rather than forwarding the call, because a typed nil forwarded
into an interface is an interface that is not nil.

Refs #38."
```

---

### Task 9: The wiring check, and `run` under test

**Files:**
- Modify: `v1alpha1/tunnel.go` (`wired`; `run` calls it; `events` drops its guard)
- Modify: `v1alpha1/tunnel_test.go` (fakes, harness, `TestRun`)
- Modify: `v1alpha1/v1alpha1_test.go` (becomes internal; `TestNewWiresEveryCollaborator`, `TestContractOptionsLand`)

**Interfaces:**
- Consumes: all six contracts and options; `stubTargets`/`stubTarget` from `origins_test.go`; `counter.New`, `counter.WithMaxGone`; `panel.New`.
- Produces: `func (b *BuilderImpl) wired() error`.

- [ ] **Step 1: Write the failing wiring tests**

`v1alpha1/v1alpha1_test.go` becomes `package v1alpha1` — it needs the fields. Change the package clause, drop the `v1alpha1` import, and drop the `v1alpha1.` prefix on every identifier in the existing three tests (`New`, `WithName`, `WithURL`, `WithOpen`). Then add:

```go
// TestNewWiresEveryCollaborator pins that New seeds all six, and that each
// With* option lands: a nil handed to one is what wired names. A bare
// BuilderImpl{} fails the same check on its first collaborator.
func TestNewWiresEveryCollaborator(t *testing.T) {
	if err := New().wired(); err != nil {
		t.Fatalf("New().wired() = %v, want every collaborator seeded", err)
	}
	if err := (&BuilderImpl{}).wired(); err == nil || !strings.Contains(err.Error(), "engine") {
		t.Errorf("BuilderImpl{}.wired() = %v, want an error naming the first missing collaborator", err)
	}

	for _, tc := range []struct {
		name string
		b    *BuilderImpl
	}{
		{"engine", New(WithEngine(nil))},
		{"cache", New(WithCache(nil))},
		{"panel", New(WithPanel(nil))},
		{"opener", New(WithOpener(nil))},
		{"counter", New(WithCounter(nil))},
		{"targets", New(WithTargets(nil))},
	} {
		err := tc.b.wired()
		if err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("wired() with no %s = %v, want an error naming it", tc.name, err)
		}
	}
}

// TestContractOptionsLand pins that a contract option replaces the default
// rather than sitting beside it: what run reads is what the caller gave.
func TestContractOptionsLand(t *testing.T) {
	e, c, p, o, n, g := &fakeEngine{}, &fakeCache{}, panel.New(), &fakeOpener{}, counter.New(), &stubTargets{}
	b := New(WithEngine(e), WithCache(c), WithPanel(p), WithOpener(o), WithCounter(n), WithTargets(g))

	if b.engine != Engine(e) || b.cache != Cache(c) || b.panel != Panel(p) ||
		b.opener != Opener(o) || b.counter != Counter(n) || b.targets != Targets(g) {
		t.Error("a contract option did not land on the field run reads")
	}
}
```

Add `"strings"`, `"github.com/tunnel-pizza/tunneld/v1alpha1/counter"` and `"github.com/tunnel-pizza/tunneld/v1alpha1/panel"` to its imports. The fakes it names are defined in the next step.

- [ ] **Step 2: Write the fakes, the harness and `TestRun`**

Append to `v1alpha1/tunnel_test.go`. Add to its imports whatever these need that the file no longer has: `"context"`, `"fmt"`, `"io"`, `"slices"`, `"github.com/cnuss/libtunnel"`, `"github.com/tunnel-pizza/tunneld/v1alpha1/counter"`.

```go
// fakeTunnel is the tunnel run drives in these tests. It embeds the
// interface so only the eight methods run touches are implemented; any other
// call panics through the nil embed, which is the right outcome here.
type fakeTunnel struct {
	libtunnel.TunnelV1
	url    *url.URL
	err    error
	done   chan struct{}
	locals []*url.URL
	ics    []libtunnel.Interceptor
	listen func(libtunnel.Event)
	order  *[]string
}

// live is a tunnel that comes up on public and stays up until the test says
// otherwise; dead is one that fails before it has a URL.
func live(public string) *fakeTunnel {
	u, err := url.Parse(public)
	if err != nil {
		panic(err)
	}
	return &fakeTunnel{url: u, done: make(chan struct{})}
}

func dead(cause error) *fakeTunnel {
	done := make(chan struct{})
	close(done)
	return &fakeTunnel{err: cause, done: done}
}

func (f *fakeTunnel) URL() *url.URL {
	if f.order != nil {
		*f.order = append(*f.order, "url")
	}
	return f.url
}
func (f *fakeTunnel) Err() error            { return f.err }
func (f *fakeTunnel) Done() <-chan struct{} { return f.done }
func (f *fakeTunnel) WithLogger(*slog.Logger) libtunnel.TunnelV1 { return f }
func (f *fakeTunnel) WithContext(context.Context) libtunnel.TunnelV1 { return f }
func (f *fakeTunnel) WithEventListener(fn func(libtunnel.Event)) libtunnel.TunnelV1 {
	f.listen = fn
	return f
}
func (f *fakeTunnel) WithLocalURL(u ...*url.URL) libtunnel.TunnelV1 {
	f.locals = append(f.locals, u...)
	return f
}
func (f *fakeTunnel) WithInterceptor(ic libtunnel.Interceptor) libtunnel.TunnelV1 {
	f.ics = append(f.ics, ic)
	return f
}

// fakeEngine hands out its tunnels in order — a remint after a refused
// replay gets the second — and records what it was asked for.
type fakeEngine struct {
	tunnels   []*fakeTunnel
	specs     []string
	providers []string
}

func (f *fakeEngine) Tunnel(spec, provider string) libtunnel.TunnelV1 {
	f.specs = append(f.specs, spec)
	f.providers = append(f.providers, provider)
	tun := f.tunnels[0]
	if len(f.tunnels) > 1 {
		f.tunnels = f.tunnels[1:]
	}
	return tun
}

// fakeCache answers Cached with a fixed spec and records the rest. onSave is
// how a case ends the run: cancelling the context, failing the tunnel, or
// delivering verdicts, all after the URL is live.
type fakeCache struct {
	cached    string
	saved     bool
	discarded bool
	onSave    func()
	order     *[]string
}

func (f *fakeCache) Cached([]string, v1.Logger) string { return f.cached }
func (f *fakeCache) Discard([]string, v1.Logger)       { f.discarded = true }
func (f *fakeCache) Save([]string, v1.Logger) {
	f.saved = true
	if f.order != nil {
		*f.order = append(*f.order, "save")
	}
	if f.onSave != nil {
		f.onSave()
	}
}

// fakeOpener records what it was asked to open and opens nothing.
type fakeOpener struct {
	opened []string
	order  *[]string
}

func (f *fakeOpener) Open(_ context.Context, addr string, _ io.Writer, _ v1.Logger) {
	f.opened = append(f.opened, addr)
	if f.order != nil {
		*f.order = append(*f.order, "open")
	}
}

// runHarness is run with every collaborator faked except the two that are
// pure: the real panel, because its URL and interceptor order are what the
// assertions check, and a real counter armed at one, because the verdict
// logic is what the gone case is about. order records the effects that
// matter in the sequence they landed.
type runHarness struct {
	engine *fakeEngine
	cache  *fakeCache
	opener *fakeOpener
	order  []string
	stderr bytes.Buffer
	b      *BuilderImpl
}

func newRunHarness(t *testing.T, tun *fakeTunnel, urls ...string) *runHarness {
	t.Helper()
	t.Setenv(v1.LogEnv, "") // a developer's shell must not turn the log on
	h := &runHarness{engine: &fakeEngine{tunnels: []*fakeTunnel{tun}}}
	h.cache = &fakeCache{order: &h.order}
	h.opener = &fakeOpener{order: &h.order}
	tun.order = &h.order
	h.b = New(
		WithURL(urls...),
		WithCacheDir(t.TempDir()), // run consults the cache only with a directory
		WithEngine(h.engine),
		WithCache(h.cache),
		WithOpener(h.opener),
		WithTargets(&stubTargets{}),
		WithCounter(counter.New(counter.WithMaxGone(1))),
	)
	return h
}

// run drives the builder under the test's own context, for the cases that
// end on their own — a tunnel that fails, a verdict, a refused builder. A
// case that has to be signalled makes its own cancelable context and hands
// cancel to onSave.
func (h *runHarness) run(t *testing.T) error {
	t.Helper()
	return h.b.run(t.Context(), &h.stderr)
}

// TestRun is the composition under test: every effect run has, against fakes
// for the edge, the disk, the browser and the daemon. Each case asserts what
// the code did before it was split, so a case going red is a behaviour
// change, not a refactor.
func TestRun(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"

	t.Run("a mint reports, opens the panel, then saves", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", ":4000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel // the signal, arriving once the tunnel is live

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v, want nil after a signal", err)
		}
		if want := []string{""}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for specs %q, want a single mint", h.engine.specs)
		}
		if want := []string{"url", "open", "save"}; !slices.Equal(h.order, want) {
			t.Errorf("effects in order %v, want %v — the cache must not be written before the URL is live", h.order, want)
		}
		if want := []string{public}; !slices.Equal(h.opener.opened, want) {
			t.Errorf("opened %q, want the panel address %q", h.opener.opened, want)
		}
		for _, want := range []string{"  " + public + "\n", "    -> http://localhost:3000\n", "    -> http://localhost:4000\n"} {
			if !strings.Contains(h.stderr.String(), want) {
				t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
			}
		}
		tun := h.engine.tunnels[0]
		if len(tun.ics) != 2 || tun.ics[0].Priority >= tun.ics[1].Priority {
			t.Errorf("registered %d interceptors, want the shell then the unframer", len(tun.ics))
		}
		if len(tun.locals) != 2 {
			t.Errorf("tunnel was given %d origins, want 2", len(tun.locals))
		}
	})

	t.Run("a cached spec is what the engine replays", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.cache.cached = "cached-spec"
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if want := []string{"cached-spec"}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for %q, want the cached spec", h.engine.specs)
		}
	})

	t.Run("a replay the edge refuses is discarded and minted again", func(t *testing.T) {
		refused := dead(fmt.Errorf("edge: %w", libtunnel.ErrCredentialRejected))
		h := newRunHarness(t, refused, ":3000")
		h.engine.tunnels = append(h.engine.tunnels, live(public))
		h.cache.cached = "stale"
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v, want the remint to succeed", err)
		}
		if want := []string{"stale", ""}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for %q, want the replay then a mint", h.engine.specs)
		}
		if !h.cache.discarded {
			t.Error("the refused spec was not discarded; every later run would replay it")
		}
		if !h.cache.saved {
			t.Error("the remint was not cached")
		}
	})

	t.Run("any other replay failure is returned and the cache kept", func(t *testing.T) {
		boom := errors.New("provider unreachable")
		h := newRunHarness(t, dead(boom), ":3000")
		h.cache.cached = "good"

		err := h.run(t)
		if !errors.Is(err, boom) {
			t.Fatalf("run() = %v, want the tunnel's own error", err)
		}
		if h.cache.discarded {
			t.Error("a good spec was discarded over a failure that was not its fault")
		}
		if h.cache.saved {
			t.Error("a tunnel that never came up was cached")
		}
		if want := []string{"good"}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for %q, want one replay and no remint", h.engine.specs)
		}
	})

	t.Run("no URL and no cause is ErrNotReady", func(t *testing.T) {
		h := newRunHarness(t, &fakeTunnel{done: make(chan struct{})}, ":3000")
		err := h.run(t)
		if !errors.Is(err, v1.ErrNotReady) {
			t.Errorf("run() = %v, want ErrNotReady", err)
		}
	})

	t.Run("--no-open opens nothing", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", ":4000")
		h.b.noOpen = true // what the flag binds over
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if len(h.opener.opened) != 0 {
			t.Errorf("opened %q, want nothing", h.opener.opened)
		}
		if want := []string{"url", "save"}; !slices.Equal(h.order, want) {
			t.Errorf("effects %v, want %v", h.order, want)
		}
	})

	t.Run("one origin keeps the bare address and no panel", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if want := []string{public}; !slices.Equal(h.opener.opened, want) {
			t.Errorf("opened %q, want the plain URL %q", h.opener.opened, want)
		}
		if n := len(h.engine.tunnels[0].ics); n != 0 {
			t.Errorf("registered %d interceptors for one origin, want none", n)
		}
		if want := "  " + public + "\n    -> http://localhost:3000\n"; !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
		}
	})

	t.Run("a tunnel that fails after coming up returns its error", func(t *testing.T) {
		tun := live(public)
		h := newRunHarness(t, tun, ":3000")
		gone := errors.New("edge went away")
		h.cache.onSave = func() {
			tun.err = gone
			close(tun.done)
		}

		err := h.run(t)
		if !errors.Is(err, gone) {
			t.Errorf("run() = %v, want the tunnel's error", err)
		}
	})

	t.Run("enough gone verdicts end the run with ErrTunnelGone", func(t *testing.T) {
		tun := live(public)
		h := newRunHarness(t, tun, ":3000")
		h.cache.onSave = func() {
			tun.listen(libtunnel.Event{Kind: libtunnel.EventGone, Hostname: "foo.tunneled.pizza"})
		}

		err := h.run(t)
		if !errors.Is(err, v1.ErrTunnelGone) {
			t.Fatalf("run() = %v, want ErrTunnelGone", err)
		}
		if !strings.Contains(err.Error(), "foo.tunneled.pizza") {
			t.Errorf("error %q does not name the hostname", err)
		}
	})

	t.Run("a bare struct is refused before it touches anything", func(t *testing.T) {
		b := &BuilderImpl{urls: []string{":3000"}}
		err := b.run(t.Context(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "construct it with New") {
			t.Errorf("run() on a bare BuilderImpl = %v, want the wiring error", err)
		}
	})
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./v1alpha1/ -run 'TestRun|TestNewWires|TestContractOptions' 2>&1 | head`
Expected: build failure — `b.wired undefined`.

- [ ] **Step 4: Add `wired`, call it, drop the guard**

`v1alpha1/tunnel.go`. At the top of `run`, before `parseOrigins`:

```go
	if err := b.wired(); err != nil {
		return err
	}
```

After `run`, before `events`:

```go
// wired reports the first collaborator New would have seeded and did not: a
// BuilderImpl assembled as a bare struct rather than through New. One check
// here, in the function that returns errors, rather than a nil guard in every
// method and in a callback that cannot report one.
func (b *BuilderImpl) wired() error {
	for _, c := range []struct {
		name    string
		missing bool
	}{
		{"engine", b.engine == nil},
		{"cache", b.cache == nil},
		{"panel", b.panel == nil},
		{"opener", b.opener == nil},
		{"counter", b.counter == nil},
		{"targets", b.targets == nil},
	} {
		if c.missing {
			return fmt.Errorf("builder has no %s: construct it with New", c.name)
		}
	}
	return nil
}
```

In `events`, delete the guard and its comment:

```go
		// A Builder assembled as a bare struct rather than through New has no
		// counter, and an event is no place to panic about it.
		if b.counter == nil {
			return
		}
```

— `run` has already refused such a builder. Update the `events` doc's last paragraph if it mentions the guard (it does not; leave it).

- [ ] **Step 5: Run the tests, then the race lane**

Run: `go test ./v1alpha1/ -run 'TestRun|TestNewWires|TestContractOptions' -v 2>&1 | grep -E '^(=== RUN|--- (PASS|FAIL)|ok|FAIL)'` then `make race`
Expected: every subtest PASS; race clean. If the gone case hangs, the counter is not armed at one — check `WithCounter` in the harness.

- [ ] **Step 6: Mutation-test the check**

Comment out the three `wired` lines at the top of `run`, run `go test ./v1alpha1/ -run 'TestRun/a_bare_struct'`, and confirm it fails (a nil-pointer panic in `b.engine.Tunnel`). Restore. Record the result for Task 11's report.

- [ ] **Step 7: Full suite, commit**

Run: `gofmt -l . && go vet ./... && go build ./... && go test ./...`
Expected: clean and PASS.

```bash
git add v1alpha1
git commit -m "test: run is under test, and refuses a builder that skipped New

Every effect run has — mint or replay, discard and remint on a refused
credential, the report, the panel, the browser, the save after the URL
is live, the gone verdict — is asserted against fakes for the edge,
the disk, the browser and the daemon. One wiring check at the top of
run replaces the nil guard events carried for the counter alone.

Refs #38."
```

---

### Task 10: Documentation

**Files:**
- Modify: `CONTRIBUTING.md` (file map, module layout note, design conventions, container origins, conventions-that-bite paths, adding a flag, new "Adding a collaborator")
- Modify: `README.md` (Layout tree, contributor note under the API block)
- Modify: `CLAUDE.md` (read-first list)

**Interfaces:** none — prose only. Every name below exists after Task 9.

- [ ] **Step 1: CONTRIBUTING file map**

In the "Where to find things" table, replace the `v1alpha1/v1alpha1.go`, `v1alpha1/builder.go` and `v1alpha1/multiview/` rows and add the rest, so the `v1alpha1` block reads:

```markdown
| `New`, `BuilderImpl`, the internal contracts + options | [`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go)                 |
| Builder options + command assembly             | [`v1alpha1/builder.go`](./v1alpha1/builder.go)                   |
| Tunnel run, origin parsing, output contract    | [`v1alpha1/tunnel.go`](./v1alpha1/tunnel.go)                     |
| Origin binding (`dockerd://` → loopback)       | [`v1alpha1/origins.go`](./v1alpha1/origins.go)                   |
| Version resolution + build banner              | [`v1alpha1/version.go`](./v1alpha1/version.go)                   |
| Env helpers (`EnvBool`, `EnvDuration`, `Logger`) | [`v1alpha1/env.go`](./v1alpha1/env.go)                         |
| Tunnel engine (`Engine` ← libtunnel)           | [`v1alpha1/engine/`](./v1alpha1/engine)                          |
| Gone-verdict counter (`Counter`)               | [`v1alpha1/counter/`](./v1alpha1/counter)                        |
| Spec cache, `TUNNEL.env` (`Cache`)             | [`v1alpha1/cache/`](./v1alpha1/cache)                            |
| Multiview panel, framing headers, template (`Panel`) | [`v1alpha1/panel/`](./v1alpha1/panel)                      |
| Edge probe + browser launch (`Opener`)         | [`v1alpha1/browser/`](./v1alpha1/browser)                        |
| Container terminal origin (`attach.Target`, `Server`) | [`v1alpha1/attach/`](./v1alpha1/attach)                   |
| Docker provider of targets (`Targets`)         | [`v1alpha1/attach/docker/`](./v1alpha1/attach/docker)            |
```

- [ ] **Step 2: CONTRIBUTING module layout and design conventions**

In "Module layout", `Application code constructs from `v1alpha1` (`v1alpha1.New()…`)` → `Application code constructs from `v1alpha1` (`v1alpha1.New(opts...)`)`, and in the same paragraph `v1alpha1.New().WithName("expose")` → `v1alpha1.New(v1alpha1.WithName("expose"))`.

In "Design conventions", replace the **Surface/engine split** paragraph with two:

```markdown
**Surface/engine split.** `v1.Builder` is only what a caller calls once the
builder exists: `Build` and `Name`. Everything `run` composes that owns an
external effect — the edge, the disk, the daemon, the browser, an HTTP probe —
is an internal contract in [`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go):
`Engine`, `Cache`, `Panel`, `Opener`, `Counter`, `Targets`. Each has one
implementation, named `XImpl`, in its own `v1alpha1/<name>` subpackage,
seeded by `New` and replaceable with the matching `With*` option. A function
that maps a value to a value (`parseOrigins`, `PublicURL`, `Version`) gets no
interface. The assertion block in `v1alpha1.go` is where a default that drifts
from its contract fails — at build, not at the first run.

**One way to configure anything.** `v1.Option[T]` is `func(T)`. Every package
aliases it to its own `Option`, exposes `With*` constructors returning it, and
its `New(opts ...Option)` applies its defaults first and the caller's after —
later wins. That holds for the builder's flag seeds, the contract injectors
and each implementation's tunables alike, and a package with no tunables still
takes the variadic so adding one changes no caller. An option is a plain
function, so it can be applied anywhere a setter used to be called:
`cacheDirValue.Set` is `WithCacheDir(s)(v.b)`.
```

In **Seeds are defaults, not settings**: `Every `With*` value becomes` → `Every `With*` option's value becomes`.

In **Implementations grow as `v1alpha1/<name>` subpackages**: `[`v1alpha1/multiview`](./v1alpha1/multiview)` → `[`v1alpha1/panel`](./v1alpha1/panel)`.

- [ ] **Step 3: CONTRIBUTING conventions that bite, container origins**

In "Conventions that bite", the panel bullet's trailing `See [`v1alpha1/multiview`](./v1alpha1/multiview).` → `See [`v1alpha1/panel`](./v1alpha1/panel).`

In "Container origins", after the first paragraph, add:

```markdown
`docker.TargetImpl` is that provider. What opens one by reference —
`docker.TargetsImpl.Open` — sits behind the root's `Targets` contract, which is
how `bindOrigins` is tested with a stub and no daemon.
```

- [ ] **Step 4: CONTRIBUTING adding a flag, adding a collaborator**

In "Adding a flag", step 1 becomes:

```markdown
1. a `With*` option in [`v1alpha1/builder.go`](./v1alpha1/builder.go), so an
   embedder can seed it — with the doc that says what it seeds and that the
   flag overrides it;
```

and step 3 becomes:

```markdown
3. the field on `BuilderImpl` and the `cmd.Flags()` binding in `v1alpha1` — the
   binding's default is the seeded field, never a literal — plus a row in
   `flagEnv` in [`v1alpha1/env.go`](./v1alpha1/env.go) pairing the flag with
   the constant;
```

After the "Adding a flag" section, add:

```markdown
## Adding a collaborator

A new thing `run` composes that has an external effect gets a contract, not
a function. Seven things move together, and `TestNewWiresEveryCollaborator`
plus the assertion block catch the ones that are easy to forget:

1. the interface in [`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go), beside
   the others, with flag-settled configuration as method arguments rather
   than constructor state;
2. `v1alpha1/<name>/<name>.go` with `XImpl`, `type Option =
   v1.Option[*XImpl]`, `New(opts ...Option) *XImpl`, and a `With*` per
   tunable;
3. the field on `BuilderImpl`, in the collaborators group;
4. `With<Name>(x <Name>) Option` in `v1alpha1.go`, and the default in `New`;
5. a line in the assertion block, and one in `wired`;
6. a fake in `v1alpha1/tunnel_test.go` and a case in `TestRun` for what
   `run` does with it;
7. a row in the file map above.
```

- [ ] **Step 5: README and CLAUDE.md**

`README.md`, Layout tree — add after the `v1alpha1` entry:

```
github.com/tunnel-pizza/tunneld/v1alpha1/<name>  — one implementation each: engine,
                                            counter, cache, panel, browser, attach,
                                            attach/docker. Behind the contracts in
                                            v1alpha1; see CONTRIBUTING.
```

`README.md`, after the first "API at a glance" code block (the one ending with `WithStderr`), add:

```markdown
`BuilderImpl` also takes `WithEngine`, `WithCache`, `WithPanel`, `WithOpener`,
`WithCounter` and `WithTargets`, which swap the collaborators the tunnel run
composes. They are a contributor's and a test's concern, not an embedder's —
see [CONTRIBUTING.md → Design conventions](./CONTRIBUTING.md#design-conventions).
```

`CLAUDE.md`, "Read first, in order" — insert after item 3:

```markdown
4. [`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go) — `New`, the six internal
   contracts, and their options
```

and renumber the two that follow (`tunnel.go` → 5, `main.go` → 6).

- [ ] **Step 6: Check every path resolves, commit**

Run: `grep -rn 'v1alpha1/multiview\|v1alpha1/env\b\|counters\.go\|docker\.Attacher\|docker\.Open\b\|NewCounter' README.md CONTRIBUTING.md CLAUDE.md`
Expected: nothing.

```bash
git add README.md CONTRIBUTING.md CLAUDE.md
git commit -m "docs: the contracts, the option convention, and where each impl lives

Refs #38."
```

---

### Task 11: Verification, memory, PR

**Files:** none in the repo beyond what earlier tasks produced. Memory files under `~/.claude/projects/-Users-christian-tunnel-pizza-tunneld/memory/`.

- [ ] **Step 1: The full gate**

Run: `make all && make race`
Expected: both green. `make all` includes `make e2e`, which builds the binary and every example and drives their `--help`.

- [ ] **Step 2: The docker lane**

Run: `docker info >/dev/null 2>&1 && docker image inspect alpine >/dev/null 2>&1 && go test ./v1alpha1/attach/... -count=1 -v 2>&1 | grep -E '^(--- (PASS|FAIL|SKIP)|ok|FAIL)' || echo "docker lane: SKIPPED (no daemon or no alpine)"`
Expected: every test PASS with a daemon and `alpine`; otherwise the SKIPPED line. Report which — a skipped lane is not a passed one.

- [ ] **Step 3: The live comparison**

Two throwaway origins, both binaries built up front, each run for twenty
seconds and stopped with SIGINT — the same signal Ctrl-C sends, so the run
ends the way an operator's would. `--cache-dir false` keeps the comparison
from writing credentials anywhere, and nothing here deletes a cache directory:
another project's `TUNNEL.env` under `~/Library/Caches/tunneld` is a spec
somebody may still be resuming.

```bash
T=$(mktemp -d)
python3 -m http.server 3000 >/dev/null 2>&1 & P1=$!
python3 -m http.server 4000 >/dev/null 2>&1 & P2=$!

go build -o "$T/branch" .
git worktree add -q "$T/main-src" main
( cd "$T/main-src" && go build -o "$T/main" . )
git worktree remove "$T/main-src"

for bin in main branch; do
  "$T/$bin" --url :3000 --url :4000 --cache-dir false --no-open 2>"$T/$bin.err" & PID=$!
  sleep 20; kill -INT $PID; wait $PID; echo "$bin exit $?"
  sed -E 's/[a-z-]+\.tunneled\.pizza/HOST/g; s/tunneld [^ ]+ \(libtunnel [^,]+,/tunneld VERSION (libtunnel VERSION,/' "$T/$bin.err" \
    | grep -v '^time=' > "$T/$bin.norm"
done
diff "$T/main.norm" "$T/branch.norm" && echo "report: identical shape"
```

Expected: both exit 0 (a signal after the URL is live is a clean shutdown), and
the diff prints nothing followed by "report: identical shape" — a banner line,
the panel address, two indented origins. Then the cache write, into a
directory this step owns, and the browser open, once, by eye:

```bash
C=$(mktemp -d)
"$T/branch" --url :3000 --cache-dir "$C" & PID=$!
sleep 20; kill -INT $PID; wait $PID
ls -la "$C/TUNNEL.env"
kill $P1 $P2
rm -rf "$T" "$C"
```

Expected: a browser tab opened on the tunnel's address within a second of the
address being reported, and `TUNNEL.env` exists with mode `-rw-------`. Close
the tab. Report each of the three observations as seen, not assumed.

- [ ] **Step 4: The other two mutations**

Each: break, run, confirm red, restore, confirm green.

1. In `v1alpha1/panel/panel.go`, swap the two elements in `Interceptors`. `go test ./v1alpha1/panel/ -run TestInterceptorsOrder` and `go test ./v1alpha1/ -run 'TestRun/a_mint'` both fail. Restore.
2. In `v1alpha1/v1alpha1.go`, swap the two `v1.Apply` calls in `New` (apply `opts` first, then the defaults). `go test ./v1alpha1/ -run 'TestCallerOptionBeatsDefault|TestWithOpenSeedsTheDefault'` fails. Restore.

`git status` must be clean after both.

- [ ] **Step 5: Update the memory that names moved code**

In `~/.claude/projects/-Users-christian-tunnel-pizza-tunneld/memory/tunneld-shipped-state.md`, the bullet about `awaitReachable` now points at `v1alpha1/browser`. In `previewing-the-multiview-panel.md`, any `v1alpha1/multiview` path becomes `v1alpha1/panel`. Add a project memory `contracts-and-options.md` recording, as of 2026-09-06: six contracts in `v1alpha1.go`, `XImpl` in subpackages, `v1.Option[T]` everywhere, `run` tested through `TestRun`, and the decision that `Count` returns nothing because of identical-return-type satisfaction. Add its line to `MEMORY.md`.

- [ ] **Step 6: Push and open the PR**

```bash
git push -u origin refactor/contracts
gh pr create --title "refactor: one contract per collaborator, one XImpl each, options everywhere" --body "$(cat <<'EOF'
Closes #38.

Six internal contracts in `v1alpha1` — `Engine`, `Cache`, `Panel`, `Opener`, `Counter`, `Targets` — each with one `XImpl` in its own subpackage, seeded by `New` and swappable through a `With*` option. `v1.Builder` is `Build` and `Name`; every knob in the tree is a `v1.Option[T]`, defaults first, caller after.

`run` is under test for the first time (`TestRun`, twelve cases against fakes for the edge, the disk, the browser and the daemon). No flag, variable, output line or exit code changes; the embedder's call goes from `New().WithURL(u).Build()` to `New(WithURL(u)).Build()`.

Design: `docs/superpowers/specs/2026-09-06-contracts-design.md`. Plan: `docs/superpowers/plans/2026-09-06-contracts.md`.

Verified: `make all`, `make race`, the docker lane (<PASS or SKIPPED — say which>), a live two-origin run whose report matches `main` line for line, and three mutations each confirmed red then green.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01AUFeC4pwfsCUZBUd52MNdQ
EOF
)"
gh pr merge --auto --squash
```

Fill the docker-lane placeholder in the body with what Step 2 actually produced before running `gh pr create`.

---

## Self-review

**Spec coverage.** Contracts → Tasks 3–8 (one each, exact signatures). Option type and `Apply` → Task 1. `v1.Builder` shrink, nine options, `cacheDirValue` → Task 2. `New` two-tier → Task 2 (booleans) and Tasks 3–8 (collaborators). Assertion block → built up across Tasks 3–8. `run` substitutions → Tasks 4–8. Wiring check → Task 9. `TestRun`'s twelve cases → Task 9 (ten subtests; "cancelled context returns nil" is the first case's exit path, and "both interceptors registered in order" is asserted inside the panel case). `engine_test`, `counter_test` inversion, `cache_test`, `panel_test` order case, `browser_test` moved + `TestOpen`, `docker_test` sites + two cases, `origins_test` stub, `v1alpha1_test` wiring, `example_test`, examples, `env_test` → Tasks 2–9. README, CONTRIBUTING, CLAUDE.md, package docs, `multiview` paths → Tasks 2 and 10. Verification and mutations → Tasks 9 and 11.

**Placeholders.** None: every step carries its code or its exact edit. Two compiler-driven steps ("remove the imports the compiler reports unused", Tasks 7) name the expected imports.

**Type consistency.** `Counter.Count(e libtunnel.Event)` with no return, in the contract (Task 3), the impl (Task 3), `events` (Task 3), and the fake harness (Task 9, real counter). `Targets.Open` returns `(attach.Target, error)` in the contract (Task 8), `TargetsImpl.Open` (Task 8), `stubTargets.Open` (Task 8). `bindOrigins(ctx, targets, display, log)` in Task 8's source, tests and `run`. `Opener.Open(ctx, addr, stderr, log)` in Task 7's contract, impl and Task 9's fake. `Panel.Interceptors(origins, log)` in Task 6 and Task 9. `v1.Logger` is `*slog.Logger`, so the unexported functions that keep `*slog.Logger` match.
