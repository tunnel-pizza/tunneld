# tunneld

[![Go Reference](https://pkg.go.dev/badge/github.com/tunnel-pizza/tunneld.svg)](https://pkg.go.dev/github.com/tunnel-pizza/tunneld)
[![CI](https://github.com/tunnel-pizza/tunneld/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/tunnel-pizza/tunneld/actions/workflows/ci.yml)
[![CodeQL](https://github.com/tunnel-pizza/tunneld/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/tunnel-pizza/tunneld/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/tunnel-pizza/tunneld/badge)](https://scorecard.dev/viewer/?uri=github.com/tunnel-pizza/tunneld)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)

`tunneld` exposes already-running local services to the public internet through
a quick tunnel — a lite `cloudflared tunnel --url` that needs no `cloudflared`
binary, no account, and no DNS. The tunnel is driven in-process by
[`libtunnel`](https://github.com/cnuss/libtunnel), which speaks to Cloudflare's
edge directly and mints against [tunnel.pizza](https://tunnel.pizza).

The bell on top: **one tunnel, many origins.**

## Quick Start

```sh
go install github.com/tunnel-pizza/tunneld@latest
```

```sh
tunneld --url http://localhost:3000   # or just: tunneld --url :3000
```

```
tunneld v0.0.3 (libtunnel v0.0.50, built go1.26.5)
  https://amber-forest-9021.tunneled.pizza/ -> http://localhost:3000
```

## Multiple origins

Pass `--url` once per local service. They share one public hostname: the first
is the default, and each later one answers on a **bare `?n`** parameter, `n`
being that flag's 0-based position.

```sh
tunneld --url http://localhost:3000 --url http://localhost:4000
```

```
tunneld v0.0.3 (libtunnel v0.0.50, built go1.26.5)
  https://amber-forest-9021.tunneled.pizza/?0 -> http://localhost:3000
  https://amber-forest-9021.tunneled.pizza/?1 -> http://localhost:4000
```

The parameter is a routing directive the tunnel's proxy consumes — it never
reaches the origin, and a *valued* parameter (`?1=x`) stays ordinary
application data. A browser then sticks to whichever origin it landed on:
subresources follow their document's URL via `Referer`, and a top-level visit
to `?n` is remembered with a cookie. So a frontend on `:3000` and an API on
`:4000` both work behind a single hostname, without one tunnel per port.

**Getting back to the default origin takes `?0`, not a bare `/`.** Stickiness
cuts both ways: once a browser has visited `?1`, a link to `/` carries no index
of its own, so it routes by the referring page's — and the cookie still names
origin 1 besides. Only an explicit index clears a previous choice, routing and
rewriting the cookie in one move.

That is why the map above prints `?0` for the default origin rather than a bare
URL: every address stays correct however much you have clicked around. A single
`--url` has nothing to route between and prints the plain URL.

## Multiview

Several origins behind one hostname are also served as one page, at
`?multiview`:

```
tunneld --url :3000 --url :4000 --url :5000
```
```
tunneld v0.0.4 (libtunnel v0.0.50, built go1.26.5)
  https://cruel-donkey.tunneled.pizza/?0         -> http://localhost:3000
  https://cruel-donkey.tunneled.pizza/?1         -> http://localhost:4000
  https://cruel-donkey.tunneled.pizza/?2         -> http://localhost:5000
  https://cruel-donkey.tunneled.pizza/?multiview -> all 3, framed together
```

One iframe per origin, two columns, and an odd count gives the last tile the
full width of the final row. Each tile is labelled with its routing index and
its local address, and links out to that origin on its own. With `--open` on,
this is the page that opens.

The panel is served in front of the origin proxy, so it needs no port and no
origin ever sees the request. `?multiview` is bare and non-numeric, which is
what keeps it clear of the `?n` routing parameters — the tunnel treats a bare
*numeric* segment as a route and forwards everything else untouched, so an
origin with its own `multiview=…` parameter is unaffected.

Two things worth knowing:

- The page pulls [Basecoat](https://basecoatui.com) (shadcn/ui's components as
  plain CSS) from jsDelivr, pinned by version and checked with subresource
  integrity. That is the one outbound request tunneld makes on your behalf;
  `--multiview=false` removes it.
- **Origins that refuse framing are un-refused, narrowly.** An app sending
  `X-Frame-Options: DENY` or a CSP `frame-ancestors` directive would otherwise
  render as a blank tile, so those two headers are dropped — but only on
  requests the panel itself makes, identified by `Sec-Fetch-Dest` being a frame
  and `Sec-Fetch-Site` being `same-origin`.

  A top-level visit keeps everything the origin sent, and so does another
  site's attempt to frame your tunnel: that arrives cross-site and is left
  alone. Nothing else in the policy is touched — `script-src`, `connect-src`
  and the rest survive directive by directive — and a browser too old to send
  `Sec-Fetch` headers strips nothing, so the failure mode is a blank tile
  rather than a quietly weakened origin. `--multiview=false` turns the whole
  thing off.

## Output contract

**stdout** is a machine interface: one public URL per origin, in flag order,
and nothing else. Line *i* reaches origin *i*, so `| head -1` is the default
origin's URL.

```sh
URL=$(tunneld --url http://localhost:3000 | head -1)
```

**stderr** carries everything human — the build banner, the origin map above,
the multiview address, and the tunnel's own logs at `--log-level`. The panel is
named there and not on stdout: it answers for every origin at once, so a line
for it would break the line-*i*-is-origin-*i* rule.

On a terminal both streams land in the same place, where the split is
invisible and every URL would simply appear twice. So when the two go to the
same destination — a terminal, or `>out 2>&1` — the bare lines are dropped and
only the map is printed; it shows every address anyway, and says which origin
each one reaches. Redirect either stream and both come back, because then the
machine-readable one has a reader of its own.

The process runs until `SIGINT`/`SIGTERM`, and exits non-zero if the tunnel
fails first.

## Flags

The surface is deliberately small. Everything else the engine can do — origin
TLS, spec replay, edge pinning, the cache directory — is reachable through
`libtunnel`'s own `LIBTUNNEL_*` variables, which pass straight through; see
[its README](https://github.com/cnuss/libtunnel#environment-variables).

Every flag has an environment mirror, and the flag wins: **flag > environment >
default.**

| Flag | Variable | Effect |
| ---- | -------- | ------ |
| `-u`, `--url` | `TUNNELD_URL` | Local origin to expose. Repeat the flag for more; the first is the default and later ones answer on `?n`. A missing scheme implies `http` and a missing host implies `localhost`, so `:8000`, `localhost:8000` and `http://localhost:8000` are one origin. Required unless supplied by the variable or seeded in code. |
| `--provider` | `TUNNELD_PROVIDER` | Quick-tunnel provider host to mint against. Default `tunnel.pizza`. |
| `--log-level` | `TUNNELD_LOG` | `debug`\|`info`\|`warn`\|`error` on stderr. Default silent. |
| `--open` | `TUNNELD_OPEN` | Open a public URL in a browser once the tunnel is live — the multiview panel when there is one, else the default origin. **Default on** — `--open=false` on a server or in CI. |
| `--multiview` | `TUNNELD_MULTIVIEW` | Serve every origin together as one panel of framed views, on `?multiview`. **Default on**, and inert with a single `--url`. |

So the whole thing runs from a container with no command line at all:

```sh
docker run -e TUNNELD_URL=http://host.docker.internal:3000,http://host.docker.internal:4000 \
           -e TUNNELD_LOG=info \
           tunneld
```

| Command | Effect |
| ------- | ------ |
| `tunneld version` | Print the build identifier — tunneld's, libtunnel's, and the Go toolchain's — and exit. |

## Embedding

`tunneld` is a thin shell around a builder, so another program can mount the
same command under its own verb — identical flags, help, and behaviour:

```go
package main

import (
	"context"
	"os"

	"github.com/tunnel-pizza/tunneld/lib"
)

func main() {
	cmd := lib.New().
		WithName("expose").                  // mount under your own verb
		WithURL("http://localhost:3000").    // a default the user can override
		Build()

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
```

Every `With*` value is a *default*, not a fixed setting: the command's flags
bind over the same fields, so an argv value wins. Seeding an origin therefore
makes `--url` optional rather than forbidden.

## Layout

The module root is the command; the library tiers sit under it.

```
github.com/tunnel-pizza/tunneld           — package main. Signals → context →
                                            Execute, and nothing else.
github.com/tunnel-pizza/tunneld/lib       — façade: New, Version, BuilderV1.
github.com/tunnel-pizza/tunneld/v1        — stable Builder contract, the Err*
                                            sentinels, the env/default constants.
github.com/tunnel-pizza/tunneld/v1alpha1  — current implementation: command
                                            assembly, the tunnel it runs, the
                                            version resolution. May change
                                            between alpha revisions.
```

Application code imports `lib`. Import `v1` directly to implement the interface
yourself, or to reach a symbol the façade doesn't re-export. Direct access to
the `BuilderImpl` struct lives in `v1alpha1`.

For the file-by-file map, see
[CONTRIBUTING.md → Where to find things](./CONTRIBUTING.md#where-to-find-things).

## API at a glance

The façade — everything an embedding program needs:

```go
type BuilderV1 = v1.Builder    // alias, so callers needn't import v1

func New() BuilderV1     // unconfigured builder
func Version() string    // the release this build is
func VersionLine() string // the human-facing build banner

const DefaultProvider = "tunnel.pizza"
```

The contract itself, in `v1`:

```go
// Builder assembles the tunneld command. Configure with the With* methods
// (each returns the Builder), then call the terminal Build.
type Builder interface {
    WithName(name string) Builder      // command name; default "tunneld"
    WithURL(urls ...string) Builder    // origins, in order; appends across calls
    WithProvider(host string) Builder  // quick-tunnel host; default tunnel.pizza
    WithLogLevel(level string) Builder // debug|info|warn|error on stderr
    WithOpen(open bool) Builder        // open a browser when live; default true
    WithMultiview(mv bool) Builder     // frame the origins together; default true
    WithStdout(w io.Writer) Builder    // the public URLs
    WithStderr(w io.Writer) Builder    // banner, origin map, logs
    Build() *cobra.Command             // terminal: assembles and returns
    Name() string                      // configured command name
}

// match with errors.Is
var ErrInvalidEnv      = errors.New("invalid environment value")
var ErrNoOrigin        = errors.New("no origin")
var ErrInvalidOrigin   = errors.New("invalid origin")
var ErrInvalidLogLevel = errors.New("invalid log level")
var ErrNotReady        = errors.New("tunnel did not become ready")

const LogEnv          = "TUNNELD_LOG"
const URLEnv          = "TUNNELD_URL"
const ProviderEnv     = "TUNNELD_PROVIDER"
const OpenEnv         = "TUNNELD_OPEN"
const MultiviewEnv    = "TUNNELD_MULTIVIEW"
const CommandName     = "tunneld"
const DefaultProvider = "tunnel.pizza"
const DefaultOpen      = true
const DefaultMultiview = true
```

And the env plumbing implementations use, in `v1alpha1`:

```go
func EnvBool(name string) (value, fixed bool, err error)
func EnvDuration(name string) (value time.Duration, fixed bool, err error)
func Logger() *slog.Logger   // silent unless TUNNELD_LOG names a level
```

## Environment

Every knob with an env-expressible value has a mirror constant in `v1`, and
**env beats code** — an operator reconfigures a deployed binary without a
rebuild. Variables are read lazily, where the knob takes effect, so a value set
after construction still lands.

| Variable | Mirrors | Effect |
| -------- | ------- | ------ |
| `TUNNELD_URL` | `--url` | Local origins, comma-separated in the order the repeated flag would take them. An origin URL containing a literal comma has to use the flag, which parses no separator. |
| `TUNNELD_PROVIDER` | `--provider` | Quick-tunnel provider host. |
| `TUNNELD_LOG` | `--log-level` | Level of the tunnel's stderr logger. Unset, it is silent. The name predates the flag, which is why it is not `TUNNELD_LOG_LEVEL`. |
| `TUNNELD_OPEN` | `--open` | Whether to open a browser once the tunnel is live. Any value `strconv.ParseBool` accepts. |
| `TUNNELD_MULTIVIEW` | `--multiview` | Whether to serve the multiview panel. Any value `strconv.ParseBool` accepts. |

Binding is [spf13/viper](https://github.com/spf13/viper), one instance per
built command rather than the package global, with each variable bound
explicitly to the constant naming it in `v1` — so the operator-facing strings
live in one registry instead of being derived from flag names.

Names follow `TUNNELD_<KNOB>` for core knobs and `TUNNELD__<IMPL>_<KNOB>` —
double underscore — for implementation-scoped ones, so two implementations can
each expose a `TIMEOUT` without colliding.

An override that is set but unparsable is reported, never silently ignored — a
typo'd knob that quietly did nothing would be indistinguishable from one that
worked. That holds for the flag mirrors (`TUNNELD_LOG=loud` is an error, the
same as `--log-level loud`) and for the `EnvBool`/`EnvDuration` helpers, which
return an error wrapping `v1.ErrInvalidEnv` naming the variable and the bad
value.

The tunnel engine carries its own `LIBTUNNEL_*` surface for everything this one
doesn't expose. Those variables pass straight through and are documented in
[libtunnel](https://github.com/cnuss/libtunnel#environment-variables), not
mirrored here.

## Examples

Self-contained programs in [`./examples`](./examples):

| Example | Demonstrates |
| ------- | ------------ |
| `basic` | Smallest complete wiring — serve on `:3000`, expose it, open a browser. |
| `multi-origin` | Two local services behind one hostname, reachable via `?n`. |

Each starts the origins it exposes, so nothing else needs to be running. Both
block until interrupted:

```sh
make run basic
make run multi-origin
```

`multi-origin` is the one to try in a browser — it serves a different page on
`:3000` and `:4000`, so switching between `/` and `/?1` shows the routing.

The seeded origins are only defaults, so every tunneld flag still works — but
pass them through `go run`, since make would read a leading `--` as one of its
own options:

```sh
go run ./examples/basic --url http://localhost:8080 --open=false
```

## Testing

```sh
make test   # unit tests (fast, in-package)
make e2e    # builds the binary and every example, drives their offline paths
make race   # every package under the race detector — the lane CI gates on
```

Neither tier mints a real tunnel — that needs the public internet and a live
provider, which would make CI flaky. Everything up to the mint is covered here;
the tunnel itself is covered by libtunnel's own live tier. That is also why the
harness drives each example with `--help`: it exercises the whole assembly path
and exits without a packet.

`make e2e` runs `go test -count=1 -v ./e2e`. The `-count=1` defeats the test
cache, since the harness builds the binaries at runtime and the cache key
wouldn't otherwise pick up source changes.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the local dev loop, the test-file
convention, what makes a good example, and the release process.

## License

[MIT](./LICENSE)
