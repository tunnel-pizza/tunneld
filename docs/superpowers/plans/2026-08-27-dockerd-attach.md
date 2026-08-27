# `--url dockerd://<container>` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `tunneld --url dockerd://<container-name-or-id>` puts a browser terminal on the tunnel's public hostname, attached to that container the way `docker attach` attaches.

**Architecture:** A `dockerd://` value is not an HTTP service, so tunneld becomes one on its behalf. Each such origin gets its own loopback HTTP server serving an xterm page at `/` and the k8s `v4.channel.k8s.io` stream protocol at `/attach`; the URL handed to libtunnel is that server's `http://127.0.0.1:<port>`. The origin list therefore splits into `display` (what the operator typed, used for the reported map and multiview tile labels) and `dialable` (what libtunnel proxies to), same length and same order — so `?n` routing, `PublicURL`, `--multiview` and `report` need no structural change.

**Tech Stack:** Go 1.26 · `k8s.io/cri-streaming/pkg/streaming/remotecommand` (owns the websocket) · `github.com/docker/docker` SDK v28 · `@xterm/xterm` 6.0.0 + `@xterm/addon-fit` 0.11.0 from jsDelivr

**Spec:** [`docs/superpowers/specs/2026-08-27-dockerd-attach-design.md`](../specs/2026-08-27-dockerd-attach-design.md)

## Global Constraints

Copied from the spec and from CONTRIBUTING. Every task's requirements implicitly include these.

- **Branch `feat/dockerd-attach`, issue [#12](https://github.com/tunnel-pizza/tunneld/issues/12).** Never push to `main`. The PR body carries `Closes #12`.
- **One test file per source file.** `something.go` → `something_test.go`. New cases join the table in the existing file. Never a file named after a scenario. The only exceptions in this repo are `lib/example_test.go` and `e2e/`.
- **`main.go` stays thin.** Nothing in this plan touches it.
- **Go module floor is 1.26** (`go.mod`); do not raise it.
- **`stdout` carries public URLs and nothing else.** Everything human goes to stderr.
- **No new flag and no new environment variable.** `--url` and `$TUNNELD_URL` carry the scheme.
- **Table-driven tests** with a `name` field and `t.Run(tc.name, ...)`, matching `v1alpha1/tunnel_test.go` and `v1alpha1/multiview/multiview_test.go`.
- **Comments explain why, not what** — match the density and voice of the surrounding files. This codebase's comments argue for decisions.
- **Pinned CDN assets carry Subresource Integrity**, hashes computed with `openssl dgst -sha384 -binary <f> | openssl base64 -A`.
- Commit after every task. Do not push until the whole plan is green.

**Verified facts** the implementation depends on — established by reading the dependency sources, do not re-derive:

- `remotecommand.Options{Stdin, Stdout, Stderr, TTY}` is a plain struct. The server constructs it; there is no query-parameter contract.
- `wsstream` installs its own `Handshake`, **replacing** `golang.org/x/net/websocket`'s origin check. Cross-origin needs no configuration.
- On connect the server writes an empty payload to the lowest writable channel, and `Conn.write` always emits `len(data)+1` bytes — so the client receives a **1-byte binary frame containing just the channel number**. That is the "attached" marker.
- Resize messages are `{"Width":80,"Height":24}` on channel 4, read by a streaming `json.Decoder`, so successive objects need no framing.
- `url.Parse("dockerd://My_Container")` preserves case and underscores; `url.Parse("dockerd://")` yields an empty `Host`.
- docker SDK v28: `ContainerInspect(ctx, id) (container.InspectResponse, error)`, `ContainerAttach(ctx, id, container.AttachOptions) (types.HijackedResponse, error)`, `ContainerResize(ctx, id, container.ResizeOptions{Height, Width uint}) error`, `HijackedResponse.CloseWrite()`, `client.IsErrNotFound`, `client.IsErrConnectionFailed`, `stdcopy.StdCopy(dstout, dsterr, src)`.
- xterm 6 ships **no `.min.` files**; the real paths are `/css/xterm.css`, `/lib/xterm.js`, `/lib/addon-fit.js`.

---

### Task 1: The `dockerd` scheme reaches `parseOrigins`

The scheme becomes contract before anything can serve it. Nothing starts a container yet — this task only makes `--url dockerd://api` parse instead of being rejected as an unproxyable scheme.

**Files:**
- Modify: `v1/v1.go` (add `DockerScheme` const and `ErrNoDocker` sentinel)
- Modify: `v1alpha1/tunnel.go` (`parseOrigins`)
- Test: `v1/v1_test.go`, `v1alpha1/tunnel_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `v1.DockerScheme = "dockerd"` (string const), `v1.ErrNoDocker` (error). `parseOrigins([]string) ([]*url.URL, error)` keeps its signature and now returns `*url.URL` values whose `Scheme` may be `"dockerd"` and whose `Host` is the container reference.

- [ ] **Step 1: Write the failing tests**

Add these rows to the `cases` table in `TestParseOriginsAccepts` in `v1alpha1/tunnel_test.go`:

```go
		{"a container by name", []string{"dockerd://api"}, []string{"dockerd://api"}},
		{"a container by id", []string{"dockerd://3f2a1b9c8d7e"}, []string{"dockerd://3f2a1b9c8d7e"}},
		{"a container name keeps case and underscores", []string{"dockerd://My_Container"}, []string{"dockerd://My_Container"}},
		{
			"a container beside an http origin, in order",
			[]string{"http://localhost:3000", "dockerd://api"},
			[]string{"http://localhost:3000", "dockerd://api"},
		},
```

Add these rows to the `cases` table in `TestParseOriginsRejects` in the same file:

```go
		{"a container with no name", []string{"dockerd://"}, v1.ErrInvalidOrigin, "dockerd://"},
		{"a container with a path", []string{"dockerd://api/sh"}, v1.ErrInvalidOrigin, "dockerd://api"},
		{"a container with a query", []string{"dockerd://api?tty=1"}, v1.ErrInvalidOrigin, "dockerd://api"},
```

In `v1/v1_test.go`, add one row to the `cases` table in `TestEnvNamesAreStable`:

```go
		{v1.DockerScheme, "dockerd"},
```

and one entry to the `sentinels` map in `TestSentinelsAreDistinct`:

```go
		"ErrNoDocker":        v1.ErrNoDocker,
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./v1/... ./v1alpha1/ -run 'ParseOrigins|EnvNames|Sentinels'`
Expected: FAIL — `undefined: v1.DockerScheme`, `undefined: v1.ErrNoDocker`, and the accept cases erroring with "only http and https origins can be proxied".

- [ ] **Step 3: Add the constant and the sentinel**

In `v1/v1.go`, add after the `ErrNotReady` block:

```go
// ErrNoDocker reports a dockerd:// origin whose Docker daemon could not be
// reached: the socket refused the connection, or the API answered an error
// that is not about this particular container. It is separate from
// ErrInvalidOrigin because the origin may be perfectly well-formed and the
// daemon simply not running, which is by far the likeliest failure of a
// container origin and has a different lever — start Docker, or point
// $DOCKER_HOST at the socket that has it.
var ErrNoDocker = errors.New("docker daemon unreachable")
```

In the same file, add to the `const` block that holds `CommandName` and `DefaultProvider`:

```go
	// DockerScheme names a running container as an origin instead of an HTTP
	// service: --url dockerd://<container-name-or-id> serves a terminal
	// attached to that container, on the same public hostname and the same
	// ?n index as any other origin.
	//
	// The daemon, not the container, is what the scheme names — the same
	// reading as dockerd's own socket — because the container reference is
	// the authority component that follows it.
	DockerScheme = "dockerd"
```

- [ ] **Step 4: Teach `parseOrigins` the scheme**

In `v1alpha1/tunnel.go`, inside the loop in `parseOrigins`, insert this block immediately after the `u, err := url.Parse(s)` error check and before the `if u.Scheme != "http" && u.Scheme != "https"` check:

```go
		// A container is not proxied at all: it is served, by a loopback
		// origin bindOrigins stands up later. Everything the shorthands below
		// fill in — a default scheme, a default host, a preserved path — is
		// meaningless here, so the value is taken exactly as typed and
		// anything extra is an error rather than a silent drop.
		if u.Scheme == v1.DockerScheme {
			if u.Host == "" {
				return nil, fmt.Errorf("%w: %q names no container, pass e.g. dockerd://my-container", v1.ErrInvalidOrigin, s)
			}
			if u.Path != "" || u.RawQuery != "" || u.User != nil {
				return nil, fmt.Errorf("%w: %q carries more than a container reference; pass %s://%s", v1.ErrInvalidOrigin, s, v1.DockerScheme, u.Host)
			}
			origins = append(origins, u)
			continue
		}
```

Then update the rejection message on the line below it so it names the third scheme:

```go
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("%w: %q has scheme %q, want http, https or %s", v1.ErrInvalidOrigin, s, u.Scheme, v1.DockerScheme)
		}
```

The existing `TestParseOriginsRejects` row `{"unproxyable scheme", []string{"ftp://localhost:21"}, v1.ErrInvalidOrigin, "ftp"}` still passes — it asserts the error names `ftp`, which it does.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./v1/... ./v1alpha1/ -run 'ParseOrigins|EnvNames|Sentinels' -v`
Expected: PASS, including every pre-existing row.

- [ ] **Step 6: Commit**

```bash
git add v1/v1.go v1/v1_test.go v1alpha1/tunnel.go v1alpha1/tunnel_test.go
git commit -m "feat: parse dockerd:// as an origin scheme

A container reference is taken exactly as typed — no implied scheme, no
implied host, and a path or query is an error rather than a silent drop,
since nothing downstream consumes them.

Refs #12."
```

---

### Task 2: The loopback origin serves the terminal page

The `attach` package appears: a `Target` seam, a `Server` that binds `127.0.0.1:0`, and the xterm page it answers `/` with. No streaming yet — `/attach` arrives in Task 3.

**Files:**
- Create: `v1alpha1/attach/attach.go`
- Create: `v1alpha1/attach/index.html`
- Test: `v1alpha1/attach/attach_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `attach.Target` interface — `remotecommand.Attacher` plus `Name() string`, `TTY() bool`, `Stdin() bool`, `Close() error`
  - `attach.Serve(ctx context.Context, target Target, log *slog.Logger) (*Server, error)`
  - `(*Server).URL() *url.URL` — `http://127.0.0.1:<port>`
  - `(*Server).Close() error`

- [ ] **Step 1: Add the dependencies**

```bash
go get k8s.io/cri-streaming@v0.37.0
go get github.com/gorilla/websocket@latest
go mod tidy
```

`gorilla/websocket` becomes a direct dependency because the tests dial the server with it; it is already in the module graph via cloudflared and cri-streaming.

- [ ] **Step 2: Write the failing test**

Create `v1alpha1/attach/attach_test.go`:

```go
package attach

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// fakeTarget stands in for a container. Every failure mode this package has to
// handle — no TTY, no stdin, a stream that ends — is a field here rather than a
// container somebody has to arrange, which is what makes them testable at all.
type fakeTarget struct {
	name   string
	tty    bool
	stdin  bool
	out    string        // written to stdout as soon as the attach begins
	seenIn chan string   // what arrived on stdin
	seenSz chan remotecommand.TerminalSize
	done   chan struct{} // closed when AttachContainer returns
}

func newFakeTarget(name string, tty, stdin bool) *fakeTarget {
	return &fakeTarget{
		name:   name,
		tty:    tty,
		stdin:  stdin,
		seenIn: make(chan string, 4),
		seenSz: make(chan remotecommand.TerminalSize, 4),
		done:   make(chan struct{}),
	}
}

func (f *fakeTarget) Name() string  { return f.name }
func (f *fakeTarget) TTY() bool     { return f.tty }
func (f *fakeTarget) Stdin() bool   { return f.stdin }
func (f *fakeTarget) Close() error  { return nil }

func (f *fakeTarget) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, _ io.WriteCloser, _ bool, resize <-chan remotecommand.TerminalSize) error {
	defer close(f.done)
	if f.out != "" {
		_, _ = io.WriteString(out, f.out)
	}
	go func() {
		for size := range resize {
			f.seenSz <- size
		}
	}()
	if in != nil {
		buf := make([]byte, 64)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				f.seenIn <- string(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}
	<-ctx.Done()
	return nil
}

// serveFake starts a Server on a fake target and tears it down with the test.
func serveFake(t *testing.T, target Target) *Server {
	t.Helper()
	s, err := Serve(t.Context(), target, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPage pins that the tunnel's own address answers with the terminal page
// and that nothing else on the origin answers at all. The origin exists to
// serve exactly two paths; anything else reaching it is a bug upstream, and a
// 404 says so instead of quietly returning the shell again.
func TestPage(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))

	cases := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{"the root serves the terminal", "/", http.StatusOK, "@xterm/xterm@"},
		{"the root names the container", "/", http.StatusOK, "api"},
		{"anything else is not found", "/favicon.ico", http.StatusNotFound, ""},
		{"a nested path is not found", "/app/index.html", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(s.URL().String() + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.status)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if tc.want != "" && !strings.Contains(string(body), tc.want) {
				t.Errorf("GET %s body does not contain %q", tc.path, tc.want)
			}
		})
	}
}

// TestURLIsLoopback pins that the origin never leaves the machine. The page it
// serves is unauthenticated by design; the only thing keeping it off the local
// network is the address it binds.
func TestURLIsLoopback(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))
	u := s.URL()
	if u.Scheme != "http" {
		t.Errorf("scheme = %q, want http", u.Scheme)
	}
	if !strings.HasPrefix(u.Host, "127.0.0.1:") {
		t.Errorf("host = %q, want a 127.0.0.1 port", u.Host)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./v1alpha1/attach/`
Expected: FAIL — `undefined: Serve`, `undefined: Server`, `undefined: Target`.

- [ ] **Step 4: Write `index.html`**

First compute the three integrity hashes:

```bash
cd "$(mktemp -d)"
curl -sO https://cdn.jsdelivr.net/npm/@xterm/xterm@6.0.0/css/xterm.css
curl -sO https://cdn.jsdelivr.net/npm/@xterm/xterm@6.0.0/lib/xterm.js
curl -sO https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.11.0/lib/addon-fit.js
for f in xterm.css xterm.js addon-fit.js; do
  printf '%-14s sha384-%s\n' "$f" "$(openssl dgst -sha384 -binary "$f" | openssl base64 -A)"
done
```

Create `v1alpha1/attach/index.html`, substituting the three hashes for `PASTE_*`:

```html
<!doctype html>
<html lang="en" class="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<meta name="robots" content="noindex, nofollow">
<title>{{.Name}} · attach</title>

<!-- xterm.js, pinned by version and checked by digest so a change at the CDN
     cannot silently alter what a tunnel serves. xterm 6 ships no .min. files;
     jsDelivr would minify on the fly for a .min.js URL, but the integrity hash
     has to match the bytes actually served, so these are the real paths. -->
<link rel="stylesheet"
      href="https://cdn.jsdelivr.net/npm/@xterm/xterm@6.0.0/css/xterm.css"
      integrity="sha384-PASTE_CSS"
      crossorigin="anonymous"
      referrerpolicy="no-referrer">
<script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@6.0.0/lib/xterm.js"
        integrity="sha384-PASTE_XTERM"
        crossorigin="anonymous"
        referrerpolicy="no-referrer"></script>
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.11.0/lib/addon-fit.js"
        integrity="sha384-PASTE_FIT"
        crossorigin="anonymous"
        referrerpolicy="no-referrer"></script>

<style>
  /* The terminal is the page. No chrome, no header, nothing to explain — what
     the container has to say is the only content, and anything framing it
     steals rows from a viewport that is often a phone. */
  html, body { height: 100%; margin: 0; background: #000; }
  #term { height: 100%; width: 100%; }
</style>
</head>
<body>
<div id="term"></div>
<script>
(() => {
  // The k8s remotecommand channels. The first byte of every binary frame says
  // which stream it belongs to, in both directions.
  const STDIN = 0, STDOUT = 1, STDERR = 2, ERROR = 3, RESIZE = 4;

  const term = new Terminal({ cursorBlink: true, convertEol: false, fontSize: 13 });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(document.getElementById('term'));
  fit.fit();

  // location.search is the tunnel's own bare routing parameter, carried over
  // verbatim: the page was served at /?1, so its socket goes to /attach?1 and
  // lands on the same origin. Leaning on the sticky origin cookie instead
  // would let a second tab's container steal this one's socket, and a
  // websocket handshake sends no Referer for the tunnel to route by.
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(proto + '//' + location.host + '/attach' + location.search,
                           ['v4.channel.k8s.io']);
  ws.binaryType = 'arraybuffer';

  const encoder = new TextEncoder();
  const decoder = new TextDecoder();

  const send = (channel, bytes) => {
    if (ws.readyState !== WebSocket.OPEN) return;
    const frame = new Uint8Array(bytes.length + 1);
    frame[0] = channel;
    frame.set(bytes, 1);
    ws.send(frame);
  };

  const sendSize = () => send(RESIZE, encoder.encode(
    JSON.stringify({ Width: term.cols, Height: term.rows })));

  ws.onmessage = (event) => {
    const frame = new Uint8Array(event.data);
    // A one-byte frame is the server saying the stream is established. There
    // is nothing to print, but it is the moment the terminal is really live,
    // so the first size goes out here rather than on open.
    if (frame.length < 2) {
      if (frame[0] === STDOUT) sendSize();
      return;
    }
    const body = frame.subarray(1);
    switch (frame[0]) {
      case STDOUT:
      case STDERR:
        term.write(body);
        break;
      case ERROR:
        // A v4 status: success carries no message worth showing, and a
        // failure is the only thing the reader can act on.
        try {
          const status = JSON.parse(decoder.decode(body));
          if (status.status !== 'Success') {
            term.write('\r\n\x1b[2m' + (status.message || 'attach failed') + '\x1b[0m\r\n');
          }
        } catch { /* not JSON; nothing useful to show */ }
        break;
    }
  };

  term.onData((data) => send(STDIN, encoder.encode(data)));

  new ResizeObserver(() => { fit.fit(); sendSize(); }).observe(document.body);

  // Cloudflare closes idle websockets and a page cannot send a websocket ping,
  // so the terminal's own size doubles as the heartbeat: resizing to the
  // dimensions already in effect is a no-op on the container.
  const beat = setInterval(sendSize, 30000);

  ws.onclose = () => {
    clearInterval(beat);
    term.write('\r\n\x1b[2mdetached\x1b[0m\r\n');
  };
})();
</script>
</body>
</html>
```

- [ ] **Step 5: Write `attach.go`**

Create `v1alpha1/attach/attach.go`:

```go
// Package attach serves a browser terminal for one attachable thing as an
// ordinary HTTP origin.
//
// A dockerd:// value names a container, which is not an HTTP service, so
// tunneld becomes one on its behalf: a Server binds a loopback listener,
// answers "/" with the xterm page and "/attach" with the Kubernetes
// remotecommand stream protocol, and hands back a URL that is registered as an
// origin like any other. Everything downstream — the bare ?n routing
// parameter, the multiview panel, the reported map — then treats a container
// exactly the way it treats a local web server.
//
// It is an implementation subpackage and knows nothing about Docker: the
// provider arrives as a Target. index.html travels with the code because
// go:embed cannot reach outside its own package directory.
package attach

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// pageHTML is the terminal page. Embedded rather than fetched, so a tunnel
// serves it with nothing else installed.
//
//go:embed index.html
var pageHTML string

// page is parsed once at init. A template that fails to parse is a build-time
// mistake in a file that ships inside the binary, so it panics here rather
// than surfacing as a 500 on somebody's first request.
var page = template.Must(template.New("attach").Parse(pageHTML))

// The window a socket may go quiet before it is reaped. The page beats every
// 30 seconds by re-sending its size, so anything approaching this is a page
// that has genuinely gone away — a laptop lid, a closed tab the browser never
// told us about.
const idleTimeout = 2 * time.Minute

// Target is one attachable thing behind a Server: it streams, it says what it
// can do, and it releases whatever it holds.
//
// Four methods is the whole provider contract, which is what keeps this
// package free of Docker. A second provider — podman, or a local shell over a
// pty — implements these and nothing here changes.
type Target interface {
	// AttachContainer streams between the caller's ends and the target's
	// stdio, returning when the target's stream ends. The name, uid and
	// container arguments are Kubernetes' shape, which ServeAttach fills in
	// from what it is given; a single-target provider ignores them.
	remotecommand.Attacher
	// Name is the reference the operator typed, used to title the page.
	Name() string
	// TTY reports whether the target's stdout is a terminal. It decides
	// whether resize means anything and whether stderr is a stream of its own.
	TTY() bool
	// Stdin reports whether the target will read anything written to it.
	Stdin() bool
	// Close releases the target. A Server closes its own on shutdown.
	Close() error
}

// Server is the loopback HTTP origin standing in for one Target.
type Server struct {
	target   Target
	listener net.Listener
	srv      *http.Server
	log      *slog.Logger
}

// Serve binds a loopback listener and starts serving the terminal on it,
// returning as soon as the port is live so the tunnel never proxies to a
// socket that is not up yet.
//
// The address is 127.0.0.1 rather than a wildcard, deliberately. The page is
// unauthenticated by design — the tunnel hostname is the secret — so the local
// network is not somewhere it belongs.
//
// The Server takes ownership of target: Close closes both.
func Serve(ctx context.Context, target Target, log *slog.Logger) (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("attach %s: listen: %w", target.Name(), err)
	}

	s := &Server{target: target, listener: listener, log: log}

	mux := http.NewServeMux()
	// "GET /{$}" is the root exactly, not a prefix — an origin's stray request
	// gets a 404 rather than the shell a second time. GET also answers HEAD,
	// which is what the reachability probe sends.
	mux.HandleFunc("GET /{$}", s.servePage)
	mux.HandleFunc("GET /attach", s.serveAttach)

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: an attached terminal is a stream that may say
		// nothing for hours and is still perfectly healthy. idleTimeout on the
		// websocket is what reaps a dead one.
	}
	context.AfterFunc(ctx, func() { _ = s.Close() })

	go func() {
		if err := s.srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Error("attach origin failed", "container", target.Name(), "error", err)
		}
	}()
	return s, nil
}

// URL is the loopback address the tunnel proxies to.
func (s *Server) URL() *url.URL {
	return &url.URL{Scheme: "http", Host: s.listener.Addr().String()}
}

// Close stops the origin and releases the target. It is safe to call twice,
// which it is: once from the context and once from the caller's defer.
func (s *Server) Close() error {
	err := s.srv.Close()
	if terr := s.target.Close(); err == nil {
		err = terr
	}
	return err
}

// servePage renders the terminal page. A render failure is logged and answered
// with a plain error rather than a half-written page.
func (s *Server) servePage(w http.ResponseWriter, _ *http.Request) {
	var rendered strings.Builder
	if err := page.Execute(&rendered, struct{ Name string }{s.target.Name()}); err != nil {
		s.log.Error("attach render failed", "container", s.target.Name(), "error", err)
		http.Error(w, "attach: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is a live view of a running container.
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.WriteString(w, rendered.String()); err != nil {
		s.log.Debug("attach write failed", "error", err) // visitor went away
	}
}

// serveAttach hands the request to ServeAttach, which owns the websocket
// upgrade and the v4.channel.k8s.io framing on it.
func (s *Server) serveAttach(w http.ResponseWriter, r *http.Request) {
	name := s.target.Name()
	opts := &remotecommand.Options{
		Stdin:  s.target.Stdin(),
		Stdout: true,
		// A TTY merges stdout and stderr at the source, so a second stream
		// would only ever be empty — and Kubernetes' own clients treat a
		// TTY session with a stderr channel as malformed.
		Stderr: !s.target.TTY(),
		TTY:    s.target.TTY(),
	}
	remotecommand.ServeAttach(w, r, notice{s.target}, name, "", name, opts,
		idleTimeout, remotecommand.DefaultStreamCreationTimeout,
		remotecommand.SupportedStreamingProtocols)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./v1alpha1/attach/ -v`
Expected: FAIL to compile — `undefined: notice`. That type arrives in Task 3; for this step, temporarily replace `notice{s.target}` with `s.target` so the page tests can run, then restore it in Task 3 Step 4.

Run again: `go test ./v1alpha1/attach/ -v`
Expected: PASS — `TestPage` (4 subtests) and `TestURLIsLoopback`.

- [ ] **Step 7: Commit**

```bash
git add v1alpha1/attach/ go.mod go.sum
git commit -m "feat: a loopback origin that serves an xterm terminal

The Target seam is four methods, which is what keeps this package free of
Docker: a second provider implements them and nothing here changes. The
page routes its own socket with location.search rather than leaning on the
sticky origin cookie, because a websocket handshake carries no Referer and
a cookie is last-write-wins across tabs.

Refs #12."
```

---

### Task 3: The stream, and the notice a degraded container earns

`/attach` starts speaking. The `notice` decorator writes one dim line for a container started without `-t` or `-i`, so a terminal that cannot echo says why instead of looking broken.

**Files:**
- Modify: `v1alpha1/attach/attach.go` (add `notice` and `degraded`)
- Test: `v1alpha1/attach/attach_test.go`

**Interfaces:**
- Consumes: `attach.Target`, `attach.Serve`, `(*Server).URL` from Task 2.
- Produces: `notice` (unexported struct embedding `Target`), `degraded(Target) string` (unexported). Nothing outside the package depends on either.

- [ ] **Step 1: Write the failing tests**

Append to `v1alpha1/attach/attach_test.go`:

```go
// dial opens a v4.channel.k8s.io socket to a Server and closes it with the
// test. The subprotocol is what selects binary channel framing; without it the
// server negotiates the base64 variant and every assertion below shifts.
func dial(t *testing.T, s *Server) *websocket.Conn {
	t.Helper()
	d := websocket.Dialer{Subprotocols: []string{"v4.channel.k8s.io"}}
	c, resp, err := d.Dial("ws://"+s.URL().Host+"/attach", nil)
	if err != nil {
		t.Fatalf("dial /attach: %v", err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readFrame reads one binary frame and splits it into channel and payload.
func readFrame(t *testing.T, c *websocket.Conn) (byte, []byte) {
	t.Helper()
	kind, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if kind != websocket.BinaryMessage {
		t.Fatalf("frame type = %d, want binary", kind)
	}
	if len(data) == 0 {
		t.Fatal("frame is empty, want at least a channel byte")
	}
	return data[0], data[1:]
}

// writeFrame sends one channel-prefixed binary frame.
func writeFrame(t *testing.T, c *websocket.Conn, channel byte, payload string) {
	t.Helper()
	if err := c.WriteMessage(websocket.BinaryMessage, append([]byte{channel}, payload...)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// TestEstablishedFrame pins the one-byte frame the server sends on connect.
// The page keys "the terminal is live" on it — it is the only signal that the
// attach actually began, since a healthy container may say nothing for hours.
func TestEstablishedFrame(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))
	channel, payload := readFrame(t, dial(t, s))
	if channel != 1 {
		t.Errorf("established frame channel = %d, want 1 (stdout)", channel)
	}
	if len(payload) != 0 {
		t.Errorf("established frame payload = %q, want empty", payload)
	}
}

// TestStdout pins that what the target writes reaches the browser on the
// stdout channel, unaltered.
func TestStdout(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "hello from pid 1\r\n"
	c := dial(t, serveFake(t, target))

	readFrame(t, c) // the established frame
	channel, payload := readFrame(t, c)
	if channel != 1 {
		t.Errorf("channel = %d, want 1 (stdout)", channel)
	}
	if string(payload) != target.out {
		t.Errorf("payload = %q, want %q", payload, target.out)
	}
}

// TestStdin pins that keystrokes reach the target.
func TestStdin(t *testing.T) {
	target := newFakeTarget("api", true, true)
	c := dial(t, serveFake(t, target))
	readFrame(t, c) // the established frame

	writeFrame(t, c, 0, "echo hi\n")
	select {
	case got := <-target.seenIn:
		if got != "echo hi\n" {
			t.Errorf("target read %q, want %q", got, "echo hi\n")
		}
	case <-t.Context().Done():
		t.Fatal("target never saw stdin")
	}
}

// TestResize pins the resize wire format. It is JSON on a channel of its own,
// read by a streaming decoder, so successive sizes need no framing — which is
// what lets the page use it as a heartbeat.
func TestResize(t *testing.T) {
	target := newFakeTarget("api", true, true)
	c := dial(t, serveFake(t, target))
	readFrame(t, c) // the established frame

	writeFrame(t, c, 4, `{"Width":100,"Height":40}`)
	writeFrame(t, c, 4, `{"Width":120,"Height":50}`)

	want := []remotecommand.TerminalSize{{Width: 100, Height: 40}, {Width: 120, Height: 50}}
	for _, w := range want {
		select {
		case got := <-target.seenSz:
			if got != w {
				t.Errorf("size = %+v, want %+v", got, w)
			}
		case <-t.Context().Done():
			t.Fatalf("target never saw %+v", w)
		}
	}
}

// TestDegradedNotice pins the line a container earns by having been started
// without -t or -i. Half of docker attach's behaviour is decided before
// tunneld is involved, and a terminal that silently swallows keystrokes is the
// one outcome worth spending a line to prevent.
func TestDegradedNotice(t *testing.T) {
	cases := []struct {
		name  string
		tty   bool
		stdin bool
		want  string
	}{
		{"a full terminal says nothing", true, true, ""},
		{"no tty", false, true, "no TTY"},
		{"no stdin", true, false, "stdin"},
		{"neither", false, false, "output only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := degraded(newFakeTarget("api", tc.tty, tc.stdin))
			if tc.want == "" {
				if got != "" {
					t.Errorf("degraded = %q, want no notice", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("degraded = %q, does not mention %q", got, tc.want)
			}
		})
	}
}

// TestNoticeReachesTheTerminal pins that the notice is written to the stream
// rather than baked into the page, so it appears in order with the container's
// own first output and survives however the page was loaded.
func TestNoticeReachesTheTerminal(t *testing.T) {
	target := newFakeTarget("api", false, false)
	c := dial(t, serveFake(t, target))
	readFrame(t, c) // the established frame

	channel, payload := readFrame(t, c)
	if channel != 1 {
		t.Errorf("channel = %d, want 1 (stdout)", channel)
	}
	if !strings.Contains(string(payload), "output only") {
		t.Errorf("first frame = %q, want the degraded notice", payload)
	}
}
```

Add `"github.com/gorilla/websocket"` to the test file's import block.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./v1alpha1/attach/ -run 'Established|Stdout|Stdin|Resize|Degraded|Notice'`
Expected: FAIL — `undefined: degraded`.

- [ ] **Step 3: Write `notice` and `degraded`**

Append to `v1alpha1/attach/attach.go`:

```go
// notice wraps a Target to write one dim line about what the container cannot
// do, ahead of anything the container itself says.
//
// It goes on the stream rather than into the page for two reasons: it then
// appears in order with the container's first output instead of above it, and
// it survives however the page was reached — a reload, a second tab, a client
// that is not this page at all.
type notice struct{ Target }

func (n notice) AttachContainer(ctx context.Context, name, uid, container string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	if line := degraded(n.Target); line != "" {
		// CRLF, not LF: a raw terminal does not move the carriage on its own.
		fmt.Fprintf(out, "\x1b[2m%s\x1b[0m\r\n", line)
	}
	return n.Target.AttachContainer(ctx, name, uid, container, in, out, errw, tty, resize)
}

// degraded names what a container was started without, or "" when it was
// started with both -t and -i and there is nothing to explain.
//
// The wording names the docker run flag rather than the symptom, because that
// is the lever: nothing tunneld can do fixes a container already running
// without a TTY, and the reader's next move is to restart it.
func degraded(t Target) string {
	switch {
	case !t.TTY() && !t.Stdin():
		return "no TTY and no stdin (started without -it) — output only"
	case !t.TTY():
		return "no TTY (started without -t) — no line editing, no resize"
	case !t.Stdin():
		return "stdin closed (started without -i) — keystrokes go nowhere"
	}
	return ""
}
```

- [ ] **Step 4: Restore the `notice` wrapper in `serveAttach`**

In `serveAttach`, change the third argument back from `s.target` to `notice{s.target}` (undoing the temporary edit from Task 2 Step 6).

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./v1alpha1/attach/ -v`
Expected: PASS — every test in the file.

Run: `go test -race ./v1alpha1/attach/`
Expected: PASS. The stream has three goroutines per connection; the race detector is the lane CI gates on.

- [ ] **Step 6: Mutation-check the routing guard**

Temporarily delete ` + location.search` from the websocket URL in `index.html`, then run `go test ./v1alpha1/attach/`. It still passes — the tests dial `/attach` directly and never load the page — so record the gap rather than pretending otherwise: the `location.search` suffix is covered by the two-origin case in Task 5, not here. Restore the suffix.

- [ ] **Step 7: Commit**

```bash
git add v1alpha1/attach/
git commit -m "feat: stream the container over v4.channel.k8s.io

ServeAttach owns the upgrade and the framing; this adds the one thing it
cannot know, which is that a container started without -t or -i has a
terminal that cannot echo. The notice goes on the stream rather than into
the page so it lands in order with the container's own first output.

Refs #12."
```

---

### Task 4: The Docker provider

`docker.Attacher` implements `attach.Target` over the SDK: inspect up front so failures happen before the tunnel is minted, then attach, demux, and resize.

**Files:**
- Create: `v1alpha1/attach/docker/docker.go`
- Test: `v1alpha1/attach/docker/docker_test.go`

**Interfaces:**
- Consumes: `attach.Target` (satisfied, not imported — the interface is structural), `v1.ErrNoDocker` and `v1.ErrInvalidOrigin` from Task 1.
- Produces: `docker.Open(ctx context.Context, ref string, log *slog.Logger) (*Attacher, error)`, and `*Attacher` with `Name() string`, `TTY() bool`, `Stdin() bool`, `Close() error`, `AttachContainer(...)`.

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/docker/docker@v28.5.2+incompatible
go mod tidy
```

- [ ] **Step 2: Write the failing test**

Create `v1alpha1/attach/docker/docker_test.go`:

```go
package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// discard is the logger every test here uses: these paths log warnings that
// are not what is under test.
func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestOpenWithoutDaemon pins the failure an operator hits most: Docker is not
// running. It needs no daemon of its own — pointing DOCKER_HOST at a port
// nothing listens on reproduces exactly the connection failure a stopped
// daemon produces.
func TestOpenWithoutDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	got, err := Open(ctx, "api", discard())
	if err == nil {
		_ = got.Close()
		t.Fatal("Open with no daemon succeeded, want an error")
	}
	if !errors.Is(err, v1.ErrNoDocker) {
		t.Errorf("error = %v, want %v", err, v1.ErrNoDocker)
	}
}

// withDaemon skips unless a Docker daemon is reachable and the alpine image is
// already local. Pulling would make this lane depend on the network and on a
// registry, which is exactly what the rest of the suite avoids.
func withDaemon(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	if _, err := cli.Ping(t.Context()); err != nil {
		_ = cli.Close()
		t.Skipf("no docker daemon: %v", err)
	}
	if _, err := cli.ImageInspect(t.Context(), "alpine"); err != nil {
		_ = cli.Close()
		t.Skip("alpine image not present locally; run: docker pull alpine")
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// startContainer runs an alpine shell and removes it with the test. tty and
// stdin mirror docker run's -t and -i, which is what decides everything the
// attach can do.
func startContainer(t *testing.T, cli *client.Client, tty, stdin bool) string {
	t.Helper()
	created, err := cli.ContainerCreate(t.Context(),
		&container.Config{
			Image:     "alpine",
			Cmd:       []string{"sh"},
			Tty:       tty,
			OpenStdin: stdin,
		}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.WithoutCancel(t.Context()), created.ID,
			container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(t.Context(), created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	return created.ID
}

// TestOpenRejects pins the two failures that must happen before the tunnel is
// minted: a container that does not exist, and one that is not running. Both
// are the operator's typo or stale assumption, and a public hostname that
// answers only errors is a worse way to learn it.
func TestOpenRejects(t *testing.T) {
	cli := withDaemon(t)

	t.Run("no such container", func(t *testing.T) {
		got, err := Open(t.Context(), "tunneld-test-nonexistent", discard())
		if err == nil {
			_ = got.Close()
			t.Fatal("Open succeeded, want an error")
		}
		if !errors.Is(err, v1.ErrInvalidOrigin) {
			t.Errorf("error = %v, want %v", err, v1.ErrInvalidOrigin)
		}
		if !strings.Contains(err.Error(), "tunneld-test-nonexistent") {
			t.Errorf("error %q does not name the container", err)
		}
	})

	t.Run("container not running", func(t *testing.T) {
		id := startContainer(t, cli, true, true)
		if err := cli.ContainerStop(t.Context(), id, container.StopOptions{}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		got, err := Open(t.Context(), id, discard())
		if err == nil {
			_ = got.Close()
			t.Fatal("Open on a stopped container succeeded, want an error")
		}
		if !errors.Is(err, v1.ErrInvalidOrigin) {
			t.Errorf("error = %v, want %v", err, v1.ErrInvalidOrigin)
		}
	})
}

// TestAttach pins the round trip against a real container, both ways it can be
// started. The TTY case copies straight through; the non-TTY case has to be
// demultiplexed, and getting that backwards produces output with 8-byte
// headers embedded in it rather than an error.
func TestAttach(t *testing.T) {
	cli := withDaemon(t)

	cases := []struct {
		name  string
		tty   bool
		stdin bool
	}{
		{"started with -it", true, true},
		{"started with -i, no tty", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := startContainer(t, cli, tc.tty, tc.stdin)
			a, err := Open(t.Context(), id, discard())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer a.Close()

			if a.TTY() != tc.tty {
				t.Errorf("TTY() = %v, want %v", a.TTY(), tc.tty)
			}
			if a.Stdin() != tc.stdin {
				t.Errorf("Stdin() = %v, want %v", a.Stdin(), tc.stdin)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			stdinR, stdinW := io.Pipe()
			out := &syncBuffer{}
			resize := make(chan remotecommand.TerminalSize)
			done := make(chan error, 1)
			go func() {
				done <- a.AttachContainer(ctx, id, "", id, stdinR, nopCloser{out}, nopCloser{out}, tc.tty, resize)
			}()

			if _, err := io.WriteString(stdinW, "echo tunneld-marker\n"); err != nil {
				t.Fatalf("write stdin: %v", err)
			}

			deadline := time.After(20 * time.Second)
			for !strings.Contains(out.String(), "tunneld-marker") {
				select {
				case <-deadline:
					t.Fatalf("never saw the marker; got %q", out.String())
				case err := <-done:
					t.Fatalf("attach ended early: %v (output %q)", err, out.String())
				case <-time.After(200 * time.Millisecond):
				}
			}

			cancel()
			_ = stdinW.Close()
			<-done
		})
	}
}
```

Also add the two small helpers the test uses, at the bottom of the same file:

```go
// syncBuffer is a bytes.Buffer the attach goroutine writes while the test
// reads. Without the mutex the race detector fails the whole lane.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// nopCloser adapts a writer to the io.WriteCloser the Attacher contract takes.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
```

with `"bytes"` and `"sync"` added to the imports.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./v1alpha1/attach/docker/`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 4: Write `docker.go`**

Create `v1alpha1/attach/docker/docker.go`:

```go
// Package docker attaches to a running container over the Docker Engine API,
// implementing the Target the attach package serves.
//
// It knows nothing about HTTP or websockets: it is handed the four streams and
// a resize channel, and it copies. The split is what keeps a second provider —
// podman, or a local shell over a pty — from having to touch the server.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Attacher is one container, resolved and inspected.
type Attacher struct {
	cli   *client.Client
	log   *slog.Logger
	id    string
	ref   string
	tty   bool
	stdin bool
}

// Open resolves ref — a container name or id — against the daemon named by the
// environment ($DOCKER_HOST and friends) and inspects it.
//
// Everything that can fail about a container origin fails here, before the
// tunnel is minted. A public hostname that answers only errors is a far worse
// way to learn that a name is misspelled, and by the time the tunnel is up the
// operator has already been handed a URL to share.
//
// The three failures are told apart because their levers differ: a daemon that
// cannot be reached is ErrNoDocker (start Docker), and both a missing container
// and a stopped one are ErrInvalidOrigin (fix the --url, or start it).
func Open(ctx context.Context, ref string, log *slog.Logger) (*Attacher, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", v1.ErrNoDocker, err)
	}

	info, err := cli.ContainerInspect(ctx, ref)
	if err != nil {
		_ = cli.Close()
		switch {
		case client.IsErrNotFound(err):
			return nil, fmt.Errorf("%w: no container named %q", v1.ErrInvalidOrigin, ref)
		case client.IsErrConnectionFailed(err):
			return nil, fmt.Errorf("%w: %w", v1.ErrNoDocker, err)
		default:
			// Anything else — a permission denial on the socket, an API
			// version the daemon refuses — is still the daemon's answer, not
			// this container's, so it reads as a daemon problem.
			return nil, fmt.Errorf("%w: inspecting %q: %w", v1.ErrNoDocker, ref, err)
		}
	}
	if info.State == nil || !info.State.Running {
		_ = cli.Close()
		return nil, fmt.Errorf("%w: container %q is not running", v1.ErrInvalidOrigin, ref)
	}

	return &Attacher{
		cli:   cli,
		log:   log,
		id:    info.ID,
		ref:   ref,
		tty:   info.Config.Tty,
		stdin: info.Config.OpenStdin,
	}, nil
}

// Name is the reference the operator typed, not the resolved id: it is what
// they will recognize in a page title and a log line.
func (a *Attacher) Name() string { return a.ref }

// TTY reports Config.Tty — whether the container was started with -t. It is
// fixed at docker run time and nothing here can change it.
func (a *Attacher) TTY() bool { return a.tty }

// Stdin reports Config.OpenStdin — whether the container was started with -i.
func (a *Attacher) Stdin() bool { return a.stdin }

// Close releases the API client.
func (a *Attacher) Close() error { return a.cli.Close() }

// AttachContainer attaches to PID 1 and copies until the stream ends or ctx is
// canceled, which is what `docker attach` does. The Kubernetes-shaped name,
// uid and container arguments are ignored: this Attacher is one container by
// construction.
//
// Logs is on, which is the one place this departs from `docker attach`: the
// backlog replays so that opening the URL shows what the container has been
// printing. An empty terminal on a quiet container is indistinguishable from a
// broken one, and this page is usually opened long after the container started.
func (a *Attacher) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	resp, err := a.cli.ContainerAttach(ctx, a.id, container.AttachOptions{
		Stream: true,
		Stdin:  a.stdin,
		Stdout: true,
		Stderr: true,
		Logs:   true,
	})
	if err != nil {
		return fmt.Errorf("attach %s: %w", a.ref, err)
	}
	defer resp.Close()

	go a.watchResize(ctx, resize)

	if a.stdin && in != nil {
		go func() {
			_, _ = io.Copy(resp.Conn, in)
			// Half-close, so a container reading to EOF sees one. Closing the
			// whole connection here would cut its output off mid-sentence.
			_ = resp.CloseWrite()
		}()
	}

	// With a TTY the container merged the two streams itself and the bytes are
	// raw. Without one they arrive stdcopy-framed — an 8-byte header per chunk
	// carrying stream id and length — and demultiplexing is what puts the
	// container's stderr on a channel of its own instead of printing the
	// headers into somebody's terminal.
	if tty {
		_, err = io.Copy(out, resp.Reader)
	} else {
		_, err = stdcopy.StdCopy(out, errw, resp.Reader)
	}

	// PID 1 exiting, the visitor leaving, and the tunnel shutting down all
	// arrive here as a closed pipe. None of them is a failure worth reporting:
	// the stream ending is the normal end of an attach.
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		err = nil
	}
	return err
}

// watchResize forwards terminal sizes to the container, and returns when the
// channel closes — which ServeAttach does when the socket ends.
//
// Without a TTY there is nothing to resize, but the channel is still drained:
// the page sends its size as a heartbeat regardless, and a blocked send would
// stall the whole stream.
func (a *Attacher) watchResize(ctx context.Context, resize <-chan remotecommand.TerminalSize) {
	for size := range resize {
		if !a.tty || size.Width == 0 || size.Height == 0 {
			continue
		}
		err := a.cli.ContainerResize(ctx, a.id, container.ResizeOptions{
			Height: uint(size.Height),
			Width:  uint(size.Width),
		})
		if err != nil && ctx.Err() == nil {
			a.log.Debug("could not resize container", "container", a.ref, "error", err)
		}
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./v1alpha1/attach/docker/ -v`
Expected: PASS. `TestOpenWithoutDaemon` runs everywhere. `TestOpenRejects` and `TestAttach` run where a daemon and a local `alpine` image exist, and skip with a named reason otherwise.

Run: `docker pull alpine && go test -race ./v1alpha1/attach/docker/ -v`
Expected: PASS with all subtests actually running, not skipped. Confirm the skip messages are absent from the output — a plan that only ever skips its real test has not tested anything.

- [ ] **Step 6: Mutation-check the demux**

Change `if tty {` to `if true {` so the non-TTY container takes the raw copy path. Run `go test ./v1alpha1/attach/docker/ -run TestAttach`. Expected: the `started with -i, no tty` subtest fails, with output containing the stdcopy header bytes around the marker. Restore the condition and confirm it passes again.

- [ ] **Step 7: Commit**

```bash
git add v1alpha1/attach/docker/ go.mod go.sum
git commit -m "feat: attach to a container over the docker engine API

Inspect up front so a misspelled name or a stopped container fails before
the mint rather than as a public hostname that answers only errors. The
three failures are told apart by their levers: ErrNoDocker means start
Docker, ErrInvalidOrigin means fix the --url.

Logs is on, the one departure from docker attach: the backlog replays so
opening the URL shows what the container has been printing.

Refs #12."
```

---

### Task 5: Wire it into the tunnel

The origin list splits. `bindOrigins` stands up a loopback server for every `dockerd://` entry and returns the list libtunnel dials, while everything human keeps reading the list the operator typed.

**Files:**
- Create: `v1alpha1/origins.go`
- Modify: `v1alpha1/tunnel.go` (`run`)
- Modify: `v1alpha1/multiview/multiview.go` (tile label)
- Test: `v1alpha1/origins_test.go`, `v1alpha1/multiview/multiview_test.go`

**Interfaces:**
- Consumes: `attach.Target`, `attach.Serve`, `(*Server).URL` (Task 2); `docker.Open` (Task 4); `v1.DockerScheme` (Task 1).
- Produces: `bindOrigins(ctx context.Context, display []*url.URL, log *slog.Logger) (*bound, error)` and `*bound` with fields `dialable []*url.URL` and method `Close() error`, plus the package variable `openTarget` that tests substitute. `multiview.label(*url.URL) string` (unexported).

- [ ] **Step 1: Write the failing tests**

Create `v1alpha1/origins_test.go`:

```go
package v1alpha1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// stubTarget is a container that never says anything. bindOrigins only needs a
// Target to stand a server on; what the stream does is attach's business and
// is tested there.
type stubTarget struct {
	name   string
	closed bool
}

func (s *stubTarget) Name() string { return s.name }
func (s *stubTarget) TTY() bool    { return true }
func (s *stubTarget) Stdin() bool  { return true }
func (s *stubTarget) Close() error { s.closed = true; return nil }
func (s *stubTarget) AttachContainer(ctx context.Context, _, _, _ string, _ io.Reader, _, _ io.WriteCloser, _ bool, _ <-chan remotecommand.TerminalSize) error {
	<-ctx.Done()
	return nil
}

// withStubOpener swaps the docker opener for the duration of a test and
// records every reference it was asked for.
func withStubOpener(t *testing.T, err error) *[]string {
	t.Helper()
	var asked []string
	original := openTarget
	openTarget = func(_ context.Context, ref string, _ *slog.Logger) (attach.Target, error) {
		asked = append(asked, ref)
		if err != nil {
			return nil, err
		}
		return &stubTarget{name: ref}, nil
	}
	t.Cleanup(func() { openTarget = original })
	return &asked
}

func mustURLs(t *testing.T, raw ...string) []*url.URL {
	t.Helper()
	got, err := parseOrigins(raw)
	if err != nil {
		t.Fatalf("parseOrigins(%q): %v", raw, err)
	}
	return got
}

// TestBindOriginsKeepsOrder pins the invariant the whole feature rests on: the
// dialable list is the same length and the same order as what the operator
// typed, so index n still means origin n everywhere downstream — ?n routing,
// PublicURL, the reported map, the multiview tiles.
func TestBindOriginsKeepsOrder(t *testing.T) {
	asked := withStubOpener(t, nil)
	display := mustURLs(t, "http://localhost:3000", "dockerd://api", "http://localhost:4000", "dockerd://db")

	bound, err := bindOrigins(t.Context(), display, slog.New(slog.DiscardHandler))
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
	if want := []string{"api", "db"}; len(*asked) != 2 || (*asked)[0] != want[0] || (*asked)[1] != want[1] {
		t.Errorf("opened %q, want %q", *asked, want)
	}
}

// TestBindOriginsWithoutContainers pins that a command with no dockerd:// URL
// starts nothing at all — the feature is inert until somebody asks for it.
func TestBindOriginsWithoutContainers(t *testing.T) {
	asked := withStubOpener(t, nil)
	display := mustURLs(t, "http://localhost:3000", "http://localhost:4000")

	bound, err := bindOrigins(t.Context(), display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("bindOrigins: %v", err)
	}
	defer bound.Close()

	if len(*asked) != 0 {
		t.Errorf("opened %q, want nothing", *asked)
	}
	for i, u := range bound.dialable {
		if u != display[i] {
			t.Errorf("dialable[%d] = %q, want the original origin", i, u)
		}
	}
}

// TestBindOriginsUnwindsOnFailure pins that a later container failing does not
// leave an earlier one's server listening. The command is about to return an
// error and exit; a leaked goroutine holding a port would outlive it in an
// embedding program.
func TestBindOriginsUnwindsOnFailure(t *testing.T) {
	var opened []*stubTarget
	original := openTarget
	calls := 0
	openTarget = func(_ context.Context, ref string, _ *slog.Logger) (attach.Target, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("no such container")
		}
		s := &stubTarget{name: ref}
		opened = append(opened, s)
		return s, nil
	}
	t.Cleanup(func() { openTarget = original })

	display := mustURLs(t, "dockerd://api", "dockerd://missing")
	if _, err := bindOrigins(t.Context(), display, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("bindOrigins succeeded, want an error")
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d targets, want 1", len(opened))
	}
	if !opened[0].closed {
		t.Error("the first container's target was left open")
	}
}
```

Append to `v1alpha1/multiview/multiview_test.go`:

```go
// TestLabel pins how a tile names the origin behind it. An http origin is
// named by its host, which is what the operator typed; anything else keeps its
// scheme, so a container tile cannot be misread as a hostname.
func TestLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://localhost:3000", "localhost:3000"},
		{"https://127.0.0.1:8443", "127.0.0.1:8443"},
		{"dockerd://api", "dockerd://api"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			u, err := url.Parse(tc.in)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tc.in, err)
			}
			if got := label(u); got != tc.want {
				t.Errorf("label(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./v1alpha1/... -run 'BindOrigins|Label'`
Expected: FAIL — `undefined: bindOrigins`, `undefined: openTarget`, `undefined: label`.

- [ ] **Step 3: Write `origins.go`**

Create `v1alpha1/origins.go`:

```go
package v1alpha1

import (
	"context"
	"io"
	"log/slog"
	"net/url"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/docker"
)

// openTarget resolves a container reference. A variable, not a direct call, so
// a test can bind origins without a Docker daemon — the ordering invariant
// below is the part worth pinning, and it has nothing to do with containers.
var openTarget = func(ctx context.Context, ref string, log *slog.Logger) (attach.Target, error) {
	return docker.Open(ctx, ref, log)
}

// bound is the result of binding: the list libtunnel dials, and the servers
// that have to be shut down with it.
type bound struct {
	dialable []*url.URL
	closers  []io.Closer
}

// bindOrigins turns the origins the operator typed into the origins libtunnel
// can proxy to.
//
// An http or https origin passes through untouched; a dockerd:// origin is
// served here, by a loopback attach server that takes its place in the list.
// The two lists share a length and an order, which is the whole point: index n
// still means origin n for the bare ?n routing parameter, for PublicURL, for
// the reported map and for the multiview tiles, so a container is an origin
// like any other and nothing downstream learns a second shape.
//
// A failure unwinds everything already bound. The command is about to return
// an error, and a listener left behind would outlive it inside an embedding
// program.
func bindOrigins(ctx context.Context, display []*url.URL, log *slog.Logger) (*bound, error) {
	b := &bound{dialable: make([]*url.URL, 0, len(display))}
	for _, origin := range display {
		if origin.Scheme != v1.DockerScheme {
			b.dialable = append(b.dialable, origin)
			continue
		}

		target, err := openTarget(ctx, origin.Host, log)
		if err != nil {
			_ = b.Close()
			return nil, err
		}
		server, err := attach.Serve(ctx, target, log)
		if err != nil {
			_ = target.Close()
			_ = b.Close()
			return nil, err
		}
		b.closers = append(b.closers, server)
		b.dialable = append(b.dialable, server.URL())
		log.Info("serving a container as an origin", "container", origin.Host, "origin", server.URL())
	}
	return b, nil
}

// Close shuts every attach server down, and with it every container client
// they own. The first error is returned and the rest still close: a partial
// shutdown is worse than a lost error message.
func (b *bound) Close() error {
	var err error
	for _, closer := range b.closers {
		if cerr := closer.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
```

- [ ] **Step 4: Wire it into `run`**

In `v1alpha1/tunnel.go`, in `run`, insert after the `logger()` block and before `backend := libtunnel.Cloudflare()`:

```go
	// A container is not an HTTP service, so tunneld serves one on its behalf
	// and hands the tunnel the loopback address instead. origins stays what
	// the operator typed — it is what the reported map and the panel show.
	bound, err := bindOrigins(ctx, origins, log)
	if err != nil {
		return err
	}
	defer bound.Close()
```

Then change the `WithLocalURL` argument on the tunnel construction from `origins...` to `bound.dialable...`:

```go
	tun := libtunnel.New(backend).
		WithLogger(log).
		WithContext(ctx).
		WithLocalURL(bound.dialable...)
```

Nothing else in `run` changes: `multiview.Wanted(b.multiview, origins)`, `multiview.Panel(origins, log)` and `report(stdout, stderr, public, origins, view)` all keep taking `origins`, which is what makes `dockerd://api` the thing an operator sees.

- [ ] **Step 5: Add the multiview label**

In `v1alpha1/multiview/multiview.go`, in `serveShell`, change the `Local` field:

```go
			Local: label(origin),
```

and add this function at the end of the file:

```go
// label is how a tile names the origin behind it. An http origin is named by
// its host, because the scheme is the assumption and the host is the thing the
// operator typed. Anything else keeps its scheme, so a tile framing a
// container reads as dockerd://api rather than as a bare hostname that happens
// to be a container name.
func label(u *url.URL) string {
	if u.Scheme == "http" || u.Scheme == "https" {
		return u.Host
	}
	return u.Scheme + "://" + u.Host
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./... `
Expected: PASS across every package, including the pre-existing multiview and tunnel tests.

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 7: Verify against a real container and a real tunnel**

This is the step that proves the feature rather than the code. Two origins, so the `location.search` routing guard is actually exercised:

```bash
docker run -d --rm --name tunneld-demo -it alpine sh
python3 -m http.server 3000 &
go run . --url http://localhost:3000 --url dockerd://tunneld-demo --log-level info
```

Confirm, in order:

1. stderr reports one address with both origins indented beneath it, and the container line reads `-> dockerd://tunneld-demo` — **not** a `127.0.0.1` port.
2. The panel opens and the second tile is a terminal, labelled `dockerd://tunneld-demo`.
3. Typing in that tile echoes; `ls` lists the container's root.
4. The first tile still shows the Python listing — the terminal did not steal its routing.
5. Open `https://<host>/?1` top-level in a second tab. It is a terminal, and going back to the first tab and typing still works — the two sockets did not fight over the cookie.
6. `docker stop tunneld-demo` from another shell: the tile prints `detached`.

Then `kill %1` and `docker rm -f tunneld-demo`. Record anything that did not behave as listed rather than moving on.

- [ ] **Step 8: Commit**

```bash
git add v1alpha1/origins.go v1alpha1/origins_test.go v1alpha1/tunnel.go v1alpha1/multiview/
git commit -m "feat: serve dockerd:// origins behind the tunnel

The origin list splits in two, same length and same order: libtunnel dials
the loopback attach servers, and everything human keeps reading what the
operator typed. That is what lets ?n routing, PublicURL, the reported map
and the multiview tiles stay exactly as they were.

A tile labels a container origin with its scheme, so it cannot be misread
as a hostname that happens to match a container name.

Refs #12."
```

---

### Task 6: The example, the e2e row, and the documentation

The feature becomes discoverable: an example that seeds a container origin, the e2e row the convention requires, and the README and CONTRIBUTING entries.

**Files:**
- Create: `examples/attach/main.go`
- Modify: `e2e/e2e_test.go` (the `TestExamples` table)
- Modify: `v1alpha1/builder.go` (the `Long` description and the `--url` usage string)
- Modify: `README.md`
- Modify: `CONTRIBUTING.md`

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: nothing further tasks depend on.

- [ ] **Step 1: Write the failing test**

Add one row to the `cases` table in `TestExamples` in `e2e/e2e_test.go`:

```go
		{"attach", "(default [dockerd://tunneld-demo])"},
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./e2e/ -run TestExamples -count=1`
Expected: FAIL — `build attach: ... no Go files in .../examples/attach`.

- [ ] **Step 3: Write the example**

Create `examples/attach/main.go`:

```go
// Command attach exposes a running container's terminal through a tunnel.
//
// Unlike the other examples it cannot start what it exposes: a container is
// somebody else's process, which is the whole point of attaching to one rather
// than spawning it. Start one first:
//
//	docker run -d --rm --name tunneld-demo -it alpine sh
//
// then run this, and the public URL is a terminal in that container:
//
//	https://<host>/   -> dockerd://tunneld-demo
//
// Stop the container and the page says so. Ctrl-C in the container's shell
// stops the container, exactly as `docker attach` would — the signal reaches
// PID 1, because with a TTY that is what a terminal does.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tunnel-pizza/tunneld/lib"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := lib.New().
		WithURL("dockerd://tunneld-demo").
		Build()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "attach: "+err.Error())
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./e2e/ -run TestExamples -count=1 -v`
Expected: PASS, with the `attach` subtest among them.

- [ ] **Step 5: Update the command's own help**

In `v1alpha1/builder.go`, extend the `Long` string. Replace the block that currently ends with the two-origin example so it reads:

```go
		Long: name + ` exposes already-running local services to the public internet
through an in-process quick tunnel — no cloudflared binary, no account, no DNS.

Repeat --url per origin. They share one hostname: the first is the default,
each later one answers on a bare ?n parameter (n is that flag's position).

  ` + name + ` --url http://localhost:3000 --url http://localhost:4000

    https://<host>/?0   -> http://localhost:3000
    https://<host>/?1   -> http://localhost:4000

An origin can also be a running container, which is served as a terminal in
the browser rather than proxied:

  ` + name + ` --url dockerd://my-container

Public URLs go to stdout, one line per origin; logs go to stderr.`,
```

and extend the `--url` usage string so `--help` names the scheme:

```go
	cmd.Flags().StringArrayVarP(&b.urls, "url", "u", b.urls,
		"local origin to expose, e.g. http://localhost:3000 or dockerd://my-container (repeat for more; :8000 and localhost:8000 also work) [$"+v1.URLEnv+", comma-separated]")
```

Run: `go test ./... -count=1` and `go test ./e2e/ -count=1`.
Expected: PASS. If a golden-help assertion in `e2e/e2e_test.go` breaks on the new wording, update that expectation — the help text is the thing that changed on purpose.

- [ ] **Step 6: Update the README**

Three edits in `README.md`:

Add a row to the examples table (after the `multi-origin` row):

```markdown
| `attach` | A running container's terminal on the public hostname. Needs a container: `docker run -d --rm --name tunneld-demo -it alpine sh`. |
```

Extend the `--url` row in the flags table so it ends with:

```markdown
A value of `dockerd://<container-name-or-id>` is not proxied but served: tunneld answers that origin with a browser terminal attached to the container, the way `docker attach` attaches.
```

Add a section after the multi-origin explanation:

```markdown
### Containers

`--url dockerd://<container-name-or-id>` exposes a terminal attached to a
running container instead of an HTTP service:

```sh
tunneld --url dockerd://my-container
```

It is an origin like any other, so it takes an index, gets a multiview tile,
and mixes freely with HTTP origins:

```sh
tunneld --url :3000 --url dockerd://my-container
```

The semantics are `docker attach`'s, which means most of the behaviour was
decided when the container was started. Without `-t` there is no TTY, so no
line editing and no resize; without `-i` keystrokes reach nothing. The terminal
says which of those applies rather than leaving you guessing. With a TTY,
Ctrl-C reaches PID 1 and stops the container — that is what `docker attach`
does, not something tunneld adds.

The container's existing output replays when the page opens, so a quiet
container still looks alive.

**The page is unauthenticated.** The tunnel hostname is the only secret, the
same as every other origin tunneld exposes — but here the thing behind it is a
shell. Anyone with the link has it.
```

- [ ] **Step 7: Update CONTRIBUTING**

Add to the file map / conventions in `CONTRIBUTING.md`:

```markdown
### Container origins

`v1alpha1/attach/` serves a `dockerd://` origin as a browser terminal, and
`v1alpha1/attach/docker/` is the one provider behind it. The split is
load-bearing: `attach` knows HTTP and the `v4.channel.k8s.io` stream protocol
and nothing about Docker, and `docker` is the reverse. A second provider
implements `attach.Target` — four methods — and `attach` does not change.

Two things there will bite if you change them without knowing why:

- **The page builds its socket URL as `"/attach" + location.search`.** A
  websocket handshake carries no `Referer`, so libtunnel cannot route it as a
  subresource and would fall back to the sticky `libtunnel-origin` cookie,
  which is last-write-wins across tabs. Drop the suffix and two container tiles
  fight over one socket.
- **`bindOrigins` keeps the dialable list the same length and order as the
  displayed one.** Index *n* means origin *n* for `?n` routing, `PublicURL`,
  the reported map and the multiview tiles. Reordering or filtering either list
  breaks all four at once.
```

- [ ] **Step 8: Run the full check**

Run: `make check`
Expected: PASS — `fmt-check`, `vet`, `windows`, `test`, `e2e`.

Run: `make race`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add examples/attach/ e2e/e2e_test.go v1alpha1/builder.go README.md CONTRIBUTING.md
git commit -m "docs: the container origin, its example, and its e2e row

The example cannot start what it exposes — a container is somebody else's
process, which is the point of attaching rather than spawning — so its doc
comment carries the docker run line it needs.

CONTRIBUTING records the two things that bite: why the page appends
location.search to its socket URL, and why the two origin lists have to
stay the same length and order.

Refs #12."
```

- [ ] **Step 10: Open the PR**

```bash
git push -u origin feat/dockerd-attach
gh pr create --title "feat: --url dockerd://<container> serves a browser terminal" \
  --body "Closes #12.

\`--url dockerd://<container-name-or-id>\` puts a terminal for a running container on the tunnel's public hostname, attached the way \`docker attach\` attaches.

A container is not an HTTP service, so tunneld becomes one on its behalf: each such origin gets a loopback server answering \`/\` with an xterm page and \`/attach\` with the \`v4.channel.k8s.io\` stream protocol. The origin list splits into what the operator typed and what libtunnel dials — same length, same order — so \`?n\` routing, \`PublicURL\`, \`--multiview\` and the reported map needed no structural change.

Design: \`docs/superpowers/specs/2026-08-27-dockerd-attach-design.md\`.

**The page is unauthenticated**, deliberately: the hostname is the secret, the same posture as every other origin, and the README says so plainly."
```

---

## Self-Review

**Spec coverage.** Every section of the spec maps to a task: the scheme and `ErrNoDocker` (Task 1), the package layout, `Target` and the page (Task 2), the wire, the established frame, keepalive and the degraded notice (Tasks 2–3), the docker provider with its three pre-mint failures and the TTY/non-TTY split (Task 4), the display/dialable split, `report` and the multiview label (Task 5), and the example, e2e row, README and CONTRIBUTING (Task 6). The spec's `Open(ctx, ref)` gained a `*slog.Logger` third parameter so the resize path can log the way `multiview.Panel` does — noted here because the plan and the spec differ on that one signature.

**Type consistency.** `attach.Target` is the same four methods plus `remotecommand.Attacher` in Tasks 2, 4 and 5. `attach.Serve(ctx, Target, *slog.Logger) (*Server, error)` and `(*Server).URL() *url.URL` are used in Task 5 exactly as defined in Task 2. `docker.Open(ctx, ref, log)` matches `openTarget`'s signature in `origins.go`. `bound.dialable` is the field name in both the definition and `run`.

**Known gaps, stated rather than hidden.** The `location.search` routing guard has no unit test — the attach tests dial `/attach` directly and never load the page — so it is covered only by Task 5 Step 7's two-origin live run. Task 3 Step 6 makes that explicit instead of letting a passing suite imply coverage.
