# `--url dockerd://<container>` — a browser terminal on the tunnel

Design for [#12](https://github.com/tunnel-pizza/tunneld/issues/12). Written
2026-08-27, against tunneld v0.0.6.

## The problem

Every `--url` value tunneld accepts today names an already-running HTTP service.
A container is not one. Reaching a shell inside a container running on some
other machine means SSH to that machine first, which is exactly the round trip
tunneld exists to remove for HTTP.

`--url dockerd://<container-name-or-id>` puts a terminal for that container on
the public hostname, attached the way `docker attach` attaches.

## What `docker attach` means

The behaviour being matched, since half of it is decided before tunneld is
involved:

- It **reattaches to PID 1** — the process the container was started with, not a
  new one.
- **TTY is fixed at `docker run` time.** With `-t` (`Config.Tty`) the attach
  stream is raw and stdout and stderr are already merged, and `ContainerResize`
  is meaningful. Without it the stream is stdcopy-framed — an 8-byte header per
  chunk carrying stream id and length — and there is no resize, no echo, and no
  line editing.
- **stdin is fixed the same way.** Without `-i` (`Config.OpenStdin`) keystrokes
  reach nothing.
- **Signals reach PID 1.** With a TTY the container's own line discipline turns
  `^C` into SIGINT for the entrypoint, so Ctrl-C stops the app and the container
  with it. That is the semantic, not a defect to paper over.
- **Attach is shared, not exclusive.** Every attached client sees the same
  output and every one can write stdin.
- **When PID 1 exits the stream ends** and the container is gone.

## Decisions

Each of these was chosen deliberately; the alternative is named so a later
reader can tell a decision from an accident.

**No authentication.** The hostname is the secret, the same posture as every
other origin tunneld exposes. An operator who types `dockerd://` has opted into
publishing a shell. Guarding it is a later, paid concern, and the seam for it is
the single `attach.Server` entry point — one place to add a check. Rejected:
minting a token into the URL, which would be real protection but is the feature
being reserved.

**Degraded containers are served, not refused.** Started without `-t` or `-i`,
the page still attaches and prints one dim line saying which capability is
missing. Refusing would make tunneld stricter than the thing it imitates and
would rule out the legitimate "watch this container's output from my phone"
case; serving it silently would leave somebody typing into a terminal that
cannot possibly answer.

**The backlog replays.** `AttachOptions{Logs: true}`, so opening the URL shows
what the container has already printed rather than an empty screen. `docker
attach` does not do this, and it is the one place this design departs from it:
an empty terminal on a quiet container is indistinguishable from a broken one,
and the page is often opened long after the container started.

**No new flag and no new environment variable.** `--url` and `$TUNNELD_URL`
already carry it. The scheme is the whole interface.

**`ServeAttach` owns the websocket.** `k8s.io/cri-streaming`'s
`remotecommand.ServeAttach` upgrades through `k8s.io/streaming`'s `wsstream` and
speaks `v4.channel.k8s.io`. There is no injection point for a different
websocket library, and the `Attacher` interface it calls takes exactly what
docker's attach hands back, so no local pty is involved either. Rejected:
implementing the channel protocol directly on `github.com/coder/websocket`,
which would buy explicit ping/pong keepalive at the cost of reimplementing the
stream plumbing.

## Architecture

A `dockerd://` value is not proxyable, so tunneld serves it. Each one gets a
loopback HTTP origin of its own, and the origin list splits in two — same
length, same order:

```
--url http://localhost:3000  --url dockerd://api  --url dockerd://db

display    http://localhost:3000   dockerd://api         dockerd://db
dialable   http://localhost:3000   http://127.0.0.1:a    http://127.0.0.1:b
                                    │                     │
                                    attach.Server         attach.Server
                                      GET /        index.html (xterm)
                                      GET /attach  ServeAttach → docker.Attacher
```

`libtunnel` receives `dialable`. Everything human — the stderr map, the
multiview tile labels — reads `display`. `PublicURL`, the bare `?n` routing
parameter, `--multiview` and `report` need no changes: a container is another
origin, and with several origins it gets a tile like anything else.

Two alternatives were rejected. A **libtunnel `Interceptor` matching `?n`**
would still need a placeholder URL occupying slot *n* in the origin list, so it
means faking an origin and then fighting the routing parameter's strip. And
**`WithListener`**, which hands libtunnel a `net.Listener` directly, is
single-origin and mutually exclusive with `WithLocalURL` — it would make mixed
`dockerd://` and `http://` origins impossible in one command, for one special
case that does not justify a second code path.

## Packages

```
v1alpha1/attach/
  attach.go        Server: the loopback origin, its two routes, the ServeAttach wiring
  attach_test.go
  index.html       xterm shell, embedded
  docker/
    docker.go      Attacher over the docker SDK: resolve, inspect, attach, resize
    docker_test.go
```

`attach.go` knows HTTP and the k8s stream protocol and nothing about docker.
`docker.go` knows docker and nothing about HTTP. `index.html` lives beside
`attach.go` because `go:embed` cannot reach outside its own package directory —
the same reason `multiview/index.html` sits where it does.

The seam between them:

```go
// Target is one attachable thing behind a Server.
type Target interface {
	remotecommand.Attacher // AttachContainer(ctx, name, uid, container, in, out, err, tty, resize)
	Name() string          // the subject of the page title
	TTY() bool             // Config.Tty — sets Options.TTY, decides whether resize means anything
	Stdin() bool           // Config.OpenStdin — sets Options.Stdin, drives the notice
	Close() error
}
```

Four methods is the whole provider contract. A second provider — podman, or the
`pty://` local shell where `github.com/creack/pty` would land — implements them
and `attach.go` does not change. `docker.Attacher` satisfies it.

## The wire

Browser to `attach.Server` is the `v4.channel.k8s.io` subprotocol, which the
browser's own `WebSocket` speaks with no client library:

```js
new WebSocket(url, ['v4.channel.k8s.io'])
```

Frames are binary and the first byte is the channel: `0` stdin, `1` stdout, `2`
stderr, `3` error (a JSON status), `4` resize. Resize messages are
`{"Width":80,"Height":24}`, read by a streaming `json.Decoder`, so successive
objects on that channel work without framing of their own. About 40 lines of
JavaScript.

Facts read out of the dependency's source rather than assumed, each of which
would otherwise be a trap:

- `remotecommand.Options{Stdin, Stdout, Stderr, TTY}` is a plain struct. There
  is no query-parameter contract to satisfy — the server constructs it.
- `wsstream` installs its own `Handshake` function, which **replaces**
  `golang.org/x/net/websocket`'s default origin check. Nothing needs configuring
  for the request to arrive through a tunnel.
- The server writes one empty frame on the lowest writable channel as soon as
  the connection is established. The page treats that as "attached".
- `supportedProtocols` is consumed only on the SPDY path; the websocket path
  negotiates its own subprotocol names. It is passed regardless, as
  `remotecommand.SupportedStreamingProtocols`.

### Routing the socket

The page builds its socket URL as `"/attach" + location.search`.

Loaded at `/?1`, the page's socket goes to `/attach?1` — the same bare numeric
parameter libtunnel already consumes and strips, so the socket lands on the same
origin the page did. With a single origin `location.search` is empty and the URL
is plain `/attach`. No origin index has to be plumbed into the template.

This is load-bearing rather than tidy. Browsers send `Origin` but **not
`Referer`** on a websocket handshake, so libtunnel's referer-based subresource
routing does not fire, and routing would fall back to the sticky
`libtunnel-origin` cookie — which is last-write-wins across tabs. Two containers
open in two tabs would fight over one cookie and the second tab would steal the
first one's terminal.

### Keepalive

Cloudflare closes idle websockets and JavaScript cannot send websocket pings.
The page re-sends the current terminal size on channel 4 every 30 seconds; a
`ContainerResize` to the dimensions already in effect is a no-op. `ServeAttach`
is given a 2-minute idle timeout, so a page that has genuinely gone away is
reaped while a live one always resets it.

## The docker provider

`Open(ctx, ref)` builds a client with `client.FromEnv` and
`client.WithAPIVersionNegotiation()`, then calls `ContainerInspect`. Failures
happen **before the tunnel is minted**:

- daemon unreachable → `v1.ErrNoDocker`
- `client.IsErrNotFound` → `v1.ErrInvalidOrigin`, naming the ref
- container not running → `v1.ErrInvalidOrigin`, naming the ref and its state

`InspectResponse.Config.Tty` and `.Config.OpenStdin` are held for `TTY()` and
`Stdin()`.

`AttachContainer` calls `ContainerAttach(ctx, id, container.AttachOptions{Stream:
true, Stdin: <OpenStdin>, Stdout: true, Stderr: true, Logs: true})`, which
returns a `types.HijackedResponse` carrying `Conn net.Conn` and `Reader
*bufio.Reader`. From there:

| Container | Output | Resize |
|---|---|---|
| TTY | `io.Copy(out, resp.Reader)` | channel → `ContainerResize` |
| no TTY | `stdcopy.StdCopy(out, errw, resp.Reader)` | drained, ignored |

stdin is `io.Copy(resp.Conn, in)` in both rows, and only when `OpenStdin` — the
TTY flag and the stdin flag are independent, so a container can have either
without the other.

`stdcopy.StdCopy` is what puts container stderr on channel 2, where xterm renders
it alongside stdout. When stdin ends, `CloseWrite` on the hijacked connection.
When PID 1 exits the reader hits EOF, `AttachContainer` returns,
`ServeAttach` writes a status on channel 3, and the page prints a dim `detached`.

Verified signatures (docker SDK v28.5.2):

```go
ContainerInspect(ctx, id) (container.InspectResponse, error)
ContainerAttach(ctx, id, container.AttachOptions) (types.HijackedResponse, error)
ContainerResize(ctx, id, container.ResizeOptions{Height, Width uint}) error
```

## Surface changes

**`parseOrigins`** learns one scheme. `dockerd://<ref>` is valid when the host is
non-empty; `url.Parse` preserves case and underscores, so container names survive
verbatim. `dockerd://` with no ref is `ErrInvalidOrigin`, and so is a
`dockerd://` value carrying a path or query — nothing consumes them, and
silently dropping a typo is the failure mode this codebase already avoids
everywhere else. A bare `foo` still means `http://foo`; the implicit-scheme rule
does not change.

**One new sentinel** in `v1`:

```go
// ErrNoDocker reports a dockerd:// origin that could not reach a Docker
// daemon …
var ErrNoDocker = errors.New("docker daemon unreachable")
```

"Is Docker running" is the likeliest failure of this feature and deserves its own
`errors.Is` rather than hiding inside `ErrInvalidOrigin`.

**`report`** prints `-> dockerd://api`. The operator never typed `127.0.0.1` and
never needs to see it.

**Multiview tiles** label a container origin `dockerd://api` rather than the bare
host `api`, so a tile cannot be mistaken for a hostname.

**Shutdown**: every `attach.Server` and docker client closes on context
cancellation, ahead of the tunnel.

**`--help`** gains one line for the scheme. The `Long` description gains a
one-line example.

## Testing

**`attach_test.go` needs no daemon.** It drives the whole `Server` against a fake
`Target` over `httptest`: channel framing in both directions, the resize decode,
`/` serving the page, and each degraded notice. The fake is what makes the
failure paths testable at all — no-TTY and no-stdin containers are awkward to
arrange for real and trivial to fake.

**`docker_test.go` skips on `client.Ping` failure.** Where a daemon exists it
starts a real `alpine` container both ways — with `-it` and without — attaches,
writes `echo hi`, and expects `hi` back on the right channel.

**`e2e/e2e_test.go`** gains a row for a new `examples/attach`, driven with
`--help` like every other example, per the convention in CONTRIBUTING.

**Mutation checks** on the guards that matter, per the project's habit: drop the
`location.search` suffix and confirm a two-origin test fails; force `tty` false
on a TTY container and confirm the stdcopy path corrupts the stream.

README table row and a CONTRIBUTING note for the new scheme land in the same PR.

## Dependency cost

Stated plainly, because it is not small:

- `k8s.io/cri-streaming` and `k8s.io/streaming` are light — `golang.org/x/net`,
  `klog`, `moby/spdystream`. `gorilla/websocket` and `golang.org/x/net` are
  already indirect dependencies through cloudflared.
- `github.com/docker/docker` is not light. It is `+incompatible` and pulls the
  moby and opencontainers type tree. That is the price of the SDK over
  hand-rolling roughly 150 lines of HTTP-over-unix-socket, and the SDK was the
  stated preference.

## Frontend

`index.html` follows `multiview/index.html`'s conventions: jsDelivr with
Subresource Integrity, dark by default, no build step, no outbound request
beyond the CDN.

```
https://cdn.jsdelivr.net/npm/@xterm/xterm@6.0.0/css/xterm.css
https://cdn.jsdelivr.net/npm/@xterm/xterm@6.0.0/lib/xterm.js
https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.11.0/lib/addon-fit.js
```

Those are the current releases and the real paths — **xterm 6 ships no `.min.`
files**. jsDelivr will minify on the fly for a `.min.js` URL, but the SRI hash
must match the bytes actually served, so the unminified files are used and their
hashes computed from them with `openssl dgst -sha384 -binary <f> | openssl base64
-A`, the same way multiview pins Basecoat. The globals are `Terminal` and
`FitAddon.FitAddon`.

Minimal by instruction: the terminal fills the viewport, there is no chrome, and
the page is blank until the socket delivers something. The dim notice line and
the replayed backlog are the only things that appear without the container
speaking. A `ResizeObserver` drives the fit addon, which drives channel 4.
