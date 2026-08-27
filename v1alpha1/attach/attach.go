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
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
	"k8s.io/klog/v2"
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

// klogRouted guards the process-global redirect in routeKlog.
var klogRouted sync.Once

// routeKlog points klog at the tunnel's own logger, once per process.
//
// ServeAttach's machinery — cri-streaming and the wsstream underneath it —
// logs through klog.Background(), which writes to stderr and has never heard
// of --log-level. That silently breaks a documented promise: the flag's
// default is silence, and Logger really does hand back a discard handler. The
// line it breaks on is not an exotic one either. Any abrupt disconnect prints
//
//	E0827 17:37:35.563392 conn.go:353] "Error on socket receive" err="read tcp ...: connection reset by peer"
//
// and an abrupt disconnect is how sessions normally end: a killed browser, a
// closed laptop, a dropped network, the Cloudflare edge reaping a connection.
//
// klog.SetLogger is process-global, which is the cost. It is the same trade
// tunneld already makes and documents for browser.Stdout/Stderr in
// openInBrowser (v1alpha1/tunnel.go) — a package global set on a dependency's
// behalf, because owning the process's output is worth more than leaving a
// global untouched. Routed here rather than in the command so the guarantee
// holds for an embedding program that never runs run(). The first Server's
// logger wins, which for a process with one --log-level is the only logger
// there is.
func routeKlog(log *slog.Logger) {
	klogRouted.Do(func() { klog.SetLogger(logr.FromSlogHandler(log.Handler())) })
}

// Target is one attachable thing behind a Server: it streams, it says what it
// can do, and it releases whatever it holds.
//
// Five methods is the whole provider contract — AttachContainer, from the
// embedded remotecommand.Attacher, plus the four below — which is what keeps
// this package free of Docker. A second provider — podman, or a local shell
// over a pty — implements them and nothing here changes.
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
	//
	// It must tolerate a second call: Server.Close calls it unconditionally
	// from both the context.AfterFunc registered in Serve and a caller's own
	// defer, so an implementation that cannot survive being closed twice will
	// break at shutdown.
	Close() error
}

// Server is the loopback HTTP origin standing in for one Target.
type Server struct {
	target   Target
	listener net.Listener
	srv      *http.Server
	log      *slog.Logger
	// ctx is this Server's lifetime — the tunnel's, narrowed by a cancel of
	// its own — held rather than passed because the only place that needs it
	// is a handler, and a handler's signature is fixed. serveAttach explains
	// why a request's own context will not do.
	ctx    context.Context
	cancel context.CancelFunc
}

// Serve binds a loopback listener and starts serving the terminal on it,
// returning as soon as the port is live so the tunnel never proxies to a
// socket that is not up yet.
//
// The address is 127.0.0.1 rather than a wildcard, deliberately. The page is
// unauthenticated by design — the tunnel hostname is the secret — so the local
// network is not somewhere it belongs. That is the whole of what the bind
// buys, and it is worth being precise about the half it does not: anything
// already running on this machine still reaches the port, a page loaded in the
// operator's own browser included. serveAttach's origin check is what covers
// that half, on the one route where it matters.
//
// The Server takes ownership of target: Close closes both.
func Serve(ctx context.Context, target Target, log *slog.Logger) (*Server, error) {
	routeKlog(log)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("attach %s: listen: %w", target.Name(), err)
	}

	// A cancel of our own, so Close means the same thing however it is
	// reached. The tunnel failing on its own cancels nothing — nobody
	// signalled — and neither srv.Close (which deliberately leaves hijacked
	// connections alone) nor target.Close (which only reaps idle transport
	// connections) reaches a live session. Without this, a Close on that path
	// leaves the session's goroutines and its daemon connection running until
	// the websocket idle timeout unwinds them. The binary exits and never
	// notices; an embedding program, which is the case bindOrigins says it
	// cares about, keeps running.
	sctx, cancel := context.WithCancel(ctx)
	s := &Server{target: target, listener: listener, log: log, ctx: sctx, cancel: cancel}

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

// Close ends every session, stops the origin and releases the target, in that
// order: cancelling first is what unblocks a Target parked in a read on a
// hijacked connection, which neither of the closes below can touch.
//
// It is safe to call twice, which it is: once from the context and once from
// the caller's defer. A context.CancelFunc is documented to tolerate it,
// http.Server.Close is idempotent, and a Target's Close is required to be —
// see Target.Close.
func (s *Server) Close() error {
	s.cancel()
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
	// The notice is page chrome, not container output: it states how the
	// container was started, which was already true before the socket opened
	// and is not changed by anything printed after it. html/template escapes
	// it like any other value — it is our own prose, but it travels next to a
	// name the operator typed.
	data := struct{ Name, Notice string }{s.target.Name(), degraded(s.target)}
	if err := page.Execute(&rendered, data); err != nil {
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
	// Refuse a handshake that came from somewhere else. A websocket is exempt
	// from the same-origin policy — new WebSocket() reaches any host the page
	// can resolve, with no preflight in the way — and the Handshake wsstream
	// installs replaces the one golang.org/x/net/websocket ships with: it
	// negotiates a subprotocol and looks at Origin not at all.
	//
	// So the bind in Serve is not the whole defence. Loopback keeps this page
	// off the local network; it does nothing about a page already running in
	// the operator's browser, which reaches 127.0.0.1 exactly as easily as we
	// do. Without this check, any tab they open can sweep ws://127.0.0.1:<port>
	// for something that speaks v4.channel.k8s.io and, on the first hit, hold
	// stdin and stdout to the container — no tunnel hostname needed.
	//
	// Host is what to compare against because it is the address the page was
	// served from, in all three shapes this origin is reached in: the public
	// hostname through the tunnel (libtunnel forwards the inbound Host rather
	// than rewriting it to the origin's), 127.0.0.1:port on a direct visit,
	// and the public hostname again inside a multiview frame, whose document
	// is served from it.
	//
	// An absent Origin passes, deliberately. A browser always sends one on a
	// handshake, so no Origin means a non-browser client — curl, a script, a
	// test — which was never the thing at risk here. Refusing it would break
	// them and buy nothing.
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			http.Error(w, "attach: cross-origin websocket refused", http.StatusForbidden)
			return
		}
	}

	// ServeAttach hands the Target r.Context(), and on this one handler that
	// context is a promise it cannot keep: Go cancels a request's context when
	// its handler returns, and this handler cannot return while ServeAttach is
	// still inside the Target. So the Target waits for a cancel that is
	// waiting for the Target. Nothing else rescues it either — the connection
	// is hijacked by the websocket upgrade, and http.Server.Close does not
	// touch hijacked connections.
	//
	// Deriving from the Server's own lifetime instead breaks the circle: a
	// tunnel shutting down cancels this before ServeAttach returns, which is
	// the only moment at which cancelling it is worth anything. Please do not
	// "simplify" this back to r.Context().
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	r = r.WithContext(ctx)

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
	remotecommand.ServeAttach(w, r, bounded{s.target}, name, "", name, opts,
		idleTimeout, remotecommand.DefaultStreamCreationTimeout,
		remotecommand.SupportedStreamingProtocols)
}

// bounded wraps a Target so that an attach ends when its connection does.
//
// A Target streaming a quiet container is blind to both exits. The context
// serveAttach supplies covers the tunnel shutting down; the far commoner exit
// is a visitor closing their tab, and nothing the Target holds is tied to that
// browser — a copy parked in Read on a socket that will never speak again has
// no way to learn the far end is gone.
//
// The resize channel is the one thing that does know. On the websocket path
// this page speaks, ServeAttach opens it unconditionally — not gated on TTY,
// not gated on stdin, unlike every other stream here — and closes it when the
// connection ends, so ranging over it and cancelling at the end translates
// "the socket died" into the one signal a Target already acts on. Forwarding
// sizes through a channel of our own is what lets us watch for that end
// without taking resize away from the Target.
//
// Worst case is bounded rather than instant: a clean close ends the range at
// once, and a connection that dies without saying so waits out the websocket's
// idle timeout first. Two minutes is the ceiling either way, which is the
// whole point — before this, the ceiling was the life of the process.
type bounded struct{ Target }

func (b bounded) AttachContainer(ctx context.Context, name, uid, container string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	forwarded := make(chan remotecommand.TerminalSize)
	go func() {
		defer close(forwarded)
		defer cancel()
		for {
			select {
			case size, ok := <-resize:
				if !ok {
					return
				}
				select {
				case forwarded <- size:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				// Also the escape hatch for a resize channel that is nil,
				// which is what a client negotiating no subprotocol over SPDY
				// would produce. Ranging over that would park this goroutine
				// for good; selecting on the context cannot.
				return
			}
		}
	}()

	return b.Target.AttachContainer(ctx, name, uid, container, in, out, errw, tty, forwarded)
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
