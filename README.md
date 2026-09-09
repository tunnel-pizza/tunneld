# tunneld

[![Go Reference](https://pkg.go.dev/badge/github.com/tunnel-pizza/tunneld.svg)](https://pkg.go.dev/github.com/tunnel-pizza/tunneld)
[![CI](https://github.com/tunnel-pizza/tunneld/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/tunnel-pizza/tunneld/actions/workflows/ci.yml)
[![CodeQL](https://github.com/tunnel-pizza/tunneld/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/tunnel-pizza/tunneld/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/tunnel-pizza/tunneld/badge)](https://scorecard.dev/viewer/?uri=github.com/tunnel-pizza/tunneld)
[![License: FSL-1.1-MIT](https://img.shields.io/badge/License-FSL--1.1--MIT-blue.svg)](./LICENSE.md)

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

Or without a Go toolchain, from npm. The package wraps the same binary, one
build per platform, and hands it the process:

```sh
npx tunneld :3000
```

```sh
tunneld http://localhost:3000   # or just: tunneld :3000
```

```
tunneld v0.0.3 (libtunnel v0.0.50, built go1.26.5)
  https://amber-forest-9021.tunneled.pizza/
    -> http://localhost:3000
```

### Container image

`ghcr.io/tunnel-pizza/tunneld` is the same binary on a busybox base, running as
root. Every flag has an environment mirror, which is what a container is
configured with:

```sh
docker run --rm -e TUNNELD_ORIGINS=http://host.docker.internal:8080 \
  ghcr.io/tunnel-pizza/tunneld
```

A `dockerd://` origin needs the daemon socket, which is root-equivalent on the
host — a container holding it can start a privileged container and own the
machine:

```sh
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  -e TUNNELD_ORIGINS=dockerd://my-container ghcr.io/tunnel-pizza/tunneld
```

Images are signed by digest, so a moved tag cannot inherit a signature:

```sh
cosign verify ghcr.io/tunnel-pizza/tunneld:<tag> \
  --certificate-identity-regexp '^https://github.com/tunnel-pizza/tunneld/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Multiple origins

Pass one argument per local service. They share one public hostname: the first
is the default, and each later one answers on a **bare `?n`** parameter, `n`
being that argument's 0-based position.

```sh
tunneld http://localhost:3000 http://localhost:4000
```

```
tunneld v0.0.3 (libtunnel v0.0.50, built go1.26.5)
  https://amber-forest-9021.tunneled.pizza/
    -> http://localhost:3000
    -> http://localhost:4000
```

With `--multiview=false` the panel goes away and each origin is named by its
own address instead:

```
tunneld v0.0.3 (libtunnel v0.0.50, built go1.26.5)
  https://amber-forest-9021.tunneled.pizza/?0
    -> http://localhost:3000
  https://amber-forest-9021.tunneled.pizza/?1
    -> http://localhost:4000
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
origin has nothing to route between and prints the plain URL.

### WebSockets

A WebSocket handshake carries nothing that says which origin it belongs to. It
has no `Referer` — that header is not part of the handshake — so the routing
that works for ordinary subresources cannot work for a socket, and one that
arrives without an index falls back to a per-browser cookie or to the first
origin.

An app you control can carry the index itself, by building the socket URL from
the page's own:

```js
new WebSocket("wss://" + location.host + "/socket" + location.search)
```

A third-party dev server cannot be told to. Mark its origin instead, and every
otherwise-unroutable handshake goes there:

```sh
tunneld :4000 http+ws://localhost:5173
```

`http+ws`, `http+wss`, `https+ws` and `https+wss` all work and mean the same
thing — the suffix names the origin, it does not describe a transport, and the
origin is dialed by its base scheme either way. It is inert with a single
origin, which has nothing to route between.

**Only one origin may be marked**, and that is the shape of the problem rather
than a limit of the flag: two services opening their own sockets behind one
hostname cannot be told apart, however they are spelled. Marking two is an
error before the tunnel is minted.

An explicit index still wins over the marker, so a page carrying its own — the
container terminal below, and every tile of the multiview panel — is unaffected.

### Containers

A `dockerd://<container>` origin exposes a terminal attached to a running
container instead of an HTTP service:

```sh
tunneld dockerd://my-container
```

`<container>` is a container name or id, or — when neither matches — a Compose
service name. Compose calls a service `web` in project `proj` by the container
name `proj-web-1`, so the name you wrote in the compose file is never the name
the daemon knows; tunneld looks it up by the labels Compose already wrote:

```sh
tunneld dockerd://web
```

A container literally named `web` still wins. The lookup is scoped to tunneld's
own Compose project when it is running inside one; otherwise it spans the host,
and a service name matching more than one container is an error listing them
rather than a guess.

It is an origin like any other, so it takes an index, gets a multiview tile,
and mixes freely with HTTP origins:

```sh
tunneld :3000 dockerd://my-container
```

The semantics are `docker attach`'s, which means most of the behaviour was
decided when the container was started. Without `-t` there is no TTY, so no
line editing and no resize; without `-i` keystrokes reach nothing. The page
shows a small notice bar naming which of those applies — e.g. `no TTY and no
stdin (started without -it) — output only` — rather than leaving you guessing.
With a TTY, Ctrl-C reaches PID 1 and stops the container — that is what
`docker attach` does, not something tunneld adds.

The container's existing output replays when the page opens, so a quiet
container still looks alive.

The terminal sits inside a frame, with what the session is written into the
border itself: the origin and the address it answers on along the top, and
along the bottom the keys, the build, the machine serving it, how many people
are watching, and the size everyone has settled on.

```
╭─ dockerd://tunneld-example ─────────────────────────── https://striped-worm.tunneled.pizza/ ╮
│➜  ~ ls                                                                                      │
│                                                                                             │
╰─ ^K  commands ───── tunneld v0.0.26 (libtunnel v0.0.72, built go1.26.5) ── my-laptop (2 viewers) [93×3] ╯
```

The origin is written the way you typed it, so the same string pasted back into
a command line still works, and the name inside it is the reference you gave
rather than the id the daemon resolved it to — a Compose service stays the name
you wrote in the compose file. Opposite it is the tunnel's own address for this
origin, which with several origins carries the `?n` that reaches this one, so it
is the address to send somebody else — and it is a hyperlink, so a terminal
that understands OSC 8 opens it in a tab of its own. The frame has nothing to
show there until the tunnel is up, because a container is bound before the
tunnel exists.

Centred along the top is whatever the terminal calls itself. A terminal carries
a title and a subtitle and they are not the same thing — a prompt framework
sets the title to the running command's whole line and the subtitle to its name
— so they are joined when they differ and said once when they do not. The same
pair names the browser tab, ahead of the origin — a tab loses its end when the
row gets crowded, and a row all beginning `dockerd://` would say nothing. That is the shell talking, not tunneld guessing: a prompt framework
like Oh My Zsh sets the terminal's tab title from its `preexec` hook, which is
the same thing your terminal reads to name a tab. A shell that sets none leaves
the space empty, and one that has not spoken since you connected shows whatever
it last said.

A narrow window drops what it cannot hold, in order: the build first, then the
counts, and the keys last.

Every key reaches the container except one:

| Key | |
| --- | --- |
| `Ctrl+K` | Opens the frame's commands. The container never sees it. |
| then `d` | Detach. Closes your tab's socket; everyone else keeps watching. |
| then `x` | Exit. Ends the run — the tunnel, every origin, and every program it started. |
| then `l` | Show tunneld's own recent log lines over the terminal. `esc` goes back. |
| then `esc` | Cancel, and the keystroke is spent on cancelling. |

### No arguments at all

```sh
tunneld
```

exposes `$SHELL` — the one origin every machine has, needing no port to be
listening. It is the ordinary program path, so the terminal is drawn on your
console as well:

```
https://thick-firefly.tunneled.pizza/
  -> file:///bin/zsh
```

An argument, `TUNNELD_ORIGINS`, or a seed from an embedding program all outrank
it. A `$SHELL` naming a program that is not there is dropped rather than read
as an address, so what you get is the message about passing an origin and not a
tunnel to nothing.

`--shell-fallback=false` turns it off, and so do `TUNNELD_SHELL_FALLBACK` and
`WithShellFallback(false)`. A bare run then fails with `ErrNoOrigin` the way it
did before. That is the setting for a script, and for a program that embeds
tunneld under its own verb: it inherits the default along with everything else,
and somebody who typed that verb meaning to name an origin should be told they
forgot one rather than handed a public terminal onto the machine.

### The console you started it from

With exactly one `dockerd://` or `file://` origin, the terminal is drawn on
your own console too:

```sh
tunneld zsh
```

The URL is printed first, then the frame takes the screen — the same frame a
browser gets, joined to the same session. Both ends see one terminal, count
each other in the viewer count, and interleave what they type.

No browser opens for it, `--open` or not: the terminal is already on a screen
you are looking at, and a tab on top of it is a second copy of the one thing
you can see — counted as another viewer, competing for the same keystrokes.
Paste the URL somewhere if you want it there too.

`^K d` gives the console back and leaves the tunnel up — the run says how to
stop it once you are looking at a prompt again. `^K x` ends the run.
There is no `Ctrl-C` for tunneld while the frame is drawing: the console is in
raw mode, so that keystroke belongs to the program, which is the same thing it
means in the browser.

Without a terminal to draw — several origins, an `http://` one, or a console
that is not a terminal — the run says what to press instead:

```
https://thick-firefly.tunneled.pizza/
  -> http://localhost:3000
Press Ctrl+C to stop the tunnel...
```

It goes to stderr, like the origin lines above it: stdout is the machine
interface and carries addresses alone.

Nothing happens unless both of the command's own streams are terminals. stdout
is a machine interface — one public URL per origin — and a frame drawn into a
pipe is a wall of escapes where a script expected an address.

While the console is drawing, tunneld's own log lines stop going to it. They
are still kept, and `^K l` is where to read them.

`l` is the only way to see what tunneld is saying about itself. Those lines go
to the console it was started on, which is not where a viewer is — so a
reconnect, a restart, or the edge disowning the hostname would otherwise
explain nothing to the person actually looking at the terminal.

`x` is the one way out of a terminal you opened from your own machine: the
command ends, and its context takes the tunnel and everything under it. A
container is not tunneld's to stop and keeps running; a program is, and does
not.

Pasting works as it does in any terminal, and an app that asked to be told
the difference between pasted and typed text still is.

### Programs

A `file://<program>` origin runs a program on this machine and exposes its
terminal, the same way a container's is exposed:

```sh
tunneld file://htop
```

A bare argument that names a program on `$PATH` is that origin written short,
since a word that resolves to a program is not a hostname anybody meant:

```sh
tunneld htop
```

What it becomes is the resolved path — `file:///usr/bin/htop` — which is what
the frame shows, what the origin map prints, and what somebody pastes back to
reach the same program rather than whatever their own `$PATH` finds. A bare
name says which program only on the machine that looked it up.

The lookup is this machine's own — `$PATH` and the executable bit on Unix,
`PATHEXT` on Windows — so the same argument names a program here and a host
somewhere else, and an explicit scheme always wins. A word that resolves only
through the working directory is left alone: a file that happens to sit where
you started tunneld does not become a public origin because you typed its name.

The program starts when the first viewer opens the page and ends with the
session. It gets a real pseudo-terminal, so a full-screen program draws,
keystrokes reach it, and resizing the browser resizes it. Nothing replays when
the page opens — unlike a container, it has not been running since before you
looked.

**A program that ends can be started again.** Whatever ends it — `Ctrl-C`, the
key the program quits on, or simply finishing — the origin stays up, the page
offers a **restart** where a container's offers only a reconnect, and the next
visit runs it once more on a clean screen. That is a new program and not a
resumed one: nothing it had open before is still open. A container cannot be
offered this, since once its PID 1 has exited there is nothing left to attach
to.

It is an origin like any other, so it takes an index, gets a multiview tile,
frames itself as `file:///usr/bin/htop`, and mixes freely with the rest:

```sh
tunneld :3000 dockerd://my-container htop
```

A machine with no pseudo-terminals refuses at startup, with the reason, rather
than minting a hostname in front of a page that cannot work.


`Ctrl+K` is the frame's, and it does cost you a key — kill-to-end-of-line — but
it is the cheaper of the two on offer. The other candidate, `Ctrl-D`, ends a
shared session for everybody watching. The frame is drawn on the alternate screen, so
there is no scrolling back through what has gone past — a full-screen program
redraws and has nothing to look back at, and a shell has `less`.

When a terminal goes, the page says so — and offers a way back only when there
is one. Your own connection dropping leaves the terminal running, so it offers
to reconnect; a container whose shell has exited leaves nothing, so it offers
nothing.

`Ctrl-C` and `Ctrl-D` end the program, and what that costs depends on what is
behind the origin. A program can be started again, so they go straight through
— the worst a mistake does is send you back to the page. A container cannot:
once its PID 1 has exited the container is gone and the terminal is over for
everybody watching. So on a container the frame asks a second time, and says
in its border which key is waiting. Typing anything else answers it, and so
does waiting a couple of seconds — a press long after the first is a new
intention rather than the other half of a pair.

**The page is unauthenticated.** The tunnel hostname is the only secret, the
same as every other origin tunneld exposes — but here the thing behind it is a
shell. Anyone with the link has it.

## Multiview

Several origins behind one hostname are also served as one page, at the
tunnel's own address:

```
tunneld :3000 :4000 :5000
```
```
tunneld v0.0.4 (libtunnel v0.0.50, built go1.26.5)
  https://cruel-donkey.tunneled.pizza/
    -> http://localhost:3000
    -> http://localhost:4000
    -> http://localhost:5000
```

One iframe per origin, two columns, and an odd count gives the last tile the
full width of the final row. Each tile is labelled with its routing index and
its local address, and links out to that origin on its own. Unless `--no-open`
is passed, this is the page that opens.

The panel is served in front of the origin proxy, so it needs no port and no
origin ever sees the request. It answers **only** the tunnel's own address:
path `/`, an empty query, and a top-level navigation that did not come from a
page already on this host. Everything else belongs to an origin — a subresource
at `/app.js`, a page at `/dashboard`, a frame, a `fetch`, and anything carrying
a query at all. Without that narrowing the panel would swallow every asset an
origin serves, or draw itself inside one of its own tiles.

The query has to be *empty*, not merely free of a routing index, because an
app's root legitimately takes parameters that the caller does not choose. An
OAuth provider redirecting to `https://<host>/?code=…&state=…` reaches the
default origin, as it must. The panel takes no parameters of its own, so it
gives up nothing by answering exactly one address.

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

**stdout** carries the public addresses, one per line and nothing else, so
`tunneld > addresses` is a machine interface and `| head -1` is the default
origin. It carries the help text and `tunneld version` too, which is what keeps
those pipeable.

**stderr** carries everything human: the build banner, the origin each address
reaches, and the tunnel's own logs at `--log-level`. With the panel on there is
one address, and every origin it serves is listed beneath it.

On a terminal holding both, that reads as a map:

```
tunneld v0.0.21 (libtunnel v0.0.66, built go1.26.5)
https://striped-worm.tunneled.pizza/?0
  -> http://localhost:3000
https://striped-worm.tunneled.pizza/?1
  -> http://localhost:4000
```

An address reaches exactly one of the two streams. stdout did carry bare URLs
once while stderr carried a full map, and every address then printed twice
wherever both streams landed together; the de-duplication meant to hide that
could only recognise one file descriptor being literally the other, which a
container's two pipes are not, so it never fired where it was needed most.
Splitting each address from the origin it reaches is not that duplication: the
map still says which origin an address serves, and a script still gets the
addresses without a parser.

The process runs until `SIGINT`/`SIGTERM`, and exits non-zero if the tunnel
fails first.

## Flags

The surface is deliberately small. Everything else the engine can do — origin
TLS, spec replay, edge pinning, the cache directory — is reachable through
`libtunnel`'s own `LIBTUNNEL_*` variables, which pass straight through; see
[its README](https://github.com/cnuss/libtunnel#environment-variables).

The origins are the arguments. One per local service, in order: the first is the
default and each later one answers on `?n`. A missing scheme implies `http` and
a missing host implies `localhost`, so `:8000`, `localhost:8000` and
`http://localhost:8000` are one origin. At least one is required, from argv,
from `TUNNELD_ORIGINS`, or seeded in code — and argv beats the variable, which
beats the seed, each replacing the one under it rather than adding to it.

```sh
tunneld :3000 :4000 dockerd://my-container
```

A `dockerd://<container>` origin is not proxied but served: tunneld answers it
with a browser terminal attached to the container, the way `docker attach`
attaches, and `<container>` is a name, an id, or a Compose service name. See
[Containers](#containers). A `file://<program>` origin is served the same way,
by running the program on a pseudo-terminal — and a bare argument this machine
can run is that origin written short, so `tunneld htop` exposes htop rather
than a hostname that resolves nowhere. See [Programs](#programs). Marking one
origin `http+ws` (or `https+ws`) names the one that owns WebSockets; see
[WebSockets](#websockets).

Every flag has an environment mirror, and the flag wins: **flag > environment >
default.**

| Flag | Variable | Effect |
| ---- | -------- | ------ |
| `--cache-dir` | `TUNNELD_CACHE_DIR` | Directory to cache the tunnel spec in — `TUNNEL.env`, the credentials that let the next run replay the same hostname instead of minting a new one. Repeat the flag for more; comma-separated in the variable. Empty or `true` means the default: a per-project directory under the user's cache directory, named for the working directory. Never the working directory itself — a spec is credentials, and a checkout is the one place they must not land by default. `false` anywhere in the list turns caching off. |
| `--provider` | `TUNNELD_PROVIDER` | Quick-tunnel provider host to mint against. Default `tunnel.pizza`. |
| `--log-level` | `TUNNELD_LOG` | `debug`\|`info`\|`warn`\|`error` on stderr. Default silent. |
| `--no-open` | `TUNNELD_NO_OPEN` | Do not open a public URL in a browser once the tunnel is live. Opening is **on by default** — the panel when there is one, else the default origin — so this is the flag for a server or CI. A browser that cannot be opened is not an error: the tunnel is up either way, and the failure goes to `--log-level=debug` rather than stderr. |
| `--multiview` | `TUNNELD_MULTIVIEW` | Answer the tunnel's own address with a panel framing every origin. **Default on**, and inert with a single origin, which keeps the bare address for itself. |
| `--shell-fallback` | `TUNNELD_SHELL_FALLBACK` | With no origin from any source, expose `$SHELL` rather than refusing to start. **Default on.** Turn it off to get `ErrNoOrigin` back — what a script wants, and what an embedding program mounting tunneld under its own verb usually wants, since a user who meant to name an origin should be told they forgot rather than handed a public terminal. |

So the whole thing runs from a container with no command line at all:

```sh
docker run -e TUNNELD_ORIGINS=http://host.docker.internal:3000,http://host.docker.internal:4000 \
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

	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

func main() {
	cmd := v1alpha1.New(
		v1alpha1.WithName("expose"),               // mount under your own verb
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

## Layout

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
github.com/tunnel-pizza/tunneld/v1alpha1/<name>  — one implementation each:
                                            cachedir, engine, cache, panel,
                                            browser, counter and attach sit
                                            behind the contracts in v1alpha1;
                                            attach declares its own Target and
                                            Targets, and attach/docker
                                            implements both. See CONTRIBUTING.
```

Application code calls `v1alpha1.New()` and matches errors against `v1`.
There is no façade package re-exporting both, and there cannot be one: a
constructor has to import what it constructs, `v1alpha1` already imports `v1`
for the sentinels, and Go does not allow the cycle.

For the file-by-file map, see
[CONTRIBUTING.md → Where to find things](./CONTRIBUTING.md#where-to-find-things).

## API at a glance

What an embedding program calls, in `v1alpha1`:

```go
func New(opts ...Option) *BuilderImpl // defaults, then opts; satisfies v1.Builder
func Version() string                 // the release this build is
func VersionLine() string             // the human-facing build banner

// The builder's options. Each seeds a flag's default, so argv still wins.
func WithName(name string) Option                 // command name; default "tunneld"
func WithOrigin(origins ...string) Option         // origins, in order; appends across options
func WithProvider(host string) Option             // quick-tunnel host; default tunnel.pizza
func WithCacheDir(dirs ...string) Option          // spec cache directories; true/false are instructions
func WithLogLevel(level string) Option            // debug|info|warn|error on stderr
func WithOpen(open bool) Option                   // open a browser when live; default true
func WithMultiview(mv bool) Option                // frame the origins together; default true
func WithShellFallback(fb bool) Option            // no origin at all means $SHELL; default true
func WithEstablishDeadline(d time.Duration) Option // wait for the URL to answer; default 10s
func WithStdout(w io.Writer) Option               // help text, the version command, public addresses
func WithStderr(w io.Writer) Option               // banner, the origin each address reaches, logs
```

There are no fluent setters: every knob is an option passed to `New`, and
`v1.Builder` is only `Command` and `Name`. An embedder on the old shape
changes `New().WithURL(u).Build()` to `New(WithOrigin(u)).Command()`.

`BuilderImpl` also takes `WithCacheDirs`, `WithEngine`, `WithCache`,
`WithBrowser`, `WithCounter` and `WithBinder`, which swap the
collaborators the tunnel run composes. They are a contributor's and a test's
concern, not an embedder's — see
[CONTRIBUTING.md → Design conventions](./CONTRIBUTING.md#design-conventions).

What `v1` declares — the contract it satisfies, and the option type every
`New` takes:

```go
// Option configures a value while it is constructed; Apply runs a list of
// them in order, so a later one wins.
type Option[T any] func(T)
func Apply[T any](t T, opts ...Option[T]) T

// Builder assembles the tunneld command: what a caller calls once New has
// configured it.
type Builder interface {
    Command() *cobra.Command // terminal: assembles and returns
    Origins() []*url.URL     // the origins exposed: argv > env > seed
    Name() string            // configured command name
}

// match with errors.Is
var ErrInvalidEnv      = errors.New("invalid environment value")
var ErrNoOrigin        = errors.New("no origin")
var ErrInvalidOrigin   = errors.New("invalid origin")
var ErrInvalidLogLevel = errors.New("invalid log level")
var ErrNotReady        = errors.New("tunnel did not become ready")

const LogEnv          = "TUNNELD_LOG"
const OriginsEnv      = "TUNNELD_ORIGINS"
const ProviderEnv     = "TUNNELD_PROVIDER"
const CacheDirEnv     = "TUNNELD_CACHE_DIR"
const NoOpenEnv       = "TUNNELD_NO_OPEN"
const MultiviewEnv    = "TUNNELD_MULTIVIEW"
const ShellFallbackEnv = "TUNNELD_SHELL_FALLBACK"
const CommandName     = "tunneld"
const DefaultProvider = "tunnel.pizza"
const DefaultOpen      = true
const DefaultMultiview = true
const DefaultShellFallback = true
```

## Environment

Every knob with an env-expressible value has a mirror constant in `v1`, and
**env beats code** — an operator reconfigures a deployed binary without a
rebuild. Variables are read lazily, where the knob takes effect, so a value set
after construction still lands.

| Variable | Mirrors | Effect |
| -------- | ------- | ------ |
| `TUNNELD_ORIGINS` | the arguments | Local origins, comma-separated in the order argv would take them. An origin URL containing a literal comma has to arrive as an argument, which is parsed for no separator. |
| `TUNNELD_CACHE_DIR` | `--cache-dir` | Spec cache directories, comma-separated and in order. `true` or an empty entry is the default location, `false` anywhere in the list turns caching off, anything else is a path. |
| `TUNNELD_PROVIDER` | `--provider` | Quick-tunnel provider host. |
| `TUNNELD_LOG` | `--log-level` | Level of the tunnel's stderr logger. Unset, it is silent. The name predates the flag, which is why it is not `TUNNELD_LOG_LEVEL`. |
| `TUNNELD_NO_OPEN` | `--no-open` | Whether to leave the browser alone once the tunnel is live. Any value `strconv.ParseBool` accepts. |
| `TUNNELD_MULTIVIEW` | `--multiview` | Whether to serve the multiview panel. Any value `strconv.ParseBool` accepts. |
| `TUNNELD_SHELL_FALLBACK` | `--shell-fallback` | Whether a run given no origin anywhere exposes `$SHELL`. Any value `strconv.ParseBool` accepts. |

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
same as `--log-level loud`), which return an error wrapping `v1.ErrInvalidEnv`
naming the variable and the bad value.

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
go run ./examples/basic http://localhost:8080 --no-open
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

[FSL-1.1-MIT](./LICENSE.md), the Functional Source License. Use, copy, modify,
and redistribute it for any purpose except a product that competes with
tunneld. Each version becomes plain MIT two years after its release.
