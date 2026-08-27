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
tunneld --url http://localhost:3000
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
  https://amber-forest-9021.tunneled.pizza/   -> http://localhost:3000
  https://amber-forest-9021.tunneled.pizza/?1 -> http://localhost:4000
```

The parameter is a routing directive the tunnel's proxy consumes — it never
reaches the origin, and a *valued* parameter (`?1=x`) stays ordinary
application data. A browser then sticks to whichever origin it landed on:
subresources follow their document's URL via `Referer`, and a top-level visit
to `?n` is remembered with a cookie. So a frontend on `:3000` and an API on
`:4000` both work behind a single hostname, without one tunnel per port.

## Output contract

**stdout** is a machine interface: one public URL per origin, in flag order,
and nothing else. Line *i* reaches origin *i*, so `| head -1` is the default
origin's URL.

```sh
URL=$(tunneld --url http://localhost:3000 | head -1)
```

**stderr** carries everything human — the build banner, the origin map above,
and the tunnel's own logs at `--log-level`.

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
| `-u`, `--url` | `TUNNELD_URL` | Local origin to expose. Repeat the flag for more; the first is the default and later ones answer on `?n`. A bare `host:port` implies `http`. Required unless supplied by the variable or seeded in code. |
| `--provider` | `TUNNELD_PROVIDER` | Quick-tunnel provider host to mint against. Default `tunnel.pizza`. |
| `--log-level` | `TUNNELD_LOG` | `debug`\|`info`\|`warn`\|`error` on stderr. Default silent. |
| `--open` | `TUNNELD_OPEN` | Open the default origin's public URL in a browser once the tunnel is live. **Default on** — `--open=false` on a server or in CI. |

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
const CommandName     = "tunneld"
const DefaultProvider = "tunnel.pizza"
const DefaultOpen     = true
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

## Testing

```sh
make test   # unit tests (fast, in-package)
make e2e    # builds the binary and drives its offline paths
make race   # every package under the race detector — the lane CI gates on
```

Neither tier mints a real tunnel — that needs the public internet and a live
provider, which would make CI flaky. Everything up to the mint is covered here;
the tunnel itself is covered by libtunnel's own live tier.

`make e2e` runs `go test -count=1 -v ./e2e`. The `-count=1` defeats the test
cache, since the harness builds the binary at runtime and the cache key
wouldn't otherwise pick up source changes.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the local dev loop, the test-file
convention, and the release process.

## License

[MIT](./LICENSE)
