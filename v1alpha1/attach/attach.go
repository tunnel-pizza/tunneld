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
// It is an implementation subpackage and knows nothing about Docker except the
// wire format Docker's attach protocol defined, which every provider here
// speaks: the provider arrives as a Targets that opens Target values,
// CopyOutput is the one place a target's stream is read, and this package
// hosts the binder that resolves an origin through it. index.html travels
// with the code: go:embed cannot reach outside its own package directory.
package attach

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/moby/moby/api/pkg/stdcopy"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
	"k8s.io/klog/v2"

	v1 "github.com/tunnel-pizza/tunneld/v1"
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

// klogRouted guards the process-global redirect Serve performs.
var klogRouted sync.Once

// Target is one attachable thing behind a Server: it streams, it says what it
// can do, and it releases whatever it holds.
//
// Six methods is the whole provider contract — AttachContainer, from the
// embedded remotecommand.Attacher, plus the five below — which is what keeps
// this package free of any particular provider. Another one — podman, say —
// implements them and nothing here changes.
type Target interface {
	// AttachContainer streams between the caller's ends and the target's
	// stdio, returning when the target's stream ends. The name, uid and
	// container arguments are Kubernetes' shape, which ServeAttach fills in
	// from what it is given; a single-target provider ignores them.
	remotecommand.Attacher
	// Name is the reference the operator typed, used to title the page.
	Name() string
	// Scheme is the origin scheme this target was opened for — the provider's
	// own, fixed. With Name it reconstructs the origin as typed, which is what
	// the frame puts in its corner and what somebody pastes back into a
	// command line.
	Scheme() string
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

// Logs is where tunneld's own recent log lines come from, for a frame to show
// on request.
//
// Read rather than subscribed to, because a frame draws when it draws: it asks
// for the lines it is about to render and renders them, and a viewer who is
// not looking at the logs costs nothing.
type Logs interface {
	Lines() []string
}

// Repeatable is a Target that can be attached to more than once, because each
// attach starts it rather than resuming it.
//
// Discovered on the Target rather than required by it, the way http.Flusher is:
// most targets are not repeatable and say so by not implementing this. A
// container is the example — once its PID 1 has exited there is nothing left
// to attach to — and a local program is the counter-example, since its origin
// is a path and running it again is exactly as well defined as running it the
// first time.
//
// The method answers rather than merely existing so that a provider can decide
// per target: a program whose path has since been removed is no more
// repeatable than a container.
type Repeatable interface {
	Repeatable() bool
}

// Targets opens an origin's reference as a Target. It is the half of the
// provider contract the binder depends on — resolving what the operator
// typed — where Target is the half Server depends on. One provider
// implements both, which is why both live here.
//
// The assertion that a provider satisfies these lives in the provider, not
// here: attach/docker and attach/shell import this package for the contract,
// so naming either of them from here is an import cycle.
type Targets interface {
	// Scheme is the origin scheme this provider answers, and the whole of how
	// the binder chooses between providers. One provider, one scheme: a
	// dockerd:// origin is a container and a file:// origin is a program, and
	// nothing about either is a matter of degree.
	Scheme() string
	Open(ctx context.Context, ref string, log *slog.Logger) (Target, error)
}

// CopyOutput copies a target's output into the ends ServeAttach handed it. It
// is the single place the wire format between a provider and this package is
// decided, so a provider that uses it cannot drift from the others.
//
// Every provider streams one thing, whatever it has underneath: a container's
// attach socket, a pseudo-terminal, a pipe. Whether that one stream carries a
// second channel inside it is therefore the same question everywhere, and the
// answer is the one Docker's attach protocol already gives.
//
// With a terminal there is nothing to split. The target merged stdout and
// stderr at the source, the bytes are raw, and errw is left alone — ServeAttach
// does not even open that channel, since Options.Stderr is !TTY and Kubernetes'
// own clients treat a TTY session carrying one as malformed.
//
// Without a terminal the two arrive multiplexed: an 8-byte header per chunk
// carrying stream id and length. Demultiplexing is what puts the target's
// stderr on a channel of its own instead of printing the headers into
// somebody's terminal.
//
// The stream ending is the normal end of an attach rather than a failure. The
// process exiting, the visitor leaving and the tunnel shutting down all arrive
// here as a dead reader, and each transport spells that its own way — EOF, a
// closed socket, a closed file, the EIO a pseudo-terminal gives once its last
// writer is gone. Every one of them comes back as nil, so a provider returns
// this directly.
func CopyOutput(out, errw io.Writer, r io.Reader, tty bool) error {
	var err error
	if tty {
		_, err = io.Copy(out, r)
	} else {
		_, err = stdcopy.StdCopy(out, errw, r)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EIO) ||
		errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Option configures a BinderImpl at construction.
type Option = v1.Option[*BinderImpl]

// BinderImpl turns the origins the operator typed into the origins libtunnel
// can proxy to.
//
// An http or https origin passes through untouched; an origin whose scheme a
// provider claims — dockerd:// for a container, file:// for a local program —
// is served here, by a loopback attach server that takes its place in the
// list. The two lists share a length and an order, which is the whole point:
// index n still means origin n for the bare ?n routing parameter, for the
// addresses reported, for the map printed and for the multiview tiles, so a
// container is an origin like any other and nothing downstream learns a second
// shape.
type BinderImpl struct {
	// targets is the providers by the scheme each one answers, which is how
	// Bind picks between them. A map rather than a list because the lookup is
	// the dispatch, and because two providers claiming one scheme is a wiring
	// mistake that should collapse rather than depend on order.
	targets map[string]Targets
	banner  string
	logs    Logs
}

// New returns a BinderImpl, configured by opts. It carries no Targets until
// WithTargets sets some; an origin whose scheme none of them answers fails at
// Bind rather than at construction.
func New(opts ...Option) *BinderImpl {
	return v1.Apply(&BinderImpl{}, opts...)
}

// WithTargets sets what turns an origin's reference into something attach can
// serve, one provider per scheme. The defaults are the Docker daemon and this
// machine's own programs; a test hands in a stub.
//
// Each provider names its own scheme, so there are no keys to keep in step
// with the values. Repeating the option appends, and a later provider replaces
// an earlier one that answered the same scheme.
func WithTargets(targets ...Targets) Option {
	return func(b *BinderImpl) {
		if b.targets == nil {
			b.targets = make(map[string]Targets, len(targets))
		}
		for _, t := range targets {
			b.targets[t.Scheme()] = t
		}
	}
}

// WithLogs sets where the frames this binder serves read tunneld's own recent
// log lines from. Unset, a frame has none to show and says so.
func WithLogs(logs Logs) Option {
	return func(b *BinderImpl) { b.logs = logs }
}

// WithBanner sets the build line every terminal this binder serves shows along
// the bottom of its frame.
//
// Passed in rather than worked out here. It names the command, which an
// embedding program renames, and the versions, which the root resolves from
// build information — none of it knowable from a subpackage, and all of it
// fixed for the life of the process, so it is configuration and not an
// announcement.
func WithBanner(banner string) Option {
	return func(b *BinderImpl) { b.banner = banner }
}

// Bind implements Binder.
//
// A failure unwinds everything already bound. The command is about to return
// an error, and a listener left behind would outlive it inside an embedding
// program.
func (b *BinderImpl) Bind(ctx context.Context, display []*url.URL, log *slog.Logger) ([]*url.URL, io.Closer, error) {
	dialable := make([]*url.URL, 0, len(display))
	var servers bound
	for at, origin := range display {
		// Anything no provider claims is an address the tunnel dials itself.
		// http and https are the whole of that today; the origin parser
		// refuses every other scheme, so this is a pass-through rather than a
		// judgement.
		provider, served := b.targets[origin.Scheme]
		if !served {
			if origin.Scheme != "http" && origin.Scheme != "https" {
				_ = servers.Close()
				return nil, nil, fmt.Errorf("attach: no Targets configured to open %s://%s", origin.Scheme, origin.Host+origin.Path)
			}
			dialable = append(dialable, origin)
			continue
		}

		// The reference is the authority, or the path when the origin carries
		// one: file:///usr/bin/top names a program the only way a URL can hold
		// an absolute path. Exactly one of the two is ever set — the origin
		// parser refuses a served origin with both — so joining them is the
		// whole rule.
		target, err := provider.Open(ctx, origin.Host+origin.Path, log)
		if err != nil {
			_ = servers.Close()
			return nil, nil, err
		}
		server, err := Serve(ctx, target, b.banner, b.logs, log)
		if err != nil {
			_ = target.Close()
			_ = servers.Close()
			return nil, nil, err
		}
		servers = append(servers, boundOrigin{at: at, srv: server})
		dialable = append(dialable, server.URL())
		log.Info("serving a reference as an origin", "reference", origin.Host+origin.Path, "scheme", origin.Scheme, "origin", server.URL())
	}
	// A closer that can be shown on a console says so by carrying the method, and only
	// when it can: one origin, which is one served origin because nothing
	// else gets a server. A caller then asks by type assertion and gets a
	// straight answer, rather than re-deriving from the origin list what was
	// already decided here.
	if len(servers) == 1 {
		return dialable, sole{servers}, nil
	}
	return dialable, servers, nil
}

// sole is a bound list of exactly one, which is the only shape that can put a
// terminal on a console: with several, a console has no way to say
// which it is watching and no room to watch them at once — that is what the
// public hostname and its routing parameter are for.
//
// A wrapper rather than a flag, because what a caller wants to know is whether
// to ask, and a method set is how Go says that. bound keeps show unexported
// to this file so the only way to reach it is through here.
type sole struct{ bound }

func (s sole) Show(ctx context.Context, in io.Reader, out io.Writer) error {
	return s.bound.show(ctx, in, out)
}

// bound is every attach server a Bind call started, with the place in the
// origin list each of them took.
//
// The index is kept because that is the only thing that connects a server to
// the address it will answer on: the tunnel hands back one public URL and the
// origins are told apart by their routing parameter, so origin n's address is
// derived from n. Same length and order as display, like everything else here.
type bound []boundOrigin

// show puts the one origin bound here on the given streams.
//
// Unexported, and reached only through the sole wrapper Bind returns
// when there is exactly one: a bound list of several has nothing to draw, and
// the length check that used to live here was a guard against a caller that
// can no longer exist.
func (b bound) show(ctx context.Context, in io.Reader, out io.Writer) error {
	if len(b) != 1 {
		return fmt.Errorf("attach: %d origins to mirror, want exactly one", len(b))
	}
	return b[0].srv.Show(ctx, in, out)
}

// Quit closes when a viewer of any of these origins asks the run to end. One
// channel for all of them, because what they are asking for is the process,
// which there is only one of.
//
// The watchers live as long as the origins do, which is as long as the run: a
// closer that has been closed has nothing left to watch for, and a run that is
// over is not waiting on this.
func (b bound) Quit() <-chan struct{} {
	asked := make(chan struct{})
	var once sync.Once
	for _, o := range b {
		go func(srv *Server) {
			select {
			case <-srv.Quit():
				once.Do(func() { close(asked) })
			case <-srv.ctx.Done():
			}
		}(o.srv)
	}
	return asked
}

type boundOrigin struct {
	at  int
	srv *Server
}

// Close shuts every attach server down, and with it every container client
// they own. The first error is returned and the rest still close: a partial
// shutdown is worse than a lost error message.
func (b bound) Close() error {
	var err error
	for _, o := range b {
		if cerr := o.srv.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// Announce gives each server the public address it answers on, taken from
// public by the index the origin had.
//
// It is what the root's Announcer asks for. Discovered by assertion rather
// than named in the Binder contract, because this package cannot refer to that
// contract's types — v1alpha1 imports attach, not the other way round — so the
// closer Bind hands back is asked whether it can do this rather than required
// to.
//
// A short list is not an error. It means the caller had fewer addresses than
// origins, and a server without one simply has nothing to show.
func (b bound) Announce(public []string) {
	for _, o := range b {
		if o.at < len(public) {
			o.srv.Announce(public[o.at])
		}
	}
}

// Server is the loopback HTTP origin standing in for one Target.
type Server struct {
	target   Target
	session  *session
	listener net.Listener
	srv      *http.Server
	log      *slog.Logger
	// ctx is this Server's lifetime — the tunnel's, narrowed by a cancel of
	// its own — held rather than passed because the only place that needs it
	// is a handler, and a handler's signature is fixed. the attach handler
	// explains why a request's own context will not do.
	ctx    context.Context
	cancel context.CancelFunc

	// quit closes when a viewer asks the whole run to end. Not this origin's
	// own shutdown — that is cancel — but the process's: the frame offers it,
	// and what acts on it is the command, which is watching through Quit.
	quitOnce sync.Once
	quit     chan struct{}
}

// Show puts this origin's terminal on the given streams, as one more viewer
// of the same session. It returns when that viewer leaves or the run ends.
func (s *Server) Show(ctx context.Context, in io.Reader, out io.Writer) error {
	return s.session.viewLocally(ctx, in, out)
}

// Quit closes when a viewer has asked the run to end. The channel is never
// sent on and closes at most once, so a caller may select on it forever.
func (s *Server) Quit() <-chan struct{} { return s.quit }

// Serve binds a loopback listener and starts serving the terminal on it,
// returning as soon as the port is live so the tunnel never proxies to a
// socket that is not up yet.
//
// The address is 127.0.0.1 rather than a wildcard, deliberately. The page is
// unauthenticated by design — the tunnel hostname is the secret — so the local
// network is not somewhere it belongs. That is the whole of what the bind
// buys, and it is worth being precise about the half it does not: anything
// already running on this machine still reaches the port, a page loaded in the
// operator's own browser included. the attach handler's origin check is what
// covers that half, on the one route where it matters.
//
// The Server takes ownership of target: Close closes both.
func Serve(ctx context.Context, target Target, banner string, logs Logs, log *slog.Logger) (*Server, error) {
	// This points klog at the tunnel's own logger, once per process.
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
	// tunneld already makes and documents for pkgbrowser.Stdout/Stderr in
	// browser.BrowserImpl.Open (v1alpha1/browser) — a package global set on a
	// dependency's behalf, because owning the process's output is worth more
	// than leaving a global untouched. Routed here rather than in the command
	// so the guarantee holds for an embedding program that never executes the
	// command. The first Server's logger wins, which for a process with one
	// --log-level is the only logger there is.
	klogRouted.Do(func() { klog.SetLogger(logr.FromSlogHandler(log.Handler())) })

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
	// notices; an embedding program, which is the case Bind says it
	// cares about, keeps running.
	sctx, cancel := context.WithCancel(ctx)
	s := &Server{
		target:   target,
		listener: listener,
		log:      log,
		ctx:      sctx,
		cancel:   cancel,
		quit:     make(chan struct{}),
	}
	// One attach for the life of the Server, shared by every page that opens
	// it. Started here rather than on the first connection so a viewer never
	// waits on the target, and so what happened before anybody looked is on
	// the screen when they do.
	//
	// The frame offers an exit, and this is what it reaches: closing quit says
	// a viewer asked, and nothing here acts on it — ending the run is the
	// command's to do, and it is watching.
	s.session = newSession(sctx, target, banner, logs, func() {
		log.Info("a viewer asked the run to end", "target", target.Name())
		s.quitOnce.Do(func() { close(s.quit) })
	}, log)

	mux := http.NewServeMux()
	// "GET /{$}" is the root exactly, not a prefix — an origin's stray request
	// gets a 404 rather than the terminal page a second time. GET also answers HEAD,
	// which is what the reachability probe sends.
	//
	// The page handler renders the terminal page. A render failure is logged
	// and answered with a plain error rather than a half-written page.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		var rendered strings.Builder
		// The notice is page chrome, not container output: it states how the
		// container was started, which was already true before the socket opened
		// and is not changed by anything printed after it. html/template escapes
		// it like any other value — it is our own prose, but it travels next to a
		// name the operator typed.
		//
		// notice names what a container was started without, or "" when it was
		// started with both -t and -i and there is nothing to explain.
		//
		// The wording names the docker run flag rather than the symptom, because
		// that is the lever: nothing tunneld can do fixes a container already
		// running without a TTY, and the reader's next move is to restart it.
		var notice string
		switch {
		case !s.target.TTY() && !s.target.Stdin():
			notice = "no TTY and no stdin (started without -it) — output only"
		case !s.target.TTY():
			notice = "no TTY (started without -t) — no line editing, no resize"
		case !s.target.Stdin():
			notice = "stdin closed (started without -i) — keystrokes go nowhere"
		}
		// Origin is what the frame puts in its top-left corner, said again
		// here because the overlay covers that corner: a page that has lost
		// its socket should still name what it was showing.
		//
		// What the button offers is not decided here. It depends on whether
		// the run is still going, which is not knowable when the page is
		// built and is exactly knowable when it is asked — see /alive.
		data := struct {
			Notice string
			Origin string
		}{notice, s.target.Scheme() + "://" + s.target.Name()}
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
	})
	// Whether coming back is worth offering, asked after a socket has gone
	// rather than answered when the page was built: a page is served while the
	// terminal is live and read when it is not, and the two endings a viewer
	// sees are the same. Their own socket dropping — a lid, an idle timeout —
	// leaves a terminal that is still there, and the session ending on a
	// container leaves nothing at all.
	//
	// The answer is the word to put on the button, because there are three
	// outcomes and not two: reconnecting to a run still going is not the same
	// as starting a program over, and a page that called both of them the same
	// thing told somebody who had just detached from their shell that pressing
	// it would replace it.
	mux.HandleFunc("GET /alive", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		offer := s.session.offer()
		if offer == "" {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, offer)
	})

	// The attach handler hands the request to ServeAttach, which owns the
	// websocket upgrade and the v4.channel.k8s.io framing on it.
	mux.HandleFunc("GET /attach", func(w http.ResponseWriter, r *http.Request) {
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
		remotecommand.ServeAttach(w, r, s.session, name, "", name, opts,
			idleTimeout, remotecommand.DefaultStreamCreationTimeout,
			remotecommand.SupportedStreamingProtocols)
	})

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

// Announce tells the terminal the public address it answers on, which is what
// its frame shows in the corner. Before this it shows nothing there.
func (s *Server) Announce(public string) { s.session.announce(public) }

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
	// The session before the target: it holds the stream the target is
	// serving, and closing it is what lets the target's own Close finish
	// rather than wait out an attach nobody is reading.
	if serr := s.session.close(); err == nil {
		err = serr
	}
	if terr := s.target.Close(); err == nil {
		err = terr
	}
	return err
}
