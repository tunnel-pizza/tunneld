# tunneld reference

The [README](../README.md) is how to use tunneld. This is how each part of it
behaves, and why. `tunneld` below is `npx tunneld` if you haven't installed it.
For embedding tunneld in a Go program, see [embedding.md](./embedding.md).

- [Origins](#origins)
- [Several origins on one address](#several-origins-on-one-address), and [WebSockets](#websockets)
- [Programs](#programs)
- [Containers](#containers)
- [The frame](#the-frame)
- [No arguments at all](#no-arguments-at-all)
- [The console you started it from](#the-console-you-started-it-from)
- [Identity](#identity)
- [Multiview](#multiview)
- [Output contract](#output-contract)
- [The browser](#the-browser)
- [Flags](#flags), [The npm launcher](#the-npm-launcher) and [Environment](#environment)
- [Running in a container](#running-in-a-container)

## Origins

The origins are the arguments. One per local service, in order: the first is the
default and each later one answers on `?n`. A missing scheme implies `http` and
a missing host implies `localhost`, so `:8000`, `localhost:8000` and
`http://localhost:8000` are one origin. At least one is required, from argv,
from `TUNNELD_ORIGINS`, or seeded in code — and argv beats the variable, which
beats the seed, each replacing the one under it rather than adding to it.

```sh
tunneld :3000 :4000 attach://dockerd/my-container
```

A served origin is spelled by the verb, with the provider that answers it in
the authority and the reference after: `attach://dockerd/<container>` is not
proxied but served, and tunneld answers it with a browser terminal attached to
the container, the way `docker attach` attaches — `<container>` is a name, an
id, or a Compose service name. See [Containers](#containers). An
`exec:///<path>` origin is served the same way,
by running the program on a pseudo-terminal — and a bare argument this machine
can run is that origin written short, so `tunneld htop` exposes htop rather
than a hostname that resolves nowhere. See [Programs](#programs). Marking one
origin `http+ws` (or `https+ws`) names the one that owns WebSockets; see
[WebSockets](#websockets).

## Several origins on one address

Pass one argument per local service. They share one public hostname: the first
is the default, and each later one answers on a **bare `?n`** parameter, `n`
being that argument's 0-based position.

```sh
tunneld http://localhost:3000 http://localhost:4000
```

```
tunneld v0.0.68 (libtunnel v0.1.11, built go1.26.5, cache 3f1c0e2a9b7d4c68)
https://0t8qsb6pq3.tunneled.pizza/
  -> http://localhost:3000
  -> http://localhost:4000
```

That one address is the [multiview panel](#multiview), with a tile per origin.
With `--multiview=false` the panel goes away and each origin is named by its
own address instead:

```
tunneld v0.0.68 (libtunnel v0.1.11, built go1.26.5, cache 3f1c0e2a9b7d4c68)
https://0t8qsb6pq3.tunneled.pizza/?0
  -> http://localhost:3000
https://0t8qsb6pq3.tunneled.pizza/?1
  -> http://localhost:4000
```

The parameter is a routing directive tunneld's router consumes — a loopback
server in front of the origins, which is what the tunnel forwards to — so it
never reaches the origin, and a *valued* parameter (`?1=x`) stays ordinary
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
origin, which has nothing to route between. It is routing configuration, not
part of the address: marking an origin, or unmarking it, keeps the tunnel's
hostname.

**Only one origin may be marked**, and that is the shape of the problem rather
than a limit of the flag: two services opening their own sockets behind one
hostname cannot be told apart, however they are spelled. Mark two and the
first keeps the claim: the second origin is still served, its marker dropped
with a warning that names both.

An explicit index still wins over the marker, so a page carrying its own — a
terminal's page, and every tile of the multiview panel — is unaffected.

## Programs

An `exec:///<path>` origin runs a program on this machine and exposes its
terminal, the same way a container's is exposed:

```sh
tunneld exec:///usr/bin/htop
```

The empty authority is this machine — that is the whole of what `exec://` with
no provider says, and why the path is absolute.

A bare argument that names a program on `$PATH` is that origin written short,
since a word that resolves to a program is not a hostname anybody meant:

```sh
tunneld htop
```

What it becomes is the resolved path — `exec:///usr/bin/htop` — which is what
the frame shows, what the origin map prints, and what somebody pastes back to
reach the same program rather than whatever their own `$PATH` finds. A bare
name says which program only on the machine that looked it up.

The words after a program are the program's, the way the words after
`docker run`'s image are the container's:

```sh
tunneld claude --resume
tunneld :3000 htop -d 5
```

Origins are read left to right, and a word that names a program takes the
words after it as its arguments — up to the first word that can only be an
origin: a bare port like `:3000`, a URL with a scheme, or the program's own
word again. That word starts the next origin, so `tunneld bash :8000` is a
shell beside a service, `tunneld bash bash` is two shells, and
`tunneld htop -d 5 :3000` is the same run as the second line above. A flag, a
path, any other bare word or a `host:port` after a program is the program's. A
program can also be quoted together with its arguments, which every shell hands
over as one word — `tunneld :8000 "python3 -m http.server 8000"`. A quoted
group is complete: the words after it are origins again, so
`tunneld 'next dev' bash :3000` is three origins, where `tunneld bash 'next dev'
:3000` is two — a bare program is greedy, so put it last or follow it with a
port or URL. Quoting is also how a program with arguments reads inside a
comma-separated `TUNNELD_ORIGINS`, beside the `?arg=` form below. The
arguments ride the origin as a query, in order, which is how the frame shows
them and how the same run is spelled from the environment or an embedding
program:

```sh
TUNNELD_ORIGINS='exec:///usr/bin/claude?arg=--resume&arg=--model&arg=opus'
```

The arguments are part of what names the tunnel: `claude --resume ABC` and
`claude --resume DEF` are two sessions, so they get two hostnames, and running
the same invocation again from the same directory gets the same one back.

`exec://htop` — the word with its scheme on and nothing after it — is looked up
the same way, because an authority with nothing after it cannot be a provider
being asked for something; it resolves to the same `exec:///usr/bin/htop`.

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

You do not have to wait for it to end. `Ctrl+K` then `r` ends the program and
starts it again, for everyone watching, with the address unchanged — the same
argv, working directory and environment it had the first time. The program is
asked first (`SIGTERM` to it and everything it started) and killed if it has
not left after five seconds.

It is an origin like any other, so it takes an index, gets a multiview tile,
frames itself as `exec:///usr/bin/htop`, and mixes freely with the rest:

```sh
tunneld :3000 attach://dockerd/my-container htop
```

A machine with no pseudo-terminals refuses at startup, with the reason, rather
than minting a hostname in front of a page that cannot work.

## Containers

An `attach://dockerd/<container>` origin exposes a terminal attached to a
running container instead of an HTTP service:

```sh
tunneld attach://dockerd/my-container
```

The scheme is the verb and the authority is where it happens: `attach` is what
tunneld does, `dockerd` is the daemon it does it to. That is what leaves room
for a second way into the same container to sit beside the first — the day
there is one it is `exec://dockerd/<container>`, differing in the word that
says what, not in the word that says to what.

`<container>` is a container name or id, or — when neither matches — a Compose
service name. Compose calls a service `web` in project `proj` by the container
name `proj-web-1`, so the name you wrote in the compose file is never the name
the daemon knows; tunneld looks it up by the labels Compose already wrote:

```sh
tunneld attach://dockerd/web
```

A container literally named `web` still wins. The lookup is scoped to tunneld's
own Compose project when it is running inside one; otherwise it spans the host,
and a service name matching more than one container is an error listing them
rather than a guess.

It is an origin like any other, so it takes an index, gets a multiview tile,
and mixes freely with HTTP origins:

```sh
tunneld :3000 attach://dockerd/my-container
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

## The frame

A program's terminal and a container's are served the same way.

The terminal sits inside a frame, with what the session is written into the
border itself: the origin and the address it answers on along the top, and
along the bottom the keys, the build, the machine serving it, how many people
are watching, and the size everyone has settled on.

```
╭─ attach://dockerd/tunneld-example ───────────────────────────────── https://0t8qsb6pq3.tunneled.pizza/ ╮
│➜  ~ ls                                                                                                 │
│                                                                                                        │
╰─ ^K  commands ── tunneld v0.0.68 (libtunnel v0.1.11, built go1.26.5) ── my-laptop (2 viewers) [104×2] ╯
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
row gets crowded, and a row all beginning `attach://dockerd/` would say nothing. That is the shell talking, not tunneld guessing: a prompt framework
like Oh My Zsh sets the terminal's tab title from its `preexec` hook, which is
the same thing your terminal reads to name a tab. A shell that sets none leaves
the space empty, and one that has not spoken since you connected shows whatever
it last said.

A narrow window drops what it cannot hold, in order: the build first, then the
counts, and the keys last. Above the frame, centred and one row each, sit the
messages the provider sent with the mint, as text with their links by name.

Every key reaches the program or the container except one:

| Key | |
| --- | --- |
| `Ctrl+K` | Opens the frame's commands. The program never sees it. |
| wheel | Scrolls back through what has gone past, when the program has nothing of its own to scroll. Any key returns you to the live screen. |
| then `d` | Detach. Closes your tab's socket; everyone else keeps watching. |
| then `x` | Exit. Ends the run — the tunnel, every origin, and every program it started. |
| then `r` | Restart. Ends the program and starts it again, for everyone watching; the address stays. Offered for a program, not a container. |
| then `l` | Show tunneld's own recent log lines over the terminal. `esc` goes back. |
| then `q` | Show the address as a QR code, for a phone pointed at the screen. `esc` goes back. |
| then `esc` | Cancel, and the keystroke is spent on cancelling. |
| — | Messages from the provider sit above the frame in every view. No key moves them. |

`Ctrl+K` is the frame's, and it does cost you a key — kill-to-end-of-line — but
it is the cheaper of the two on offer. The other candidate, `Ctrl-D`, ends a
shared session for everybody watching.

The wheel is decided per notch. A program that asked for the mouse gets it as a
mouse event; a full-screen program gets it as arrow keys, the way a terminal
with alternate scroll would send it; otherwise it is the frame's, and scrolls
back through what the terminal kept — up to ten thousand lines. Scrolling is
per viewer, so two people can be reading different places in one terminal. The
bottom border says how far back you are, output arriving while you read stays
below you rather than pulling you down to it, and the first key you press puts
you back on the live screen and still reaches the program.

The frame asks whatever it is drawn on for the mouse — your terminal on the
console, xterm in the tab — which is what makes the wheel reach it, and what
stops either from doing its own drag-select. So the frame does that too, the
same way in both places: drag across the pane and the stretch is highlighted
and copied to your clipboard on release, with `copied` in the bottom border to
say so. In the tab the copy goes through the browser's clipboard API; on the
console it goes through OSC 52, which iTerm2 honours once "Applications in
terminal may access clipboard" is on, VS Code's terminal honours as is, and
Terminal.app does not — there the highlight shows and nothing is copied.

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

## No arguments at all

```sh
tunneld
```

exposes `$SHELL` — the one origin every machine has, needing no port to be
listening. It is the ordinary program path, so the terminal is drawn on your
console as well:

```
https://0t8qsb6pq3.tunneled.pizza/
  -> exec:///bin/zsh
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

## The console you started it from

With exactly one `attach://` or `exec://` origin, the terminal is drawn on
your own console too:

```sh
tunneld zsh
```

The URL is printed first, then the frame takes the screen — the same frame a
browser gets, joined to the same session. Both ends see one terminal, count
each other in the viewer count, and interleave what they type.

No browser opens for it: the terminal is already on a screen
you are looking at, and a tab on top of it is a second copy of the one thing
you can see — counted as another viewer, competing for the same keystrokes.
Paste the URL somewhere if you want it there too.

`^K d` gives the console back and leaves the tunnel up — the run says how to
stop it once you are looking at a prompt again. `^K x` ends the run. So does
the program ending on its own: the frame stays up with its last screen and an
`ended` chip in the border, and the next key gives the console back with the
run already over, nothing left waiting for Ctrl+C.
There is no `Ctrl-C` for tunneld while the frame is drawing: the console is in
raw mode, so that keystroke belongs to the program, which is the same thing it
means in the browser.

Without a terminal to draw — several origins, an `http://` one, or a console
that is not a terminal — the run says what to press instead:

```
https://0t8qsb6pq3.tunneled.pizza/
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

## Identity

The mint request can carry a credential, so a provider that gates minting can
tell who is asking. tunneld does not create one — it looks where this machine
already keeps one, and sends what it finds:

```sh
tunneld :3000                               # the default, github
tunneld --identity-providers= :3000         # send nothing
```

`github` asks `gh auth token` first, since a logged-in `gh` is the identity the
machine is actually using, and falls back to `GITHUB_TOKEN`, `GH_TOKEN`,
`GITHUB_PERSONAL_ACCESS_TOKEN` and `ACTIONS_RUNTIME_TOKEN`, in that order. The
last of those is the Actions runner's own token, scoped to the runner's
services rather than the GitHub API; it is sent because it is the only
credential a default Actions job has, and what it is worth is the mint
provider's call.

Finding nothing is ordinary: the tunnel mints anonymously, as every tunnel did
before this. A name the list carries that tunneld has no provider for is an
error before anything is minted, so a typo does not quietly send nothing.

`LIBTUNNEL_TOKEN` is the operator's own override and outranks all of it — set
it and no provider is consulted at all.

The credential never appears in a log line, an error, the origin map or the
cached spec. `--log-level=debug` says which provider answered, never what it
answered with.

## Multiview

Several origins behind one hostname are also served as one page, at the
tunnel's own address:

```sh
tunneld :3000 :4000 :5000
```
```
tunneld v0.0.68 (libtunnel v0.1.11, built go1.26.5, cache 5d2a7e61c0b8f394)
https://0t8qsb6pq3.tunneled.pizza/
  -> http://localhost:3000
  -> http://localhost:4000
  -> http://localhost:5000
```

One iframe per origin, two columns, and an odd count gives the last tile the
full width of the final row. Each tile is drawn the way the terminal frame
draws itself — xterm's own stylesheet and the frame's palette — with the local
address on the top border, the route that reaches it at the other corner as a
link into a tab of its own, and chips on the bottom border for what the page
can do for the frame: reload it where it is, and once it has gone somewhere,
step it back. The border goes white on the tile that has the keyboard, the way
a console marks its active pane, so a keystroke's destination is never a
guess. Anything the provider said with the mint sits in a bar above the
tiles, the same bar the frame draws, in the colour of how loudly it was said.
When a browser is opened at all, this is the page it lands on.

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

- The page pulls xterm.js's stylesheet from jsDelivr, pinned by version and
  checked with subresource integrity, the same copy a terminal's page loads
  along with xterm.js itself. Those requests are the browser's, made when a
  page opens; `--multiview=false` removes the panel's.
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
one address, and every origin it serves is listed beneath it. Whatever the
provider said with the mint — a note, a warning or a caution — is not printed
here: it is shown on every terminal frame and above the panel's tiles, where
the reader is, every run, cached spec or fresh.

On a terminal holding both, with `--multiview=false`, that reads as a map:

```
tunneld v0.0.68 (libtunnel v0.1.11, built go1.26.5, cache 3f1c0e2a9b7d4c68)
https://0t8qsb6pq3.tunneled.pizza/?0
  -> http://localhost:3000
https://0t8qsb6pq3.tunneled.pizza/?1
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

## The browser

Nothing configures this. Once the tunnel is live, tunneld works out whether
anybody is there to look at it:

| It opens a page when | It stays quiet when |
| --- | --- |
| a terminal is on one of its own streams | none of stdin, stdout or stderr is a terminal — a pipeline, a service manager, a CI step, a container |
| the machine has a display | `$CI` is set to anything truthy |
| an ssh session forwarded one (`ssh -X`) | an ssh session did not, so the tab would open where nobody is sitting |
| | the console is already drawing the terminal, which would make the tab a second copy competing for the same keystrokes |

The decision lives in `display.Open`, which is told what the run is doing and
works out what that means — there is no "should I" for a caller to answer.
Every branch says on `--log-level=debug` why it went the way it did, which is
the only account of a decision nobody typed:

```
DEBUG not opening a browser reason="an ssh session with no display to open on"
```

An embedding program can override it, and nothing else can: there is no flag
and no environment variable. See
[embedding.md](./embedding.md#the-browser).

## Flags

The surface is deliberately small. Everything else the engine can do — origin
TLS, spec replay, edge pinning, the cache directory — is reachable through
`libtunnel`'s own `LIBTUNNEL_*` variables, which pass straight through; see
[its README](https://github.com/cnuss/libtunnel#environment-variables).

Flags go before the origins, docker's rule: parsing stops at the first origin,
and every word after it is positional — an origin, or a program's argument. So
`tunneld --log-level debug :3000`, not `tunneld :3000 --log-level debug`; the
environment variables are not argv and work regardless.

Every flag has an environment mirror, and the flag wins: **flag > environment >
default.**

| Flag | Variable | Effect |
| ---- | -------- | ------ |
| `--no-cache` | `TUNNELD_NO_CACHE` | Don't cache the tunnel spec: mint a fresh hostname every run. Cached, it goes to `<user cache dir>/tunneld/<key>.env` — one file per working directory and set of origins, where the key names that pairing and the banner prints it. Never the working directory: a spec is credentials, and a checkout is the one place they must not land. Where it goes is not configurable from a flag; an embedding program passes `v1alpha1.WithCacheDir`. |
| `--provider` | `TUNNELD_PROVIDER` | Quick-tunnel provider host to mint against. Default `tunnel.pizza`. |
| `--log-level` | `TUNNELD_LOG` | `debug`\|`info`\|`warn`\|`error` on stderr. Default silent. |
| `--multiview` | `TUNNELD_MULTIVIEW` | Answer the tunnel's own address with a panel framing every origin. **Default on**, and inert with a single origin, which keeps the bare address for itself. |
| `--shell-fallback` | `TUNNELD_SHELL_FALLBACK` | With no origin from any source, expose `$SHELL` rather than refusing to start. **Default on.** Turn it off to get `ErrNoOrigin` back — what a script wants, and what an embedding program mounting tunneld under its own verb usually wants, since a user who meant to name an origin should be told they forgot rather than handed a public terminal. |
| `--identity-providers` | `TUNNELD_IDENTITY_PROVIDERS` | Identity providers to find a mint credential with, in order — the first to find one wins. **Default `github`**, which asks `gh auth token` and then the GitHub environment variables. Empty sends no credential. A name with no provider behind it is an error before the tunnel is minted, so a typo does not quietly send nothing. `LIBTUNNEL_TOKEN` outranks all of it. |

So the whole thing runs from a container with no command line at all:

```sh
docker run -e TUNNELD_ORIGINS=http://host.docker.internal:3000,http://host.docker.internal:4000 \
           -e TUNNELD_LOG=info \
           ghcr.io/tunnel-pizza/tunneld
```

| Command | Effect |
| ------- | ------ |
| `tunneld version` | Print the build identifier — tunneld's, libtunnel's, and the Go toolchain's — and exit. |

A subcommand's name is read as the subcommand, not as a program: to expose a
program called `version`, spell it `exec:///path/to/version`.

`-d` and `-k` are not tunneld's and never will be: they are the npm
launcher's, below, and the binary refuses both by name, so `tunneld -d` and
`npx tunneld -d` cannot come to mean two things. An embedding program mounting
the command under its own name inherits the refusal; any other flag error goes
to the parent command's own handler.

## The npm launcher

`npx tunneld` runs a small Node launcher, `v1/v1.cjs`, that finds the binary
for the machine and hands it the command line. Everything above is the
binary's. The launcher adds three things of its own.

**`-d` detaches.** As the first word, and only there, it starts the run in the
background and waits for its addresses. Once they are up it prints them on
stdout, what the run said on stderr so far (the banner, the origin each
address reaches) on stderr, and the run's pid, then gives the prompt back. The
tunnel stays up. `-d` is stripped before the binary sees the line, so
`npx tunneld -d :3000 claude` is the run `npx tunneld :3000 claude` would have
been, minus the console: with no terminal on any of its streams, it draws no
frame and opens no browser tab.

A run that ends before its addresses (a bad origin, a mint that fails,
`--help`) is relayed like a foreground one, output and exit status both.
Ctrl+C while `-d` is waiting ends the run, since nothing has been handed back
yet.

**`-k` ends every detached run.** As the only word. Each run gets `SIGINT`,
which is what Ctrl+C sends a foreground run, so each tears down the same way:
the programs its origins started, the attach servers, the tunnel. `-k` waits
for them, names each one it stopped with its addresses, and exits non-zero if
one is still going after 30 seconds.

A detached run leaves three files in `<user cache dir>/tunneld/detached/`,
named by its pid: a record `-k` reads, its stdout, and its stderr, which keeps
growing for as long as the run does and is where to look when one misbehaves.
A record whose pid is gone, or now belongs to another program, is a run that
ended on its own, and `-k` clears it. `make clean` removes the directory with
the rest of the cache, so a run detached before it has to be ended by pid.

Neither flag is offered on Windows yet. Node there can only terminate another
process, which would skip the teardown `-k` exists to run, and a run nothing
can end cleanly is not one to leave behind.

**An ambiguous line is refused.** A bare program takes every word after it,
up to a port or a URL, so

```sh
npx tunneld claude "next dev"
```

is, as written, one program, `claude`, with `next dev` as its argument. The
shell has removed the quotes before anything runs, but a word with whitespace
in it, whose first word is a program on `$PATH`, is most likely a quoted group
meant as an origin of its own. When one sits among a bare program's
arguments, and is not the value of a flag in front of it (`sh -c "npm run
dev"` is one program on purpose), the launcher stops before the run starts
and exits 1, naming both readings, each ready to paste back:

```
tunneld: ambiguous: 'next dev' could be claude's argument or an origin of its own.
  Say which with quotes:
    npx tunneld 'next dev' claude    two origins
    npx tunneld 'claude "next dev"'  one origin
```

Two origins is the bare program moved last. One origin is the program quoted
together with its arguments, which is a single word with whitespace in it and
so never a bare program; the binary splits it back into the program and its
arguments. Neither line is refused in turn. A prompt,
`npx tunneld claude "fix the bug"`, is not refused at all, because `fix` is
not a program.

## Environment

Every knob with an env-expressible value has a mirror constant in `v1`, and
**env beats code** — an operator reconfigures a deployed binary without a
rebuild. Variables are read lazily, where the knob takes effect, so a value set
after construction still lands.

| Variable | Mirrors | Effect |
| -------- | ------- | ------ |
| `TUNNELD_ORIGINS` | the arguments | Local origins, comma-separated in the order argv would take them. An origin URL containing a literal comma has to arrive as an argument, which is parsed for no separator. |
| `TUNNELD_NO_CACHE` | `--no-cache` | Whether to skip the spec cache, so every run mints a fresh hostname. Any value `strconv.ParseBool` accepts. |
| `TUNNELD_PROVIDER` | `--provider` | Quick-tunnel provider host. |
| `TUNNELD_LOG` | `--log-level` | Level of the tunnel's stderr logger. Unset, it is silent. The name predates the flag, which is why it is not `TUNNELD_LOG_LEVEL`. |
| `TUNNELD_MULTIVIEW` | `--multiview` | Whether to serve the multiview panel. Any value `strconv.ParseBool` accepts. |
| `TUNNELD_SHELL_FALLBACK` | `--shell-fallback` | Whether a run given no origin anywhere exposes `$SHELL`. Any value `strconv.ParseBool` accepts. |
| `TUNNELD_IDENTITY_PROVIDERS` | `--identity-providers` | Identity providers to find a mint credential with, comma-separated and in order. Empty sends no credential. |

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

## Running in a container

`ghcr.io/tunnel-pizza/tunneld` is the same binary on a busybox base, running as
root. Every flag has an environment mirror, which is what a container is
configured with:

```sh
docker run --rm -e TUNNELD_ORIGINS=http://host.docker.internal:8080 \
  ghcr.io/tunnel-pizza/tunneld
```

An `attach://dockerd/` origin needs the daemon socket, which is root-equivalent on the
host — a container holding it can start a privileged container and own the
machine:

```sh
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  -e TUNNELD_ORIGINS=attach://dockerd/my-container ghcr.io/tunnel-pizza/tunneld
```

Images are signed by digest, so a moved tag cannot inherit a signature:

```sh
cosign verify ghcr.io/tunnel-pizza/tunneld:<tag> \
  --certificate-identity-regexp '^https://github.com/tunnel-pizza/tunneld/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
