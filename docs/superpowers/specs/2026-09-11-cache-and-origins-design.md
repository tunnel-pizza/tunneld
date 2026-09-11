# Spec cache by origins, and origins as a type

**Issue:** [#115](https://github.com/tunnel-pizza/tunneld/issues/115)
**Date:** 2026-09-11

## The problem

A tunnel's spec is cached under one filename, `TUNNEL.env`, in every directory
`--cache-dir` settled on. One name means one spec per directory, whatever the
run was serving — and the default directory is per-project, so two runs started
from the same project share it by construction.

The failure is not a stale read. `Load` hands the spec to `libtunnel.From`,
which replays that tunnel's identity, so a second run comes up on the first
run's hostname while serving something else. The file is well-formed, the spec
is valid, and the hostname it names is the one it was minted for. It is only
the wrong one.

It fails in the other direction too. Two tunnels somebody keeps around — one
serving `:3000`, one serving `exec:///usr/bin/htop` — cannot both hold a stable
hostname from the same project, because whichever ran last owns the file.

The fix is to name the file after what it belongs to. That needs the origins at
every cache call site, which is the second half of this spec: the origins stop
being a bare `[]*url.URL` passed around and become a type that can answer for
itself.

## Decisions

| | Before | After |
|---|---|---|
| Cache directory | `--cache-dir` list, each entry a path or `true`/`false` | `<os.UserCacheDir()>/.tunneld/<sha256(cwd)[:16]>`, computed |
| Cache file | `TUNNEL.env` | `<sha256(origins)[:16]>.env` |
| Directory override | `--cache-dir`, `TUNNELD_CACHE_DIR`, `WithCacheDir(dirs ...string)` | `WithCacheDir(dir string)` — a wrapper over `WithCache`, no flag, no variable |
| Off switch | `--cache-dir=false` (a value that is an instruction) | `--no-cache`, `TUNNELD_NO_CACHE`, `WithCache(nil)` |
| Origins | `[]*url.URL` in five signatures | `v1.Origins`, implemented in `v1alpha1/origins` |
| Build banner | `VersionLine()` | `VersionLine(origins Origins)` — prints the cache key |
| Contracts | eight, including `CacheDirs` | seven — `CacheDirs` deleted |

No backwards compatibility. Every cached tunnel is orphaned by the rename and
mints once more; the project is 0.0.x and the alternative is carrying a
migration for a file whose whole purpose is to be disposable.

## On disk

```
<os.UserCacheDir()>/.tunneld/<sha256(cwd + origins)[:16]>.env
```

One flat directory, one file per tunnel. What identifies a tunnel is the
working directory it was started from together with what it serves — two
projects on one machine are two tunnels even when they serve the same port,
and one project serving two things is two tunnels as well. Both go into one
hash rather than one nesting inside the other.

The name is `origins.Key()`: hex SHA-256 over the absolute working directory
and then each origin's `URL.String()` **sorted and deduplicated**, every part
joined with `\n`, first 16 characters — the width the current default directory
already uses:

- **Sorted**, so `tunneld a b` and `tunneld b a` are one tunnel. The order still
  matters to the run — it is what the `?n` routing parameters index — but it
  does not make a second tunnel.
- **Deduplicated**, so a repeated origin does not make a third name.
- **Settled URLs rather than typed strings**, so `:3000`,
  `localhost:3000` and `http://localhost:3000` are one origin with one cache
  file. This is what makes the name mean "which tunnel" rather than "what did
  somebody type this time".
- **Newline-joined**, which is what keeps the parts from running together: a
  directory and an origin concatenated raw could be split two ways and collide.

Directory mode `0700` and file mode `0600`, unchanged: a spec is the credential
for a public hostname, and a directory somebody else can list has already
leaked which projects are on this machine.

Nothing on disk says which project a file belongs to, which is the cost of one
flat directory: a name that reveals the working directory would defeat the
hashing. Tooling that wants to remove one project's tunnels cannot, which is
why `make clean` wipes the directory whole.

`os.UserCacheDir()` failing means no caching, which is what an unusable
directory already means today.

The names are not decodable. That is the trade against base64, which is
reversible but unbounded: filenames cap at 255 bytes, and five `exec:///`
origins or a deep checkout path overflow it. A fixed-width hash never does.

## Origins

The interface is declared in `v1/v1.go`, not `v1alpha1.go`, because
`v1.Builder.Origins()` returns it and `v1` cannot import `v1alpha1` without an
import cycle. `v1alpha1.go` carries `type Origins = v1.Origins` next to its
`Option` alias, so the package can name it without a `v1.` prefix.

It is a type, not a contract, and it does not join the list in `v1alpha1.go`.
The file's own rule is that a contract is something with an external effect,
seeded by `New` and replaceable with a matching `With*` option — the edge, the
disk, the daemon, the browser. `Origins` is a value the builder computes from
argv, the environment and its seeds, with nothing to replace and nowhere to
reach. Declaring it beside `Engine` and `Cache` would say it is one of them.
The contract count therefore goes from eight to seven, which
[`CLAUDE.md:12`](../../../CLAUDE.md) states in words.

```go
// v1/v1.go
type Origins interface {
	Len() int
	At(i int) *url.URL
	URLs() []*url.URL
	Key() string
}
```

`Key` is this run's identity, sixteen hex characters of SHA-256 over the
working directory and then each origin's `URL.String()`, sorted, deduplicated,
every part newline-joined. It is the cache file's stem, and it is what the
build banner prints.

The working directory is in it because two projects on one machine are two
tunnels even when both serve `:3000`. That makes `Key` the whole identity
rather than half of one: the cache hashes nothing and joins a path, the banner
prints the same string that is on disk, and there is one sixteen-character
value in the system rather than two that look alike.

Implemented once, in `v1alpha1/origins`, constructed the way every other
implementation in this repo is:

```go
type Option = v1.Option[*OriginsImpl]

func New(opts ...Option) *OriginsImpl
func WithURL(url ...*url.URL) Option
func WithDir(dir string) Option
```

`WithURL` appends, repeating the option across calls the way `WithOrigin` does
on the builder and `WithProviders` does on the identity — so a list can be
assembled from more than one source without the caller concatenating first. A
repeat that lands the same origin twice costs nothing: `Key` deduplicates, and
the run exposes what it was given.

`New` seeds the directory from `os.Getwd` and `WithDir` replaces it, which is
what lets a test fix the key without chdir-ing the process — the same shape
`github.New` and `WithTimeout` already have. A `Getwd` that fails leaves it
empty, and an empty directory still hashes: the key is then the origins alone,
which is a worse identity but not a broken one.

`OriginsImpl` is immutable after construction. `URLs` returns a copy, so a
consumer cannot reorder the run's origins by sorting the slice it was handed —
which is a live hazard the moment `Key` sorts.

There is no `WithOrigins`. An option handing in a prebuilt list would be a
second seeding path beside `WithOrigin(...string)`, and two answers to "what is
this run exposing" is the thing `Origins` exists to stop.

Signatures that stop taking `[]*url.URL`:

| Where | After |
|---|---|
| `v1.Builder` | `Origins() Origins` |
| `v1alpha1.Binder` | `Bind(ctx, shown Origins, log) (dialable Origins, bound attach.Bound, err error)` |
| `v1alpha1.Display` | `URL(enabled bool, public *url.URL, origins Origins) string` |
| `v1alpha1.Display` | `Interceptors(enabled bool, origins Origins, log v1.Logger) []libtunnel.Interceptor` |
| `v1alpha1.Cache` | below |

`attach` already imports `v1`, so it gains no dependency.

## The Cache contract

```go
// v1alpha1/v1alpha1.go
type Cache interface {
	Load(origins Origins, log v1.Logger) string
	Save(origins Origins, log v1.Logger)
	Discard(origins Origins, log v1.Logger)
}
```

The origins and nothing else. Where the file goes is the cache's own business,
settled when it is built:

```go
// v1alpha1/cache
func New(opts ...Option) *CacheImpl
func WithDir(dir string) Option
```

A directory cannot be a builder option in its own right: `New` applies its
defaults — `cache.New()` among them — before it applies the caller's options,
so an option that set a directory on the builder would be configuring a cache
that had already been constructed. It has to build a new one, which is what
`WithCacheDir` does:

```go
// v1alpha1
func WithCache(c Cache) Option
func WithCacheDir(dir string) Option {
	return WithCache(cache.New(cache.WithDir(dir)))
}
```

So `WithCacheDir` keeps its name and its job, and is a wrapper over the
contract option rather than a second field to keep in step with it. An embedder
wanting more than a directory reaches the same lever directly:
`WithCache(cache.New(cache.WithDir(d), cache.WithSomethingElse()))`.

Unset means `<os.UserCacheDir()>/.tunneld`, computed once at construction.

The filename is `origins.Key() + ".env"`. The cache hashes nothing and decides
nothing about identity — it joins a directory to a name it was given, which is
the whole of what "where does this spec live" means once the key exists.

One directory, so `Save` writes one file. The old asymmetry — `Load` reads the
first directory and `Save` writes all of them — was there because a list could
name a volume that may or may not be mounted. A single directory has no such
question, and the two halves become symmetric.

`cache.File` is deleted. Nothing outside the package needs the name now that it
is derived.

## The build banner

```go
func VersionLine(origins Origins) string
```

It already names tunneld and libtunnel because a bug report needs both numbers.
It now names the cache key too, for the same reason: "which spec is this run
replaying" is the question behind every report of a hostname that changed when
it should not have, or did not change when it should have, and the answer was
previously only derivable by hashing things by hand.

```
tunneld v0.0.45 (libtunnel v0.1.1, built go1.26.0, cache 9f2b7c1e4a8d0356)
```

`nil` origins drop the clause, which is what `attach.WithBanner` passes: that
banner is built in `New`, before a flag has been parsed, so there is no run to
identify yet. The `version` subcommand and the startup banner both pass
`b.Origins()` — the same origins the run would expose, settled the same way.
Settling them there costs a `$SHELL` resolution and can log that it is falling
back to a shell, at a level a default run does not print.

## The off switch

`--no-cache`, mirrored by `TUNNELD_NO_CACHE` in `flagEnv` like every other flag.

There is no separate option for it, because there is no separate state: no
caching is no cache, spelled `WithCache(nil)`. An embedder turning it off and a
run started with `--no-cache` arrive at the same nil field rather than at a
bool that some other field then has to be read against, and "off but with an
implementation configured" never becomes a state anybody can be in.

The run reads the flag once and the three call sites guard on the result:

```go
spec := b.cache
if b.noCache {
	spec = nil
}
...
if spec != nil { cached = spec.Load(origins, log) }
```

A local rather than a field, so the flag does not reach back and mutate the
builder — a builder whose cache disappeared because a command ran once would
be a surprise to an embedder calling Run twice. The three guards replace the
three `if len(b.cacheDirs.GetSlice()) > 0` at `builder.go:473`,
`builder.go:616` and `builder.go:738`.

Removing the boolean-as-value knob is most of why `v1alpha1/cachedir` can go:
`true`, `false`, empty-means-true, false-poisons-the-list, and the
nil-versus-empty distinction that carried "unset" against "turned off" are all
answers to a question a boolean flag does not ask.

## Fallout

Every one of these is part of the change, not a follow-up:

- **`.gitignore:45`** — the `TUNNEL.env` entry goes. With no `--cache-dir`, a
  spec cannot be written into a checkout, so the hazard the entry guards stops
  existing. `CONTRIBUTING.md:894` explains the entry and gets rewritten to say
  the cache is out of the tree by construction.
- **`Makefile:138-148`** — `clean` drops `TUNNEL.env` and wipes
  `<UserCacheDir>/.tunneld` whole: every cached tunnel on the machine, not
  this project's. That is deliberate. Nothing in a flat directory says which
  project a file came from — a name that revealed it would defeat the hashing —
  and a `clean` that removed some cached tunnels and left others would be the
  worse of the two behaviours to explain. The comment stops claiming that
  "Only this project's entry is removed", which is what the recipe already
  fails to do today.
- **`e2e/e2e_test.go:509`** — `TUNNELD_CACHE_DIR=t.TempDir()` becomes
  `TUNNELD_NO_CACHE=true`. e2e asserts nothing about caching; it only needs a
  live run not to touch the developer's own cache, and the flag says that
  directly. No `HOME` or `XDG_CACHE_HOME` rewriting, which would be
  per-platform (`os.UserCacheDir` reads `XDG_CACHE_HOME` on Linux, `$HOME` on
  macOS, `%LocalAppData%` on Windows) and would reach further than the cache.
- **`examples/docker-compose/docker-compose.yml:17`** — `TUNNELD_CACHE_DIR=
  /var/run/tunneld` becomes `XDG_CACHE_HOME=/var/run`, with the named volume
  mounted at `/var/run/.tunneld`. The container's `WORKDIR` is what the
  directory hash covers, so it is stable across runs of the same image.
- **`v1/v1.go`** — `CacheDirEnv` deleted, `NoCacheEnv` added; the `--cache-dir=
  false` mention at `v1/v1.go:172` becomes `--no-cache`.
- **`README.md`** — the `--cache-dir` flag row (:564), the `WithCacheDir` line
  (:657), the `WithCacheDirs` mention (:671), the `CacheDirEnv` constant (:705)
  and the environment table row (:779).
- **`CONTRIBUTING.md`** — the file map row for `v1alpha1/cachedir` (:19)
  becomes one for `v1alpha1/origins`, the contract list (:79), the flag/env
  pairing example (:278), and the gitignore paragraph (:894).
- **`CLAUDE.md:44-46`** — the secrets note, which names `TUNNEL.env` and
  `--cache-dir .`; and `CLAUDE.md:12`, which counts "the eight internal
  contracts".

## Testing

- **`v1alpha1/origins`** — `Key` is stable across order and duplicates, differs
  for different sets, and differs for the same set under two `WithDir` values;
  `WithDir` overrides what `New` seeded; `URLs` returns a copy a caller cannot
  use to reorder the list; `Len`/`At` against the constructed order.
- **`v1alpha1/cache`** — the computed directory from a fake `UserCacheDir`; the
  override when `WithDir` was given; the filename is the key it was handed; a
  load/save/discard round trip; a save into a directory that cannot be created;
  a load of a file that is not there and of one that is malformed. The existing
  table moves over rather than being rewritten. That two working directories
  give two files is `origins`' case, not this package's — the cache no longer
  knows what a working directory is.
- **`v1alpha1/builder`** — `--no-cache` and `TUNNELD_NO_CACHE` reach the
  guards, with a fake cache recording whether it was called, and a second Run
  on the same builder still caching (the flag nils a local, not the field);
  `WithCache(nil)` does the same with no flag; `WithCacheDir` reaches the
  directory it names; two runs with
  different origins ask for different files; two runs with the same origins in
  a different order ask for the same one; `VersionLine` carries the key when
  given origins and drops the clause when given nil.
- **`e2e`** — unchanged beyond the environment line.

## Out of scope

Nothing removes stale files. The directory accumulates one file per working
directory and set of origins ever served, and each is a few hundred bytes. A cache that
expired its own entries would be deciding for somebody which tunnel they have
stopped caring about, and `make clean` removes the directory whole. Worth revisiting if the directories turn out to grow in practice.

## Shape of the work

One pull request, closing #115.

The plan still starts with `Origins` and takes the cache second, because the
cache needs `Key()` and because a type change with no behaviour in it is worth
having green before anything depends on it. That is task ordering, not two
branches: the halves are one breaking change to one surface, and a `main` that
had taken the first without the second would carry an `Origins` nothing uses
and a `CacheDirs` contract on its way out.
