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
	remotecommand.ServeAttach(w, r, s.target, name, "", name, opts,
		idleTimeout, remotecommand.DefaultStreamCreationTimeout,
		remotecommand.SupportedStreamingProtocols)
}
