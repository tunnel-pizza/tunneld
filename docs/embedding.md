# Embedding tunneld

`tunneld` is a thin shell around a builder, so another Go program can mount the
same command under its own verb, with identical flags, help, and behaviour, or
skip the command and run a tunnel directly. For what the command does, see the
[README](../README.md) and the [reference](./reference.md).

## Mounting the command

```go
package main

import (
	"context"
	"os"

	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

func main() {
	cmd := v1alpha1.New(
		v1alpha1.WithName("expose"),                  // mount under your own verb
		v1alpha1.WithOrigin("http://localhost:3000"), // a default the user can override
	).Command()

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
```

Every option's value is a *default*, not a fixed setting: the command's flags
bind over the same fields, so an argv value wins. Seeding an origin therefore
lets the command run with no arguments at all.

A program mounting tunneld under its own verb usually wants
`WithShellFallback(false)` as well. It inherits the default along with
everything else, and somebody who typed that verb meaning to name an origin
should be told they forgot one rather than handed a public terminal onto the
machine.

## A tunnel without a CLI

`Run` is the command's body, so executing the command and calling this do the
same work:

```go
b := v1alpha1.New(
	v1alpha1.WithOrigin("http://localhost:3000"),
	v1alpha1.WithShellFallback(false),
)
if err := b.Run(ctx); err != nil { ... }
```

`ctx` is the shutdown handle either way — cancel it and the tunnel comes down,
during startup as well as after. Nothing parses `os.Args`, and there is no
command to assemble that nobody will ever see; the configuration stays the
options it was built with. A process shell wants the other door, which is what
`main.go` is: signals into a context, then `Command().ExecuteContext(ctx)`.

The environment is bound on both paths, so `TUNNELD_*` still beats code here.
What differs is argv: with no command line parsed, the origins are whatever the
environment and the seeds settle on.

## The browser

Whether a run opens a tab is worked out from what the run is doing; the
[reference](./reference.md#the-browser) has the table. `WithOpen` is the
override, and the only one — there is no flag and no environment variable:

```go
v1alpha1.New(v1alpha1.WithOpen(false))  // never
v1alpha1.New(v1alpha1.WithOpen(true))   // always, unless the console is drawing it
```

A service embedding tunneld wants that: it knows nobody is watching however
interactive its own streams happen to look.

## API at a glance

What an embedding program calls, in `v1alpha1`:

```go
func New(opts ...Option) *BuilderImpl       // defaults, then opts; satisfies v1.Builder
func Version() string                       // the release this build is
func VersionLine(origins Origins) string    // the human-facing build banner; nil origins drops the cache key

// The builder's options. Each seeds a flag's default, so argv still wins.
func WithName(name string) Option                  // command name; default "tunneld"
func WithOrigin(origins ...string) Option          // origins, in order; appends across options
func WithProvider(host string) Option              // quick-tunnel host; default tunnel.pizza
func WithCacheDir(dir string) Option               // cache specs here instead of the user's cache directory
func WithLogLevel(level string) Option             // debug|info|warn|error on stderr
func WithIdentityProviders(names ...string) Option // where to look for a mint credential; default github
func WithOpen(open bool) Option                    // force the browser decision; unset means derived
func WithMultiview(multiview bool) Option          // frame the origins together; default true
func WithShellFallback(fallback bool) Option       // no origin at all means $SHELL; default true
func WithStdout(w io.Writer) Option                // help text, the version command, public addresses
func WithStderr(w io.Writer) Option                // banner, the origin each address reaches, logs
```

There are no fluent setters: every knob is an option passed to `New`, and
`v1.Builder` is `Command`, `Origins`, `Name` and `Run`. An embedder on the old
shape changes `New().WithURL(u).Build()` to `New(WithOrigin(u)).Command()`.

`BuilderImpl` also takes `WithCache`, `WithDisplay`, `WithBinder`,
`WithRouter`, `WithConsole`, `WithIdentity` and `WithMotd`, which swap the collaborators the
tunnel run composes, and `WithTunnelFactory`, which replaces `libtunnel.From`
as how a spec becomes a tunnel — `WithCache(nil)` being how an embedder turns
caching off, and what `--no-cache` leaves a run in. They are a contributor's
and a test's concern, not an embedder's — see
[CONTRIBUTING.md → Design conventions](../CONTRIBUTING.md#design-conventions).
The motd board and the log ring are built once in `New` and shared through
their own options — the board into the display and the binder, the ring into
the console and the binder — so an embedder replacing one of those passes the
same instance.

What `v1` declares — the contract it satisfies, the option type every `New`
takes, the errors to match, and the names an operator types:

```go
// Option configures a value while it is constructed; Apply runs a list of
// them in order, so a later one wins.
type Option[T any] func(T)
func Apply[T any](t T, opts ...Option[T]) T

// Builder assembles the tunneld command: what a caller calls once New has
// configured it.
type Builder interface {
    Command() *cobra.Command       // terminal: assembles and returns
    Origins() Origins              // the origins exposed: argv > env > seed
    Name() string                  // configured command name
    Run(ctx context.Context) error // the command's body, without the command
}

// Origins is the list a run exposes, with an identity: Key names the tunnel
// these origins are, which is what the spec cache files it under.
type Origins interface {
    Len() int
    At(i int) *url.URL
    URLs() []*url.URL
    Key() string
}

// match with errors.Is
var ErrInvalidEnv      = errors.New("invalid environment value")
var ErrNoOrigin        = errors.New("no origin")
var ErrInvalidOrigin   = errors.New("invalid origin")
var ErrInvalidLogLevel = errors.New("invalid log level")
var ErrNotReady        = errors.New("tunnel did not become ready")
var ErrNoDocker        = errors.New("docker daemon unreachable")
var ErrUnknownIdentity = errors.New("unknown identity provider")

const LogEnv               = "TUNNELD_LOG"
const OriginsEnv           = "TUNNELD_ORIGINS"
const ProviderEnv          = "TUNNELD_PROVIDER"
const NoCacheEnv           = "TUNNELD_NO_CACHE"
const MultiviewEnv         = "TUNNELD_MULTIVIEW"
const ShellFallbackEnv     = "TUNNELD_SHELL_FALLBACK"
const IdentityProvidersEnv = "TUNNELD_IDENTITY_PROVIDERS"
const CommandName          = "tunneld"
const DefaultProvider      = "tunnel.pizza"
const DefaultMultiview     = true
const DefaultShellFallback = true
const DefaultIdentityProviders = "github"
const AttachScheme   = "attach"  // attach://dockerd/<container>
const ExecScheme     = "exec"    // exec:///<path>
const DockerProvider = "dockerd"
const ArgKey         = "arg"     // a program's arguments, as ?arg=…&arg=…
```

## Packages

The module root is the command; the library tiers sit under it.

```
github.com/tunnel-pizza/tunneld           — package main. Signals → context →
                                            Execute, and nothing else.
github.com/tunnel-pizza/tunneld/v1        — stable Builder contract, the Err*
                                            sentinels, the env/default constants.
github.com/tunnel-pizza/tunneld/v1alpha1  — current implementation: command
                                            assembly, the tunnel it runs, the
                                            version resolution. May change
                                            between alpha revisions.
github.com/tunnel-pizza/tunneld/v1alpha1/<name>  — one implementation each,
                                            behind the contracts in v1alpha1.
                                            See CONTRIBUTING.
```

Application code calls `v1alpha1.New()` and matches errors against `v1`.
There is no façade package re-exporting both, and there cannot be one: a
constructor has to import what it constructs, `v1alpha1` already imports `v1`
for the sentinels, and Go does not allow the cycle.

For the file-by-file map, see
[CONTRIBUTING.md → Where to find things](../CONTRIBUTING.md#where-to-find-things).

## Examples

Self-contained programs in [`examples/`](../examples):

| Example | Demonstrates |
| ------- | ------------ |
| `basic` | Smallest complete wiring — serve on `:3000`, expose it, open a browser. |
| `multi-origin` | Two local services behind one hostname, reachable via `?n`. |
| `attach` | A container's terminal on the public hostname. Starts the container too; needs a Docker daemon. |
| `shell` | A local program's terminal on the public hostname. Runs `zsh`. |

Each starts the origins it exposes, so nothing else needs to be running —
`attach` starts its container too, pulling `ghcr.io/cnuss/zsh` if it is not
already local, and `shell` runs its program when the first viewer opens the
page. All four block until interrupted:

```sh
make run basic
make run multi-origin
make run attach
make run shell
```

`multi-origin` is the one to try in a browser — it serves a different page on
`:3000` and `:4000`, so switching between `/` and `/?1` shows the routing.

The seeded origins are only defaults, so every tunneld flag still works — but
pass them through `go run`, since make would read a leading `--` as one of its
own options:

```sh
go run ./examples/basic http://localhost:8080
```
