# Spec cache by origins, and origins as a type — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Name a cached tunnel spec after the run it belongs to, so two runs from one project stop taking each other's tunnel.

**Architecture:** The origins stop being a bare `[]*url.URL` and become `v1.Origins`, implemented once in `v1alpha1/origins`, carrying a `Key()` — sixteen hex of SHA-256 over the working directory and the sorted, deduplicated origin URLs. The cache then names its file `<Key()>.env` in one flat directory and hashes nothing itself. `--cache-dir` and its list machinery are deleted; caching is off when the builder's cache is nil.

**Tech Stack:** Go 1.26, cobra/pflag, viper (env-file parsing), `v1.Option[T]` functional options.

**Spec:** [`docs/superpowers/specs/2026-09-11-cache-and-origins-design.md`](../specs/2026-09-11-cache-and-origins-design.md)

## Global Constraints

- **Issue:** #115. One branch, one pull request, `Closes #115.` in the body. The branch `feat/cache-by-origins` already exists and carries the spec commit.
- **No backwards compatibility.** Existing cache files are orphaned deliberately. Do not write a migration, a fallback read, or a deprecation shim.
- **Tests go beside their source**: `something.go` → `something_test.go`, one file per source file. New cases join the table in the existing file. See CONTRIBUTING → One test file per source file.
- **The contract count drops from eight to seven.** `CacheDirs` is deleted and nothing replaces it. `Origins` is a type, not a contract: it gets no entry in the contract list in `v1alpha1/v1alpha1.go` and no `With*` option of its own.
- **Every flag is mirrored by a variable** in the `flagEnv` map in `v1alpha1/builder.go:199`.
- **Cache key width is 16** hex characters, and the file extension is `.env`.
- **Cache directory is `<os.UserCacheDir()>/.tunneld`**, computed once at construction, overridden only by `cache.WithDir`.
- **Never log a spec.** Paths yes, contents no.
- `make check` and `make race` must both be green before the final commit of every task.
- Commit messages: conventional prefix, body explains the *why*, wrapped at ~72 columns, ending with the two attribution lines used by the repo's recent commits (`Co-Authored-By:` and `Claude-Session:`).
- **Do not push and do not open the PR** until the human asks.

---

## File Structure

**Created**

| File | Responsibility |
|---|---|
| `v1alpha1/origins/origins.go` | `OriginsImpl`: the list, its options, and `Key()`. Knows the working directory and nothing about caches. |
| `v1alpha1/origins/origins_test.go` | Key stability, `WithDir`, `WithURL` appending, the defensive copy. |

**Modified**

| File | Change |
|---|---|
| `v1/v1.go` | Declare `Origins`; `Builder.Origins() Origins`; delete `CacheDirEnv`; add `NoCacheEnv`; fix the `--cache-dir=false` mention at :172. |
| `v1alpha1/v1alpha1.go` | `type Origins = v1.Origins`; `Cache` takes origins; delete `CacheDirs`/`WithCacheDirs`; `WithCache` accepts nil; add `WithCacheDir`; `Display`/`Binder` take `Origins`; drop `cachedir` from imports, defaults and assertions. |
| `v1alpha1/builder.go` | `Origins()` returns `Origins`; `--no-cache` flag; the three cache guards; `VersionLine` calls; the address/report loops. |
| `v1alpha1/version.go` | `VersionLine(origins Origins) string`. |
| `v1alpha1/cache/cache.go` | Rewritten around one directory and `origins.Key()`. |
| `v1alpha1/cache/cache_test.go` | Same cases, new shape. |
| `v1alpha1/display/display.go` | `URL` and `Interceptors` take `Origins`. |
| `v1alpha1/attach/attach.go` | `Bind` takes and returns `Origins`. |
| `e2e/e2e_test.go` | `TUNNELD_NO_CACHE=true` replaces the cache-dir redirect. |
| `examples/docker-compose/docker-compose.yml` | `XDG_CACHE_HOME`, volume at `/var/run/.tunneld`. |
| `Makefile`, `.gitignore`, `README.md`, `CONTRIBUTING.md`, `CLAUDE.md` | Fallout. |

**Deleted**

| File | Why |
|---|---|
| `v1alpha1/cachedir/cachedir.go` | The list, and `true`/`false`-as-a-value, have nowhere to go. |
| `v1alpha1/cachedir/cachedir_test.go` | With it. |

---

### Task 1: The origins type

**Files:**
- Create: `v1alpha1/origins/origins.go`
- Create: `v1alpha1/origins/origins_test.go`
- Modify: `v1/v1.go` (add the `Origins` interface; leave `Builder.Origins()` alone for now)

**Interfaces:**
- Consumes: `v1.Option[T]` and `v1.Apply`, already in `v1/v1.go`.
- Produces: `v1.Origins` with `Len() int`, `At(i int) *url.URL`, `URLs() []*url.URL`, `Key() string`; `origins.New(opts ...origins.Option) *origins.OriginsImpl`; `origins.WithURL(url ...*url.URL) origins.Option`; `origins.WithDir(dir string) origins.Option`.

- [ ] **Step 1: Declare the interface in `v1/v1.go`**

Put it immediately above the `Builder` interface. `net/url` is already imported there.

```go
// Origins is the local origins a run exposes, in order: the first is the
// default and each later one answers on a bare ?n routing parameter.
//
// A type rather than a []*url.URL, because the list answers a question no
// slice can: Key is what identifies this run's tunnel, and it is the name its
// cached spec is filed under. Everything else here is the slice's own
// vocabulary, so a caller reads it the way it would read a slice.
type Origins interface {
	// Len is how many origins this run exposes.
	Len() int
	// At is the origin at i, which the caller has already bounded by Len.
	At(i int) *url.URL
	// URLs is every origin, in order, as a copy — sorting or reordering what
	// comes back cannot reorder the run.
	URLs() []*url.URL
	// Key identifies the tunnel these origins are: the working directory they
	// were settled in and the origins themselves, sorted so the order they
	// were typed does not make a second tunnel, deduplicated so a repeat does
	// not either. Stable across runs, and safe as a filename.
	Key() string
}
```

- [ ] **Step 2: Write the failing test**

Create `v1alpha1/origins/origins_test.go`:

```go
package origins

import (
	"net/url"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// The contract this implements, asserted where the implementation is.
var _ v1.Origins = (*OriginsImpl)(nil)

// must parses a URL a test wrote, where a failure is the test's own bug.
func must(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u
}

// TestKeyIsTheSetAndWhereItRan pins what makes two runs the same tunnel: the
// same origins in any order, from the same directory. It is the whole of why
// this type exists — the cache files a spec under this name.
func TestKeyIsTheSetAndWhereItRan(t *testing.T) {
	const dir = "/work/project"
	a, b := must(t, "http://localhost:3000"), must(t, "exec:///usr/bin/htop")

	same := []struct {
		name string
		opts []Option
	}{
		{"as given", []Option{WithDir(dir), WithURL(a, b)}},
		{"reversed", []Option{WithDir(dir), WithURL(b, a)}},
		{"repeated", []Option{WithDir(dir), WithURL(a, b, a)}},
		{"appended across calls", []Option{WithDir(dir), WithURL(a), WithURL(b)}},
	}
	want := New(same[0].opts...).Key()
	for _, tc := range same[1:] {
		if got := New(tc.opts...).Key(); got != want {
			t.Errorf("%s: Key() = %q, want %q — the same tunnel", tc.name, got, want)
		}
	}

	differ := []struct {
		name string
		opts []Option
	}{
		{"another directory", []Option{WithDir("/work/other"), WithURL(a, b)}},
		{"one origin fewer", []Option{WithDir(dir), WithURL(a)}},
		{"another origin", []Option{WithDir(dir), WithURL(a, must(t, "http://localhost:3001"))}},
		{"no origins at all", []Option{WithDir(dir)}},
	}
	for _, tc := range differ {
		if got := New(tc.opts...).Key(); got == want {
			t.Errorf("%s: Key() = %q, want a different tunnel", tc.name, got)
		}
	}

	// Safe as a filename and stable in width, because it is one.
	if got := New(same[0].opts...).Key(); len(got) != keyWidth {
		t.Errorf("Key() = %q, %d characters, want %d", got, len(got), keyWidth)
	}
}

// TestNewSeedsTheWorkingDirectory pins that the default identity is where the
// process is, so a run that configures nothing still files its spec per
// project.
func TestNewSeedsTheWorkingDirectory(t *testing.T) {
	u := must(t, "http://localhost:3000")

	seeded := New(WithURL(u)).Key()
	if elsewhere := New(WithDir("/somewhere/else"), WithURL(u)).Key(); seeded == elsewhere {
		t.Errorf("Key() = %q from both the working directory and /somewhere/else", seeded)
	}
	if again := New(WithURL(u)).Key(); again != seeded {
		t.Errorf("Key() = %q then %q for the same run", seeded, again)
	}
}

// TestURLsCannotBeUsedToReorderTheRun pins the defensive copy. Key sorts, and
// a caller that sorted the slice it was handed would reorder the routing
// parameters as a side effect of asking a question.
func TestURLsCannotBeUsedToReorderTheRun(t *testing.T) {
	a, b := must(t, "http://localhost:3000"), must(t, "http://localhost:3001")
	o := New(WithURL(a, b))

	got := o.URLs()
	got[0], got[1] = got[1], got[0]

	if o.At(0) != a || o.At(1) != b {
		t.Errorf("At(0), At(1) = %v, %v, want %v, %v — URLs handed out the list itself", o.At(0), o.At(1), a, b)
	}
	if o.Len() != 2 {
		t.Errorf("Len() = %d, want 2", o.Len())
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

Run: `go test ./v1alpha1/origins/`
Expected: FAIL — `no required module provides package .../v1alpha1/origins` or undefined `New`.

- [ ] **Step 4: Write the implementation**

Create `v1alpha1/origins/origins.go`:

```go
// Package origins is the local origins a run exposes: the list, and the key
// that says which tunnel they are.
//
// A type rather than a slice because of that key. Two runs are the same tunnel
// when they serve the same origins from the same directory, and the spec cache
// files a tunnel under exactly that — so the question "are these two runs the
// same tunnel" has one answer, computed here, rather than one per caller.
package origins

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"slices"
	"strings"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// keyWidth is how much of the digest a key carries. Sixteen hex characters is
// 64 bits: far beyond collision for the number of tunnels one machine has, and
// short enough to read out of a banner and compare by eye.
const keyWidth = 16

// Option configures an OriginsImpl at construction.
type Option = v1.Option[*OriginsImpl]

// OriginsImpl is the default Origins: a list, and the directory it was settled
// in. Both are fixed once New returns.
type OriginsImpl struct {
	// dir is the working directory this run's identity is scoped to, so two
	// projects serving :3000 are two tunnels rather than one.
	dir  string
	urls []*url.URL
}

// New returns an OriginsImpl configured by opts, with the process's working
// directory already in it.
//
// A Getwd that fails leaves the directory empty rather than failing: the key
// is then the origins alone, which is a weaker identity and not a broken one —
// the same judgement the cache makes about a machine with no cache directory.
func New(opts ...Option) *OriginsImpl {
	o := &OriginsImpl{}
	if wd, err := os.Getwd(); err == nil {
		o.dir = wd
	}
	return v1.Apply(o, opts...)
}

// WithURL adds origins, in order, appending across calls — so a list can be
// assembled from more than one source without the caller joining them first.
// A repeat costs nothing: Key deduplicates, and the run exposes what it was
// given.
func WithURL(url ...*url.URL) Option {
	return func(o *OriginsImpl) { o.urls = append(o.urls, url...) }
}

// WithDir replaces the working directory the key is scoped to. What a test
// uses to fix a key without moving the process, and what an embedder uses when
// the directory it runs in is not the one it is serving for.
func WithDir(dir string) Option {
	return func(o *OriginsImpl) { o.dir = dir }
}

// Len implements v1.Origins.
func (o *OriginsImpl) Len() int { return len(o.urls) }

// At implements v1.Origins. The caller has already bounded i by Len, so an
// index outside it panics the way a slice does.
func (o *OriginsImpl) At(i int) *url.URL { return o.urls[i] }

// URLs implements v1.Origins, as a copy: Key sorts, and a caller that sorted
// what it was handed would reorder the run's routing parameters as a side
// effect of asking what they are.
func (o *OriginsImpl) URLs() []*url.URL { return slices.Clone(o.urls) }

// Key implements v1.Origins: the directory and the origins, sorted and
// deduplicated, hashed together.
//
// Sorted because the order is the run's business and not the tunnel's — ?0 and
// ?1 index the list, but serving the same two things in the other order is the
// same tunnel. Deduplicated for the same reason. Newline-joined so the parts
// cannot run together: a directory and an origin concatenated raw could be
// split two ways and collide.
func (o *OriginsImpl) Key() string {
	parts := make([]string, 0, len(o.urls))
	for _, u := range o.urls {
		parts = append(parts, u.String())
	}
	slices.Sort(parts)
	parts = slices.Compact(parts)

	sum := sha256.Sum256([]byte(o.dir + "\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:keyWidth]
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./v1alpha1/origins/ ./v1/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add v1/v1.go v1alpha1/origins/
git commit   # feat: the origins a run exposes are a type with an identity
```

---

### Task 2: The builder returns Origins

**Files:**
- Modify: `v1/v1.go` (`Builder.Origins() Origins`)
- Modify: `v1alpha1/builder.go:897` (`Origins()`), and every call site inside `Run`
- Modify: `v1alpha1/builder_test.go` (the `originStrings` helper and its callers)

**Interfaces:**
- Consumes: `origins.New`, `origins.WithURL`, `v1.Origins` from Task 1.
- Produces: `(*BuilderImpl).Origins() Origins` where `Origins` is `v1alpha1`'s alias for `v1.Origins`. Consumers downstream still take `[]*url.URL` in this task and are fed `.URLs()`; Task 3 removes those adapters.

- [ ] **Step 1: Alias the type in `v1alpha1/v1alpha1.go`**

Directly under the `Option` alias near the top of the file, so the package can name it without a `v1.` prefix. It is deliberately **not** in the contract list below it:

```go
// Origins is the local origins a run exposes — v1's type, aliased so this
// package can name it without a prefix. Not a contract: it has no external
// effect and nothing to replace, so it carries no With* option and takes no
// seat in the list below.
type Origins = v1.Origins
```

- [ ] **Step 2: Change the interface in `v1/v1.go`**

`Origins() []*url.URL` becomes `Origins() Origins`. Keep the doc comment; append:

```go
	// The list is a value with an identity: Key names the tunnel these
	// origins are, which is what the spec cache files it under.
	Origins() Origins
```

- [ ] **Step 3: Run the build to see what breaks**

Run: `go build ./... 2>&1 | head -30`
Expected: FAIL, naming `v1alpha1/builder.go` and the test file. This is the list of call sites to fix — read it before editing.

- [ ] **Step 4: Rewrite `(*BuilderImpl).Origins()`**

Two edits inside it, at `v1alpha1/builder.go:897`:

The local named `origins` at line 939 collides with the new package import. Rename it to `urls`:

```go
	urls := make([]*url.URL, 0, len(settled))
```

…and every `origins = append(origins, …)` / `return origins` inside the function body becomes `urls`. Then the signature and the return:

```go
func (b *BuilderImpl) Origins() Origins {
	...
	return origins.New(origins.WithURL(urls...))
}
```

Add the import: `"github.com/tunnel-pizza/tunneld/v1alpha1/origins"`.

- [ ] **Step 5: Fix the call sites inside Run**

`v1alpha1/builder.go`, in order:

```go
	// :441
	origins := b.Origins()
	if origins.Len() == 0 {

	// :466 — adapter, removed in Task 3
	dialable, bound, err := b.binder.Bind(ctx, origins.URLs(), log)

	// :529 — adapter, removed in Task 3
	for _, ic := range b.display.Interceptors(b.multiview, origins.URLs(), log) {

	// :536
	log.Info("tunneld starting", "version", Version(), "libtunnel", libtunnel.Version(), "origins", origins.Len())

	// :632
	addresses := make([]string, origins.Len())
	for i := range addresses {
		addresses[i] = publicURL(public, i, origins.Len())
	}

	// :641 — adapter, removed in Task 3
	view := b.display.URL(b.multiview, public, origins.URLs())

	// :689 and :693, the report loops
	for _, origin := range origins.URLs() {
	for i, origin := range origins.URLs() {
		fmt.Fprintf(stdout, "%s\n", publicURL(public, i, origins.Len()))
```

`dialable` stays a `[]*url.URL` in this task; it is passed straight to `libtunnel`.

- [ ] **Step 6: Fix the tests**

In `v1alpha1/builder_test.go`, `originStrings` takes the new type:

```go
func originStrings(o Origins) []string {
	out := make([]string, 0, o.Len())
	for _, u := range o.URLs() {
		out = append(out, u.String())
	}
	return out
}
```

`builder_test.go` is `package v1alpha1`, so `Origins` is in scope unqualified. Then fix the direct users of the old slice — `b.Origins()` compared with `len()` or indexed, at `:1283`, `:1368` and `:1402` — by running the build and following it:

Run: `go vet ./... 2>&1 | head -30`

- [ ] **Step 7: Run the tests**

Run: `go test ./v1alpha1/... ./v1/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add v1/v1.go v1alpha1/v1alpha1.go v1alpha1/builder.go v1alpha1/builder_test.go
git commit   # refactor: the builder reports origins as a list that knows itself
```

---

### Task 3: The binder and the display take Origins

**Files:**
- Modify: `v1alpha1/v1alpha1.go` (the `Binder` and `Display` contracts)
- Modify: `v1alpha1/attach/attach.go:309` (`Bind`)
- Modify: `v1alpha1/display/display.go:178` and `:338`
- Modify: `v1alpha1/builder.go` (drop the three `.URLs()` adapters)
- Modify: `v1alpha1/attach/attach_test.go`, `v1alpha1/display/display_test.go`, `v1alpha1/builder_test.go` (fakes and call sites)

**Interfaces:**
- Consumes: `v1.Origins`, `origins.New`, `origins.WithURL` from Tasks 1-2.
- Produces: `Binder.Bind(ctx context.Context, shown Origins, log v1.Logger) (dialable Origins, bound attach.Bound, err error)`; `Display.URL(enabled bool, public *url.URL, origins Origins) string`; `Display.Interceptors(enabled bool, origins Origins, log v1.Logger) []libtunnel.Interceptor`.

- [ ] **Step 1: Write the failing test**

In `v1alpha1/attach/attach_test.go`, add to the binder's cases:

```go
// TestBindKeepsTheOriginsAList pins that what comes back out of Bind is the
// same kind of thing that went in: a run's dialable origins are origins, and
// the caller should not have to rebuild the type to hand them on.
func TestBindKeepsTheOriginsAList(t *testing.T) {
	b := New()
	shown := origins.New(origins.WithURL(mustURLs(t, "http://localhost:3000")...))

	dialable, bound, err := b.Bind(t.Context(), shown, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Bind() = %v", err)
	}
	defer bound.Close()

	if dialable.Len() != 1 {
		t.Fatalf("dialable.Len() = %d, want 1", dialable.Len())
	}
	if got, want := dialable.At(0).String(), "http://localhost:3000"; got != want {
		t.Errorf("dialable.At(0) = %q, want %q", got, want)
	}
}
```

`mustURLs(t, raw ...string) []*url.URL` is the helper that file already defines at `attach_test.go:1356`. `display_test.go` has `var discard = slog.New(slog.DiscardHandler)` for its own cases.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./v1alpha1/attach/ -run TestBindKeepsTheOriginsAList`
Expected: FAIL to compile — `cannot use shown (variable of type *origins.OriginsImpl) as []*url.URL`.

- [ ] **Step 3: Change `Bind`**

`v1alpha1/attach/attach.go:309`. The loop reads the list through the interface and builds its result through the package:

```go
func (b *BinderImpl) Bind(ctx context.Context, shown v1.Origins, log *slog.Logger) (v1.Origins, Bound, error) {
	var dialable []*url.URL
	var servers bound
	for at, origin := range shown.URLs() {
		...
	}
	...
	return origins.New(origins.WithURL(dialable...)), servers, nil
}
```

Every `return nil, nil, …` error path stays as it is. Import `"github.com/tunnel-pizza/tunneld/v1alpha1/origins"`.

Note `at` is still the index the announce order depends on; `URLs()` preserves order, so nothing about routing changes.

- [ ] **Step 4: Change the display**

`v1alpha1/display/display.go`, both methods, where only the count is read:

```go
func (*DisplayImpl) Interceptors(enabled bool, origins v1.Origins, log v1.Logger) []libtunnel.Interceptor {
	if !enabled || origins.Len() < 2 {
		return nil
	}

func (*DisplayImpl) URL(enabled bool, public *url.URL, origins v1.Origins) string {
	if !enabled || origins.Len() < 2 {
		return ""
	}
```

If either body reads the origins further down, replace `origins[i]` with `origins.At(i)` and `range origins` with `range origins.URLs()`.

- [ ] **Step 5: Change the contracts**

`v1alpha1/v1alpha1.go`:

```go
type Display interface {
	URL(enabled bool, public *url.URL, origins Origins) string
	Interceptors(enabled bool, origins Origins, log v1.Logger) []libtunnel.Interceptor
	Open(ctx context.Context, log v1.Logger, opts ...display.Option)
}

type Binder interface {
	Bind(ctx context.Context, shown Origins, log v1.Logger) (dialable Origins, bound attach.Bound, err error)
}
```

- [ ] **Step 6: Drop the adapters in the builder**

In `v1alpha1/builder.go`, the three `.URLs()` calls added in Task 2 become plain `origins` again:

```go
	dialable, bound, err := b.binder.Bind(ctx, origins, log)
	for _, ic := range b.display.Interceptors(b.multiview, origins, log) {
	view := b.display.URL(b.multiview, public, origins)
```

`dialable` is now an `Origins`; whatever passes it to libtunnel takes `dialable.URLs()`.

- [ ] **Step 7: Fix the fakes**

Every fake binder and fake display in `v1alpha1/builder_test.go` (and any in `v1alpha1/v1alpha1_test.go`) takes the new types. Build until quiet:

Run: `go vet ./... 2>&1 | head -30`

- [ ] **Step 8: Run the tests**

Run: `make check`
Expected: PASS, every package.

- [ ] **Step 9: Commit**

```bash
git add v1alpha1/
git commit   # refactor: the binder and the panel take the origins, not a slice
```

---

### Task 4: The build banner names the cache key

**Files:**
- Modify: `v1alpha1/version.go:76`
- Modify: `v1alpha1/builder.go:377`, `:545`
- Modify: `v1alpha1/v1alpha1.go:256` (`attach.WithBanner`)
- Modify: `v1alpha1/version_test.go`

**Interfaces:**
- Consumes: `Origins` from Tasks 1-2.
- Produces: `VersionLine(origins Origins) string`.

- [ ] **Step 1: Write the failing test**

Add to `v1alpha1/version_test.go`:

```go
// TestVersionLineNamesTheCache pins the third number a bug report needs. Which
// spec a run replayed is the question behind every report of a hostname that
// changed when it should not have, and the key is the whole answer.
func TestVersionLineNamesTheCache(t *testing.T) {
	u, err := url.Parse("http://localhost:3000")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	o := origins.New(origins.WithDir("/work/project"), origins.WithURL(u))

	line := VersionLine(o)
	if !strings.Contains(line, "cache "+o.Key()) {
		t.Errorf("VersionLine() = %q, want the cache key %q in it", line, o.Key())
	}
	if !strings.Contains(line, Version()) {
		t.Errorf("VersionLine() = %q, want the version in it", line)
	}

	// Nothing to name before a run has origins — the frame's banner is built
	// in New, before a flag has been parsed.
	for _, empty := range []Origins{nil, origins.New(origins.WithDir("/work/project"))} {
		if got := VersionLine(empty); strings.Contains(got, "cache ") {
			t.Errorf("VersionLine(%v) = %q, want no cache clause", empty, got)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./v1alpha1/ -run TestVersionLineNamesTheCache`
Expected: FAIL — too many arguments to `VersionLine`.

- [ ] **Step 3: Change `VersionLine`**

`v1alpha1/version.go`:

```go
// VersionLine is the human-facing build banner printed by `tunneld version`
// and logged at startup. It names libtunnel too, since that is what actually
// speaks to the edge — a bug report needs both numbers.
//
// And the cache key, when there is a run to name: it is what says which spec
// this run replays, which is the question behind a hostname that changed when
// it should not have. Origins with nothing in them, or none at all, drop the
// clause — that is the frame's banner, built in New before a flag has been
// parsed, where there is no run to identify yet.
func VersionLine(origins Origins) string {
	line := fmt.Sprintf("tunneld %s (libtunnel %s, built %s", Version(), libtunnel.Version(), runtime.Version())
	if origins != nil && origins.Len() > 0 {
		line += ", cache " + origins.Key()
	}
	return line + ")"
}
```

- [ ] **Step 4: Fix the three callers**

```go
	// v1alpha1/builder.go:377, the version subcommand
	fmt.Fprintln(cmd.OutOrStdout(), VersionLine(b.Origins()))

	// v1alpha1/builder.go:545, the startup banner — origins is in scope
	fmt.Fprintln(stderr, VersionLine(origins))

	// v1alpha1/v1alpha1.go:256, built in New, before any flag
	attach.WithBanner(VersionLine(nil)),
```

- [ ] **Step 5: Run the tests**

Run: `go test ./v1alpha1/...`
Expected: PASS. If a case asserts the banner's exact text, update it to the new shape rather than loosening it.

- [ ] **Step 6: Commit**

```bash
git add v1alpha1/version.go v1alpha1/version_test.go v1alpha1/builder.go v1alpha1/v1alpha1.go
git commit   # feat: the banner names the cache this run replays
```

---

### Task 5: The cache is one directory and a key

**Files:**
- Modify: `v1alpha1/cache/cache.go` (substantially rewritten)
- Modify: `v1alpha1/cache/cache_test.go`
- Modify: `v1alpha1/v1alpha1.go` (the `Cache` contract)
- Modify: `v1alpha1/builder.go:473`, `:616`, `:738` (drop the dirs argument; the guards still read `cacheDirs` in this task)

**Interfaces:**
- Consumes: `v1.Origins` from Task 1.
- Produces: `Cache` with `Load(origins Origins, log v1.Logger) string`, `Save(origins Origins, log v1.Logger)`, `Discard(origins Origins, log v1.Logger)`; `cache.New(opts ...cache.Option) *cache.CacheImpl`; `cache.WithDir(dir string) cache.Option`.

- [ ] **Step 1: Write the failing test**

`v1alpha1/cache/cache_test.go` is `package cache_test` — the outside-the-package view, deliberately, so it can only see `New`, the options and the three methods. Keep it that way: nothing below reaches for an unexported helper.

Two fixtures change. `write` no longer knows a constant filename, and a new one builds a matching cache and origins:

```go
// write puts a cache file in dir under the name key gives it.
func write(t *testing.T, dir, key, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, key+".env"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// fixed is a cache in a directory only this case can see, an origins value
// with a key that does not depend on where the test process is running, and
// the path the two of them imply.
func fixed(t *testing.T, raw ...string) (*cache.CacheImpl, v1.Origins, string) {
	t.Helper()
	dir := t.TempDir()

	opts := []origins.Option{origins.WithDir("/work/project")}
	for _, r := range raw {
		u, err := url.Parse(r)
		if err != nil {
			t.Fatalf("parsing %q: %v", r, err)
		}
		opts = append(opts, origins.WithURL(u))
	}
	o := origins.New(opts...)

	return cache.New(cache.WithDir(dir)), o, filepath.Join(dir, o.Key()+".env")
}
```

Every existing case — `TestRoundTrip`, `TestSave`, `TestLoad`, `TestDiscard` — keeps its assertions and swaps its setup onto `fixed`, passing the origins where it used to pass a `[]string` of directories, and looking for the file at the third return value rather than at `filepath.Join(dir, cache.File)`.

Then the case this whole change exists for:

```go
// TestTheFileIsNamedForTheRun pins #115: a run reads back its own spec and
// never another run's. Before this, one directory held one TUNNEL.env, so the
// second run in a project replayed the first one's hostname while serving
// something else entirely.
func TestTheFileIsNamedForTheRun(t *testing.T) {
	t.Setenv(ltv1.SpecEnv, envelope)
	t.Setenv(ltv1.HostnameEnv, "brave-otter.tunneled.pizza")

	c, three, path := fixed(t, "http://localhost:3000")
	c.Save(three, discard())
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("after Save, %s: %v", path, err)
	}

	// The same project, serving something else: a different tunnel, so
	// nothing to resume.
	other := origins.New(
		origins.WithDir("/work/project"),
		origins.WithURL(mustParse(t, "http://localhost:4000")),
	)
	if got := c.Load(other, discard()); got != "" {
		t.Errorf("Load(other origins) = %q, want nothing — that is another run's tunnel", got)
	}

	// And the run that saved it finds its own.
	if got := c.Load(three, discard()); got != envelope {
		t.Errorf("Load(same origins) = %q, want the spec it saved", got)
	}
}

// TestTheDefaultDirectoryIsUnderTheUsersCache pins where a spec lands when
// nothing says otherwise: never the working directory, which is usually a
// checkout, and a spec is credentials.
func TestTheDefaultDirectoryIsUnderTheUsersCache(t *testing.T) {
	base := t.TempDir()
	// The three variables os.UserCacheDir reads, one per platform: XDG on
	// Linux, HOME on macOS (<HOME>/Library/Caches), LocalAppData on Windows.
	t.Setenv("XDG_CACHE_HOME", base)
	t.Setenv("HOME", base)
	t.Setenv("LocalAppData", base)
	t.Setenv(ltv1.SpecEnv, envelope)

	want, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache directory: %v", err)
	}

	o := origins.New(origins.WithDir("/work/project"), origins.WithURL(mustParse(t, "http://localhost:3000")))
	cache.New().Save(o, discard())

	path := filepath.Join(want, ".tunneld", o.Key()+".env")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("want the spec at %s: %v", path, err)
	}
	// A spec is the credential for a public hostname.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("%s is mode %v, want -rw-------", path, info.Mode().Perm())
	}
}
```

`mustParse(t, raw)` is a two-line helper this file needs and does not have yet — add it beside `write`. `discard` and `envelope` are already there. The imports grow by `net/url`, `v1` and `v1alpha1/origins`.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./v1alpha1/cache/`
Expected: FAIL — undefined `WithDir`, `ext`, `path`.

- [ ] **Step 3: Rewrite the package's head**

`v1alpha1/cache/cache.go`. Delete `const File = "TUNNEL.env"` and put in its place:

```go
// ext is what a cache file is called after its key. The contents are a file of
// environment variables, so the extension says what it is to anyone who opens
// the directory.
const ext = ".env"

// dirName is the one directory every cached spec lives in, under the user's
// own cache directory. Flat: what identifies a tunnel is in the filename, so
// nesting would only add a level that says the same thing twice.
const dirName = ".tunneld"

// Option configures a CacheImpl at construction.
type Option = v1.Option[*CacheImpl]

// CacheImpl is the default cache: one file per tunnel, named for it, under
// the user's cache directory.
type CacheImpl struct {
	// dir is where files go. Empty means this machine has no cache directory
	// and nothing is cached, which is the same answer an unwritable one gives.
	dir string
}

// New returns a CacheImpl configured by opts, pointed at the user's cache
// directory.
//
// Not the working directory, which is what this used to allow. A spec is
// credentials, the working directory is usually a repository, and no filename
// avoids being committed there: measured against GitHub's 239 gitignore
// templates and 752 real ones, the best a name managed was 13% and 26%.
func New(opts ...Option) *CacheImpl {
	c := &CacheImpl{}
	if base, err := os.UserCacheDir(); err == nil {
		c.dir = filepath.Join(base, dirName)
	}
	return v1.Apply(c, opts...)
}

// WithDir replaces the directory specs are cached in — a mounted volume in a
// container, a temporary directory in a test. An empty directory turns caching
// off, which is what a machine with no cache directory already gets.
func WithDir(dir string) Option {
	return func(c *CacheImpl) { c.dir = dir }
}

// path is where this run's spec lives: the key, which names the tunnel, under
// the directory, which names nothing. This package hashes nothing and decides
// nothing about identity — that is the origins' answer, and this joins it to a
// directory.
func (c *CacheImpl) path(origins v1.Origins) string {
	if c.dir == "" || origins == nil {
		return ""
	}
	return filepath.Join(c.dir, origins.Key()+ext)
}
```

- [ ] **Step 4: Rewrite the three methods**

Keep every existing doc comment's reasoning; the loops go. `Load`:

```go
func (c *CacheImpl) Load(origins v1.Origins, log v1.Logger) string {
	path := c.path(origins)
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}

	v := viper.New()
	v.SetConfigType("env")
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		log.Warn("could not read the tunnel cache", "path", path, "error", err)
		return ""
	}

	// viper lower-cases the keys it parses.
	spec := v.GetString(strings.ToLower(ltv1.SpecEnv))
	if spec == "" {
		log.Warn("the tunnel cache names no spec", "path", path)
		return ""
	}
	log.Info("resuming a tunnel from cache", "path", path)
	return spec
}
```

`Discard`:

```go
func (c *CacheImpl) Discard(origins v1.Origins, log v1.Logger) {
	path := c.path(origins)
	if path == "" {
		return
	}
	if err := os.Remove(path); err == nil {
		log.Info("discarded a dead tunnel cache", "path", path)
	} else if !os.IsNotExist(err) {
		log.Warn("could not discard the tunnel cache", "path", path, "error", err)
	}
}
```

`Save` keeps the body that builds `lines` and `body` unchanged, and ends:

```go
	path := c.path(origins)
	if path == "" {
		log.Debug("nothing to cache into: no cache directory")
		return
	}
	// The directory does not exist until the first save, so creating it is
	// part of saving rather than something the caller was asked to arrange.
	// 0700: it holds credentials, and a directory somebody else can list is a
	// directory that has already leaked which projects are on this machine.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Warn("could not make the cache directory", "dir", filepath.Dir(path), "error", err)
		return
	}
	// 0600: a spec is the credential for a public hostname.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		log.Warn("could not cache the tunnel", "path", path, "error", err)
		return
	}
	log.Info("cached the tunnel", "path", path)
```

Update the package doc comment's last paragraph: the directories no longer arrive from `--cache-dir`, they are the user's cache directory or whatever `WithDir` says.

- [ ] **Step 5: Change the contract and the call sites**

`v1alpha1/v1alpha1.go`:

```go
// Cache persists a tunnel's spec between runs, under the name the origins
// give it.
type Cache interface {
	Load(origins Origins, log v1.Logger) string
	Save(origins Origins, log v1.Logger)
	Discard(origins Origins, log v1.Logger)
}
```

`v1alpha1/builder.go` — the guards are still the old ones in this task, only the arguments change:

```go
	// :473
	if len(b.cacheDirs.GetSlice()) > 0 {
		cached = b.cache.Load(origins, log)
	}
	// :616
	b.cache.Discard(origins, log)
	// :738
	if len(b.cacheDirs.GetSlice()) > 0 {
		b.cache.Save(origins, log)
	}
```

- [ ] **Step 6: Run the tests**

Run: `make check`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add v1alpha1/cache/ v1alpha1/v1alpha1.go v1alpha1/builder.go
git commit   # feat: a cached spec is filed under the run it belongs to
```

---

### Task 6: One knob, and the list machinery goes

**Files:**
- Modify: `v1/v1.go` (delete `CacheDirEnv`, add `NoCacheEnv`, fix the `--cache-dir=false` mention at :172)
- Modify: `v1alpha1/v1alpha1.go` (delete `CacheDirs`/`WithCacheDirs`, add `WithCacheDir`, `WithCache` accepts nil, drop the `cachedir` import/default/assertion, drop the `cacheDirs` field)
- Modify: `v1alpha1/builder.go` (`--no-cache` flag, `flagEnv`, `WithCacheDir`, the three guards)
- Modify: `v1alpha1/builder_test.go`
- Delete: `v1alpha1/cachedir/cachedir.go`, `v1alpha1/cachedir/cachedir_test.go`

**Interfaces:**
- Consumes: `cache.New`, `cache.WithDir` from Task 5.
- Produces: `WithCache(c Cache) Option` (nil turns caching off); `WithCacheDir(dir string) Option`; `v1.NoCacheEnv = "TUNNELD_NO_CACHE"`; the `--no-cache` flag.

- [ ] **Step 1: Write the failing test**

`newRunHarness` ends its runs by cancelling the context from `h.cache.onSave` — which never fires when caching is off, so a case that reused that signal would hang until the establish deadline. Give `fakeBinder` a hook that fires in every run instead. `Announce` happens once the tunnel is live and before the save, so it is the signal both halves of this case can share:

```go
// fakeBinder.Announce, at builder_test.go:766
func (f *fakeBinder) Announce(public []string) {
	f.announced = public
	if f.onAnnounce != nil {
		f.onAnnounce()
	}
}
```

with `onAnnounce func()` added to the struct beside `announced`.

Then the case, in `v1alpha1/builder_test.go` beside `TestRun`:

```go
// TestCachingIsOnUnlessItIsTurnedOff pins the switch and everything that can
// throw it, and pins that throwing it leaves the builder alone: an embedder
// that runs a command twice gets its cache back on the second run.
func TestCachingIsOnUnlessItIsTurnedOff(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"

	for _, tc := range []struct {
		name  string
		args  []string
		env   string
		off   func(b *BuilderImpl)
		saved bool
	}{
		{name: "by default", saved: true},
		{name: "--no-cache", args: []string{"--no-cache"}},
		{name: "the variable", env: "true"},
		{name: "WithCache(nil)", off: func(b *BuilderImpl) { v1.Apply(b, WithCache(nil)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunHarness(t, live(public), ":3000")
			if tc.off != nil {
				tc.off(h.b)
			}
			ctx, cancel := context.WithCancel(t.Context())
			h.binder.onAnnounce = cancel

			if tc.env != "" {
				t.Setenv(v1.NoCacheEnv, tc.env)
			}
			if err := h.run(t, ctx, tc.args...); err != nil {
				t.Fatalf("run() = %v, want nil after a signal", err)
			}
			if h.cache.saved != tc.saved {
				t.Errorf("cache saved = %v, want %v", h.cache.saved, tc.saved)
			}
		})
	}
}
```

Two edits to the harness make that case mean something:

- `run` blanks every mirror it knows about at `builder_test.go:837`, and a row that sets `NoCacheEnv` would have it blanked back out. Delete `v1.CacheDirEnv` from that list and do **not** add `NoCacheEnv` to it. Blank it once in `newRunHarness` instead — `t.Setenv(v1.NoCacheEnv, "")`, beside the `v1.LogEnv` line that already exists so a developer's shell cannot reach in — which a case's own later `t.Setenv` then overrides.
- `newRunHarness` passes `WithCacheDir(t.TempDir())` with the comment "run consults the cache only with a directory". That is no longer true and the option now builds a real cache that `WithCache(h.cache)` immediately replaces. Delete the line.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./v1alpha1/ -run TestNoCacheStopsTheCache`
Expected: FAIL — undefined `v1.NoCacheEnv`, and `WithCache(nil)` compiles but caches anyway.

- [ ] **Step 3: Change the constants in `v1/v1.go`**

Delete `CacheDirEnv` and its doc comment. In its place:

```go
	// NoCacheEnv turns the spec cache off — the mirror of --no-cache, which
	// beats it. Anything strconv.ParseBool reads as true turns it off; unset
	// or false leaves it on, which is the default.
	//
	// A run with no cache mints a fresh hostname every time, which is what
	// somebody wants when the hostname must not be reused.
	NoCacheEnv = "TUNNELD_NO_CACHE"
```

At `v1/v1.go:172`, `--cache-dir=false and a restart` becomes `--no-cache and a restart`.

- [ ] **Step 4: Change the options in `v1alpha1/v1alpha1.go`**

Delete the `CacheDirs` interface and `WithCacheDirs`. Replace `WithCache`'s doc and add the wrapper:

```go
// WithCache replaces where a tunnel's spec is kept between runs. The default
// is cache.New(), one file per tunnel under the user's cache directory.
//
// nil turns caching off, and is the same state --no-cache leaves the run in:
// no caching is no cache, rather than a flag that some other field has to be
// read against. It is what makes "off but with an implementation configured"
// impossible to be in.
func WithCache(c Cache) Option {
	return func(b *BuilderImpl) { b.cache = c }
}

// WithCacheDir caches specs in dir rather than under the user's cache
// directory — a mounted volume in a container, a temporary directory in a
// test.
//
// A wrapper rather than a field of its own, because the directory belongs to
// the cache and New has already built one by the time a caller's options run:
// the way to change where a spec lands is to hand over a cache that puts it
// there. An embedder wanting more than the directory reaches the same lever
// directly, with cache.New and its own options.
func WithCacheDir(dir string) Option {
	return WithCache(cache.New(cache.WithDir(dir)))
}
```

Then: remove `cachedir` from the imports, remove `WithCacheDirs(cachedir.New())` from `New`'s defaults, remove `_ CacheDirs = (*cachedir.ValueImpl)(nil)` from the assertions, and remove the `cacheDirs CacheDirs` field from `BuilderImpl`.

- [ ] **Step 5: Change the flag and the guards in `v1alpha1/builder.go`**

Replace the `--cache-dir` registration at `:348`:

```go
		cmd.Flags().BoolVar(&b.noCache, "no-cache", b.noCache,
			"Don't cache the tunnel spec: mint a fresh hostname every run")
```

Add `noCache bool` to `BuilderImpl`, and in `flagEnv` replace `"cache-dir": v1.CacheDirEnv` with `"no-cache": v1.NoCacheEnv`.

In `Run`, above the first use:

```go
	// The cache this run uses, or none. A local rather than the field, so a
	// run started with --no-cache does not leave an embedder's builder without
	// a cache for the next one.
	spec := b.cache
	if b.noCache {
		spec = nil
	}
```

and the three sites become:

```go
	cached := ""
	if spec != nil {
		cached = spec.Load(origins, log)
	}
	...
	if spec != nil {
		spec.Discard(origins, log)
	}
	...
	if spec != nil {
		spec.Save(origins, log)
	}
```

- [ ] **Step 6: Delete the package**

```bash
git rm -r v1alpha1/cachedir
```

- [ ] **Step 7: Run the tests**

Run: `go build ./... && go test ./...`
Expected: PASS. Any test that set `--cache-dir` or `TUNNELD_CACHE_DIR` now uses `WithCacheDir(t.TempDir())` or `--no-cache`.

- [ ] **Step 8: Run the full gate**

Run: `make check && make race`
Expected: PASS both.

- [ ] **Step 9: Commit**

```bash
git add -A
git commit   # feat: caching is on or off, and the directory is the cache's own business
```

---

### Task 7: The fallout

**Files:**
- Modify: `e2e/e2e_test.go:509`
- Modify: `examples/docker-compose/docker-compose.yml:17` and its volume
- Modify: `Makefile:138-148`
- Modify: `.gitignore:41-45`
- Modify: `README.md:564`, `:657`, `:671`, `:705`, `:779`
- Modify: `CONTRIBUTING.md:19`, `:79`, `:278`, `:894`
- Modify: `CLAUDE.md:12`, `:44-46`

**Interfaces:**
- Consumes: everything above. Produces nothing new.

- [ ] **Step 1: e2e**

`e2e/e2e_test.go:509`:

```go
	r.cmd.Env = append(strippedEnv(), v1.NoCacheEnv+"=true")
```

and rewrite the comment above `start` — the child no longer gets a cache directory of its own, it caches nothing at all, which is the same protection by a shorter route: a live run must not read the developer's real tunnel cache, and must not leave its own tunnel in it.

- [ ] **Step 2: The compose example**

`examples/docker-compose/docker-compose.yml`: `TUNNELD_CACHE_DIR=/var/run/tunneld` becomes `XDG_CACHE_HOME=/var/run`, and the named volume moves from `/var/run/tunneld` to `/var/run/.tunneld`. Update the comment that explains the volume: the path is where `os.UserCacheDir` lands plus `.tunneld`, and the container's own working directory is what scopes the key inside it.

- [ ] **Step 3: The Makefile**

`clean` drops `TUNNEL.env` from its `rm -f` line, and the cache line becomes:

```make
	rm -rf "$$HOME/Library/Caches/.tunneld" "$${XDG_CACHE_HOME:-$$HOME/.cache}/.tunneld"
```

Rewrite the comment above it: every cached tunnel on the machine is removed, not this project's, because the working directory is inside the filename's hash and nothing in a flat directory says which project a file came from.

- [ ] **Step 4: `.gitignore`**

Delete the `TUNNEL.env` entry and the three comment lines above it. Nothing writes a spec into a checkout any more: there is no flag that can point one here.

- [ ] **Step 5: The README**

- `:564` — replace the `--cache-dir` row with a `--no-cache` / `TUNNELD_NO_CACHE` row: don't cache the tunnel spec; mint a fresh hostname every run. Say where the cache is when it is on: `<user cache dir>/.tunneld/<key>.env`, one file per working directory and set of origins.
- `:657` — `func WithCacheDir(dir string) Option // cache specs here instead of the user's cache directory`.
- `:671` — the `WithCacheDirs` mention goes.
- `:705` — `CacheDirEnv` becomes `NoCacheEnv`.
- `:779` — the environment table row, matching :564.

- [ ] **Step 6: CONTRIBUTING**

- `:19` — the `v1alpha1/cachedir` row becomes `Origins, their key, and the options that build them | v1alpha1/origins/`.
- `:79` — the contract list loses `CacheDirs`; the sentence naming eight becomes seven.
- `:278` — the flag/env pairing example names `--no-cache` rather than `--cache-dir`.
- `:894` — the `TUNNEL.env` paragraph is replaced by one saying a spec never lands in the tree: the cache is the user's cache directory or whatever an embedder passed `WithCacheDir`, and no flag can point it at a checkout.

- [ ] **Step 7: CLAUDE.md**

- `:12` — "the eight internal contracts" becomes seven.
- `:44-46` — the secrets note: `.gitignore` no longer lists `TUNNEL.env`; a cached spec is credentials and lives under the user's cache directory, named for the run.

- [ ] **Step 8: Grep for stragglers**

Run:
```bash
grep -rn 'TUNNEL\.env\|cache-dir\|CacheDir\|cachedir\|CACHE_DIR' \
  --include='*.go' --include='*.md' --include='*.yml' --include='Makefile' \
  --include='.gitignore' . | grep -v docs/superpowers
```
Expected: only `WithCacheDir` in `v1alpha1/v1alpha1.go`, the README and CONTRIBUTING.

- [ ] **Step 9: Run everything**

Run: `make check && make race && make e2e`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add -A
git commit   # docs: the spec cache is the user's cache directory, keyed by the run
```

---

## Verification before handing back

- [ ] `make check`, `make race`, `make e2e` green.
- [ ] `go doc ./v1 Origins` and `go doc ./v1alpha1 Cache` read the way the spec says.
- [ ] A live run writes one file: `go run . :3000`, then find it under the user's cache directory, confirm the name matches the key the banner printed, and confirm mode `-rw-------`.
- [ ] A second live run in the same checkout with a different origin writes a *second* file and mints its own hostname — the bug in #115, gone.
- [ ] `go run . --no-cache :3000` writes nothing.
- [ ] Do not push. Report, and wait to be told.
