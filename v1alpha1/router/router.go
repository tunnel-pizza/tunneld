// Package router is what puts several origins behind one tunnel hostname: a
// loopback server in front of them that decides, per request, which one it
// reaches, with the multiview panel and the framing-header scrub in front of
// that — and what a visitor is told when the one it reaches is not listening.
//
// Every rule here is tunneld's convention and no tunnel's concern — the bare
// ?n parameter the panel's tiles and the reported addresses carry, the +ws
// marker an operator types, the Referer a framed tile's assets follow, the
// cookie that keeps an address-bar visit on the tile somebody picked. They
// lived in libtunnel's reverse proxy until #176, which left tunneld encoding
// every signal into a URL list for libtunnel to read back out, and a marker
// three packages had to carry untouched to reach the one that consumed it
// (#173). The tunnel is handed this server's address, one URL, and forwards a
// hostname to it without knowing there is more than one thing behind it.
package router

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tunnel-pizza/tunneld/v0exp1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
)

// Cookie is the sticky-routing cookie an explicit top-level pick answers
// with, so a parameter-less follow-up — an address-bar visit, a bookmark —
// stays on the origin somebody chose.
const Cookie = "tunneld-origin"

// ControlPath is the prefix tunneld keeps for itself on the tunnel hostname:
// a request under it is answered by the router's mux and never reaches an
// origin — one it has no pattern for is a 404, not an origin's page. Every
// other path is the origins', passed through exactly as it was sent.
//
// A prefix rather than patterns registered at the root, because a ServeMux
// cleans the paths it serves, answering "/a//b" or "/a/../b" with a redirect
// to the clean form; in front of the origins that would rewrite what they are
// sent. Under this prefix only tunneld's own paths are cleaned.
//
// Declared in v1, so auth, which the router imports, can name it too.
const ControlPath = v1.ControlPath

// unreachableHeader marks tunneld's own answer for an origin nothing is
// listening on, its value that origin's host. The page the answer carries
// asks again until the origin answers, and needs to tell the two apart: a 503
// on its own could be the origin's, and an app that is up and says it is
// unavailable is answering — waiting on it would hide it.
const unreachableHeader = "X-Tunneld-Unreachable"

// retryAfter is the Retry-After, in seconds, on that answer, and how soon its
// page first asks again. About as long as a dev server takes to bind once it
// is started, so somebody who starts it after the link went out is not left
// waiting on a timer; long enough that a page left open is not a busy loop
// across the edge, and the page backs off from here. A client that honours
// the header, curl --retry among them, waits the same.
const retryAfter = 2

// dialTimeout bounds each dial Unanswered makes. A refused dial on this
// machine answers at once; the bound is for an origin on another host that
// drops the attempt instead, which would otherwise hold the run's report for
// as long as a dial takes to give up — thirty seconds, in the transport's.
const dialTimeout = time.Second

// unreachableHTML is the page a browser gets for an origin nothing is
// listening on. Embedded, like the panel, so a tunnel serves it with nothing
// installed and no outbound request.
//
//go:embed unreachable.html
var unreachableHTML string

// unreachableTmpl is parsed once at init: a page that fails to parse is a
// mistake in a file that ships inside the binary, not a visitor's problem.
var unreachableTmpl = template.Must(template.New("unreachable").Parse(unreachableHTML))

// Router puts several origins behind the one address a tunnel forwards to.
// Every rule that picks an origin for a request — the bare ?n parameter, the
// +ws origin for a handshake, a same-host Referer, the sticky cookie — is
// tunneld's convention, so it is served here on a loopback listener rather
// than asked of the tunnel engine, which is handed one URL and knows nothing
// of origins (#176).
//
// Route answers with that URL, the router's own — a lone origin's run
// included, so tunneld's /_tunneld/ control path answers on every tunnel.
// What it routes is the run's,
// handed over in the options it takes, as Display's Open is: the dialable
// origins, the index of the one marked +ws, what the display's Panel answered
// to put in front, and the run's logger.
//
// Unanswered dials each http and https origin once and says which indexes
// nothing answered on, so the run can report that the address is up and the
// thing behind it is not. It is the router's because the router is what dials
// origins: a failed dial is what it answers visitors with a page for.
//
// Cancel takes the router down. Not ctx: the router outlives the run for as
// long as the tunnel drains, so the caller cancels it once the tunnel is done.
type Router interface {
	Route(ctx context.Context, opts ...Option) (*url.URL, error)
	Unanswered(ctx context.Context, origins v1.Origins) []int
	Cancel()
}

// Option configures a RouterImpl, at construction or for one Route.
type Option = v1.Option[*RouterImpl]

// discard is where a router with no logger writes: nowhere.
var discard = slog.New(slog.DiscardHandler)

// RouterImpl is the default Router. What it routes is one run's, so every
// field is set by an option — given to New as a default, or to Route for that
// route alone.
type RouterImpl struct {
	// dialable is the origins a request can reach, in the order the operator
	// gave them: index n is origin n.
	dialable v1.Origins
	// ws is the index of the origin marked +ws, which a WebSocket handshake
	// with nothing else to route on goes to; -1 for none.
	ws int
	// wrap wraps the routing handler: the display's panel and its framing
	// scrub, which answer or reshape a request before any origin is chosen.
	// Nil is nothing in front.
	wrap func(http.Handler) http.Handler
	log  v1.Logger
	// mux answers the ControlPath: the router's own endpoints, registered on
	// the mux New makes. Shared by every route, since no route registers on
	// it.
	mux *http.ServeMux

	// live is what Cancel stops: every route this router has serving. A
	// pointer, so the copy each Route configures still reaches it.
	live *routes
	// env is what the cache's endpoints under ControlPath answer from, once
	// WithCache has put them on the mux, and the auth in front of the
	// visitor side. A pointer for the same reason as live.
	env *env
	// allowOrigin is the one browser origin CORS answers on .env: the
	// provider's, which minted the tunnel and holds its secret. "" for none.
	allowOrigin string
}

// CacheKeyHeader is what every response under the ControlPath carries, a
// refusal included: the run's cache key, the name of the file its spec is
// saved in, so a caller knows which run answered. Absent only when the cache
// does not know its run yet.
const CacheKeyHeader = "X-Cache-Key"

// env is the cache endpoints' state: the cache they serve from, the agent
// server, and the patterns already on the mux — a ServeMux panics on a
// pattern registered twice, and WithCache and WithMcp can be applied more
// than once.
type env struct {
	mu         sync.Mutex
	cache      cache.Cache
	auth       auth.Auth
	mcp        v0exp1.Mcp
	registered map[string]bool
}

// routes is a router's serving routes, by the function that stops each.
type routes struct {
	mu    sync.Mutex
	stops []func()
}

// New returns a RouterImpl configured by opts: no origins, none owning
// WebSockets, nothing in front, a logger that discards, and the mux that
// answers the ControlPath with the router's own endpoints:
//
//	GET ControlPath+"ping"   200 "pong": this tunnel reaches this tunneld
//
// and, once WithCache has been applied, whatever the cache's Handlers answer —
// the default cache's being:
//
//	GET ControlPath+".env"   200 the run's cached spec, as the cache file's
//	                         LIBTUNNEL_SPEC line; 404 when nothing is cached
func New(opts ...Option) *RouterImpl {
	mux := http.NewServeMux()
	// Answered here, by tunneld, and never by an origin: a 200 says the edge,
	// the tunnel and the router are all up, whatever state the origins are in.
	// Not stored anywhere on the way, so every ping is asked of this process.
	mux.HandleFunc("GET "+ControlPath+"ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "pong")
	})
	return v1.Apply(&RouterImpl{ws: -1, log: discard, mux: mux, live: &routes{}, env: &env{}}, opts...)
}

// WithOrigins sets the origins to route between: the dialable list, in the
// operator's order.
func WithOrigins(dialable v1.Origins) Option {
	return func(r *RouterImpl) { r.dialable = dialable }
}

// WithWebSockets sets the origin a WebSocket handshake goes to when nothing
// else in it routes: the index of the origin marked +ws, -1 for none.
func WithWebSockets(ix int) Option {
	return func(r *RouterImpl) { r.ws = ix }
}

// WithWrap sets what wraps the routing handler — the display's Panel — or
// nothing, when nil.
func WithWrap(wrap func(http.Handler) http.Handler) Option {
	return func(r *RouterImpl) { r.wrap = wrap }
}

// WithMcp puts m's Handler at ControlPath+"mcp" on the router's mux, behind
// the secret like everything else under ControlPath (see authorize). Which
// server answers is asked on every request, so applied again, it replaces
// the server rather than registering the endpoint twice; once nil, the path
// is a 404.
func WithMcp(m v0exp1.Mcp) Option {
	return func(r *RouterImpl) {
		e := r.env
		e.mu.Lock()
		defer e.mu.Unlock()
		e.mcp = m
		pattern := ControlPath + "mcp"
		if m == nil || e.registered[pattern] {
			return
		}
		if e.registered == nil {
			e.registered = map[string]bool{}
		}
		e.registered[pattern] = true
		r.mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
			e.mu.Lock()
			m := e.mcp
			e.mu.Unlock()
			if m == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			m.Handler().ServeHTTP(w, req)
		})
	}
}

// WithCache puts c's Handlers under ControlPath on the router's mux —
// the default cache's .env being a remote copy of the file the run last
// saved. Which cache answers is asked on every request, so applied again, it
// replaces the cache rather than registering an endpoint twice; a pattern the
// cache answering does not have is a 404.
//
// c is also what the ControlPath is authorized against: its secret is the
// token every request under it but ping has to carry (see authorize). The
// file holds the credential for the tunnel's public hostname, so it is
// served only to whoever already has the secret it contains.
func WithCache(c cache.Cache) Option {
	return func(r *RouterImpl) {
		e := r.env
		e.mu.Lock()
		defer e.mu.Unlock()
		e.cache = c
		if c == nil {
			return
		}
		for pattern := range c.Handlers(ControlPath) {
			if e.registered[pattern] {
				continue
			}
			if e.registered == nil {
				e.registered = map[string]bool{}
			}
			e.registered[pattern] = true
			r.mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
				e.mu.Lock()
				c := e.cache
				e.mu.Unlock()
				var h func(http.ResponseWriter, *http.Request)
				if c != nil {
					h = c.Handlers(ControlPath)[pattern]
				}
				if h == nil {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				h(w, req)
			})
		}
	}
}

// WithAuth puts a in front of everything a visitor reaches and its pages
// (login, logout) under ControlPath, which need no secret. Which auth answers
// is asked on every request, so applied again it replaces the auth rather
// than registering a page twice.
func WithAuth(a auth.Auth) Option {
	return func(r *RouterImpl) {
		e := r.env
		e.mu.Lock()
		defer e.mu.Unlock()
		e.auth = a
		if a == nil {
			return
		}
		for pattern := range a.Handlers(ControlPath) {
			if e.registered[pattern] {
				continue
			}
			if e.registered == nil {
				e.registered = map[string]bool{}
			}
			e.registered[pattern] = true
			r.mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
				e.mu.Lock()
				a := e.auth
				e.mu.Unlock()
				var h func(http.ResponseWriter, *http.Request)
				if a != nil {
					h = a.Handlers(ControlPath)[pattern]
				}
				if h == nil {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				h(w, req)
			})
		}
	}
}

// WithAllowOrigin is the one origin CORS answers on .env: the provider's,
// which minted the tunnel and holds its secret, so the owner's browser on it
// can PATCH the password with a grant. "" answers none.
func WithAllowOrigin(origin string) Option {
	return func(r *RouterImpl) { r.allowOrigin = origin }
}

// ProviderOrigin is the browser origin of a provider host as libtunnel reads
// it: https://<host>, or the scheme and host of a value that carries a
// scheme already (a dev server, a mock).
func ProviderOrigin(provider string) string {
	if strings.Contains(provider, "://") {
		if u, err := url.Parse(provider); err == nil {
			return u.Scheme + "://" + u.Host
		}
	}
	return "https://" + provider
}

// WithLog sets where the router says what it did. Nil keeps the one it has.
func WithLog(log v1.Logger) Option {
	return func(r *RouterImpl) {
		if log != nil {
			r.log = log
		}
	}
}

// Origins, WebSockets and Wrap read back what the options set, for a
// caller standing in for a router that wants to see what it was handed
// without standing one up.
func (r *RouterImpl) Origins() v1.Origins                   { return r.dialable }
func (r *RouterImpl) WebSockets() int                       { return r.ws }
func (r *RouterImpl) Wrap() func(http.Handler) http.Handler { return r.wrap }

// Unanswered dials each http and https origin in origins once and answers
// with the index of every one nothing answered on — nothing listening, no
// route, or no answer within dialTimeout — in order. An origin tunneld serves
// itself is not an address, and is not dialed. A context that ends first
// answers nothing: what failed then was the asking.
//
// A dial and not a request: whether anything listens is the question, and a
// request would reach an app that does with a visit nobody made. All at once,
// so a report waits on one slow dial at most.
func (r *RouterImpl) Unanswered(ctx context.Context, origins v1.Origins) []int {
	urls := origins.URLs()
	down := make([]bool, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		port := u.Port()
		switch {
		case u.Scheme != "http" && u.Scheme != "https":
			continue
		case port == "" && u.Scheme == "http":
			port = "80"
		case port == "":
			port = "443"
		}
		wg.Go(func() {
			d := net.Dialer{Timeout: dialTimeout}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
			if err != nil {
				down[i] = true
				return
			}
			_ = conn.Close()
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	var unanswered []int
	for i, d := range down {
		if d {
			unanswered = append(unanswered, i)
		}
	}
	return unanswered
}

// Route stands a loopback server up in front of the origins and returns the
// one address the tunnel should forward to.
//
// opts are this route's, applied over the router's own on a copy: what New
// was given stays the default for every route, and one route's origins never
// leak into the next.
//
// Every route is served, a lone origin's with nothing in front of it
// included, so the ControlPath answers on every tunnel. A lone origin gets
// none of the routing — a bare numeric parameter is then the application's
// own — only the pass-through and the control path.
//
// The server is down when Cancel is called, not when ctx ends. A tunnel told
// to stop keeps answering what the edge already sent it for a grace period,
// and every one of those requests comes through here; a router closed with
// ctx would turn them into refused dials in a program that embeds tunneld and
// outlives the run. So ctx lends the requests its values and nothing else, and
// the caller cancels the router once the tunnel forwarding here is done — or
// at once, when there never was one. Upgraded connections — a WebSocket
// through the proxy — are not ended by closing the server, so the requests
// are based on the router's lifetime as well, and the proxy drops the origin
// side of a socket when that ends.
func (r *RouterImpl) Route(ctx context.Context, opts ...Option) (*url.URL, error) {
	route := *r
	v1.Apply(&route, opts...)
	dialable, ws, front, log := route.dialable, route.ws, route.wrap, route.log
	if dialable == nil || dialable.Len() == 0 {
		return nil, errors.New("router: no origins to route to")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("router: listen: %w", err)
	}
	// The visitor side: the origins, the panel in front of them, and auth in
	// front of both. The control path sits beside it rather than under it:
	// it has its own guard (authorize), and the panel answers nothing there.
	var site http.Handler = redirect(dialable.Len(), proxy(dialable.URLs(), ws, log))
	if front != nil {
		site = front(site)
	}
	route.env.mu.Lock()
	gate := route.env.auth
	route.env.mu.Unlock()
	if gate == nil {
		site = metadata(site, fallback)
	} else {
		site = gate.Handler(metadata(site, gate))
	}
	control := route.cors(route.authorize(route.mux))
	routing, stop := context.WithCancel(context.WithoutCancel(ctx))
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w = &unstored{ResponseWriter: w}
			if strings.HasPrefix(r.URL.Path, ControlPath) {
				control.ServeHTTP(w, r)
				return
			}
			site.ServeHTTP(w, r)
		}),
		BaseContext: func(net.Listener) context.Context { return routing },
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	context.AfterFunc(routing, func() { _ = srv.Close() })
	route.live.mu.Lock()
	route.live.stops = append(route.live.stops, stop)
	route.live.mu.Unlock()
	go func() { _ = srv.Serve(l) }()

	local := &url.URL{Scheme: "http", Host: l.Addr().String()}
	log.Info("routing origins", "listen", local.Host, "origins", dialable.Len())
	return local, nil
}

// authorize guards the ControlPath: every request under it needs
// "Authorization: token <secret>", the running tunnel's secret as the cache
// WithCache handed over holds it, base64-encoded (the encoding the spec's own
// JSON gives it, so whoever holds the spec holds the token), except a path
// and method authMethods has: ping, login, logout and the resource metadata
// by the methods a visitor uses, and a PATCH of .env carrying "Bearer
// <grant>".
//
// A CORS preflight never reaches it: cors, in front, answers that. Anything
// else is the auth's Unauthorized, a bodyless 401 naming the resource
// metadata, before the mux sees it, registered endpoint or not, so nothing
// under the prefix can be probed without it.
//
// Fails closed: with no cache, or no secret yet, nothing but ping, login,
// logout and the resource metadata answers. Compared in constant time, so the time a refusal takes says
// nothing about how much of a guess was right.
func (r *RouterImpl) authorize(next http.Handler) http.Handler {
	e := r.env
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		e.mu.Lock()
		c, a := e.cache, e.auth
		e.mu.Unlock()
		// Every answer under the ControlPath names the run, a refusal too.
		if c != nil {
			if key := c.Key(); key != "" {
				w.Header().Set(CacheKeyHeader, key)
			}
		}
		// And says the gate, as a 401 from an origin would (gated).
		if a != nil {
			w = &gated{ResponseWriter: w, gate: a.Header}
		}
		name := strings.TrimPrefix(req.URL.Path, ControlPath)
		if slices.Contains(authMethods[name], req.Method) {
			switch name {
			case ".env":
				// Only with a live grant; anything else meets the secret below.
				if bearer, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok && c != nil && c.Grant(bearer) {
					next.ServeHTTP(w, req)
					return
				}
			default:
				next.ServeHTTP(w, req)
				return
			}
		}
		// Anything else needs the secret.
		var secret []byte
		if c != nil {
			secret = c.Secret()
		}
		want := "token " + base64.StdEncoding.EncodeToString(secret)
		if len(secret) > 0 && subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte(want)) == 1 {
			next.ServeHTTP(w, req)
			return
		}
		if a == nil {
			a = fallback
		}
		a.Unauthorized(w, req)
	})
}

// fallback is what a router handed no auth refuses through, so its 401 still
// carries a challenge, and serves the metadata that challenge names.
var fallback auth.Auth = auth.New()

// gated stamps an answer with X-Tunneld-Authenticate, the challenges in
// public: nothing a 401 would not say to anyone, said ahead so a caller (the
// provider's visibility) need not provoke one. Read as the status is
// written, so an answer that changed the gate, a grant's PATCH, says the new
// one; absent when the tunnel is public.
type gated struct {
	http.ResponseWriter
	gate    func() (key, value string)
	written bool
}

func (g *gated) WriteHeader(code int) {
	if !g.written {
		g.written = true
		if key, v := g.gate(); v != "" {
			g.ResponseWriter.Header().Set(key, v)
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gated) Write(b []byte) (int, error) {
	if !g.written {
		g.WriteHeader(http.StatusOK)
	}
	return g.ResponseWriter.Write(b)
}

// Unwrap is for http.ResponseController: the MCP server flushes through it.
func (g *gated) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// unstored writes "Cache-Control: no-store" on any answer the router serves
// that says nothing about caching, as its status is written: tunneld is
// meant to feel like localhost, where nothing caches. Cloudflare's edge
// caches a cacheable extension an answer said nothing about for its default
// TTL, which served a dev server's stale CSS for four hours (#179), and a
// page of tunneld's own (the panel, a terminal, login, the control path) is
// a live view nobody should be handed a copy of. An answer that does say
// keeps what it said: an origin's, or .env's no-transform.
type unstored struct {
	http.ResponseWriter
	written bool
}

func (u *unstored) WriteHeader(code int) {
	if !u.written {
		u.written = true
		if u.ResponseWriter.Header().Get("Cache-Control") == "" {
			u.ResponseWriter.Header().Set("Cache-Control", "no-store")
		}
	}
	u.ResponseWriter.WriteHeader(code)
}

func (u *unstored) Write(b []byte) (int, error) {
	if !u.written {
		u.WriteHeader(http.StatusOK)
	}
	return u.ResponseWriter.Write(b)
}

// Unwrap is for http.ResponseController: the proxy flushes and upgrades
// through it.
func (u *unstored) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// authMethods is every path under the ControlPath a request reaches without
// the secret, by its name there and the methods it may use: ping, login,
// logout and the resource metadata, which a visitor without the secret asks,
// and .env, which the owner's browser
// PATCHes with a live grant the cache issued to the secret's holder.
var authMethods = map[string][]string{
	"ping":   {http.MethodGet, http.MethodHead},
	"login":  {http.MethodGet, http.MethodHead, http.MethodPost},
	"logout": {http.MethodGet, http.MethodHead, http.MethodPost},
	".env":   {http.MethodPatch},
}

// corsMethods is every path under the ControlPath a browser calls across
// origins, by its name there and the methods its preflight allows: .env,
// which the provider's page PATCHes with a grant.
var corsMethods = map[string][]string{
	".env": {http.MethodPatch},
}

// cors stands in front of the ControlPath's guard for a browser calling
// across origins. On a path corsMethods has, every answer names the
// provider's origin, and no other, a refusal included, so the browser can
// read why; and a preflight, which carries no credentials by design, is
// answered here, before the guard would refuse it. Every other path, and an
// OPTIONS that is no preflight, goes on to next untouched.
func (r *RouterImpl) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		methods, ok := corsMethods[strings.TrimPrefix(req.URL.Path, ControlPath)]
		if !ok {
			next.ServeHTTP(w, req)
			return
		}
		w.Header().Add("Vary", "Origin")
		provider := false
		if o := req.Header.Get("Origin"); o != "" && o == r.allowOrigin {
			provider = true
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Expose-Headers", v1.AuthenticateHeader+", Retry-After")
		}
		if req.Method == http.MethodOptions && req.Header.Get("Access-Control-Request-Method") != "" {
			if provider {
				w.Header().Set("Access-Control-Allow-Methods", strings.Join(methods, ", "))
				w.Header().Set("Access-Control-Allow-Headers", "authorization, content-type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// metadata answers auth.MetadataPath from the origins first, and with a's
// RFC 9728 metadata only when they have none there: a 404, or nothing
// listening. Anything else the origin says, a 401 from protection of its own
// included, is the origin's, so tunneld never stands in front of an origin
// that guards itself. GET and HEAD only; any other method is the origin's
// whatever it answers.
func metadata(next http.Handler, a auth.Auth) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != auth.MetadataPath || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		h := &held{ResponseWriter: w, header: http.Header{}}
		next.ServeHTTP(h, r)
		if h.missing {
			a.ResourceMetadata(w, r)
		}
	})
}

// held keeps an answer's headers back until its status says whether there
// was one: a 404, or tunneld's own answer for an origin nothing listens on,
// is missing and dropped whole; anything else goes out as it came.
// Informational answers are dropped: what follows them decides.
type held struct {
	http.ResponseWriter
	header           http.Header
	written, missing bool
}

func (h *held) Header() http.Header { return h.header }

func (h *held) WriteHeader(code int) {
	if h.written || code < http.StatusOK {
		return
	}
	h.written = true
	if code == http.StatusNotFound || h.header.Get(unreachableHeader) != "" {
		h.missing = true
		return
	}
	maps.Copy(h.ResponseWriter.Header(), h.header)
	h.ResponseWriter.WriteHeader(code)
}

func (h *held) Write(b []byte) (int, error) {
	if !h.written {
		h.WriteHeader(http.StatusOK)
	}
	if h.missing {
		return len(b), nil
	}
	return h.ResponseWriter.Write(b)
}

// FlushError is for http.ResponseController: the proxy flushes through it,
// and nothing goes out of an answer that is missing.
func (h *held) FlushError() error {
	if !h.written || h.missing {
		return nil
	}
	return http.NewResponseController(h.ResponseWriter).Flush()
}

// Cancel takes down every route this router has serving: their listeners
// close and their requests, upgraded ones included, end. A router that never
// routed has nothing to stop.
//
// Every route, so a router routing twice at once — an embedding program
// starting a run while the last one's tunnel still drains — is cancelled as
// one; each run that should outlive another wants a router of its own.
func (r *RouterImpl) Cancel() {
	if r.live == nil {
		return
	}
	r.live.mu.Lock()
	stops := r.live.stops
	r.live.stops = nil
	r.live.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

// proxy relays each request to one of origins, relaying the response verbatim
// — status, headers, body untouched — and dialing https origins without
// verifying them, the way the tunnel engine dials an origin it is handed.
//
// With more than one origin the index is resolved as: a bare numeric query
// parameter (?n, empty value, dropped from the forwarded query), else — for a
// WebSocket handshake — the origin declared to own WebSockets (ws, the +ws
// marker, -1 for none), else a same-host Referer carrying one (an iframe's or
// page's subresources follow their document URL — per-tab, no shared state),
// else a same-host Referer naming a subresource this router already routed
// (see remembered: a stylesheet's font, a script's import), else the sticky
// Cookie, else origins[0]. The declaration sits above the
// cookie deliberately: the cookie is a per-browser guess, the declaration an
// operator-stated fact, and a fact beats a guess. It sits below an explicit
// parameter so a page carrying its own index — and every tile of a multiview
// panel — is unaffected. An explicit parameter on a top-level navigation
// answers with the sticky cookie so parameter-less follow-ups stay on the
// same origin — an iframe's pick does not, or side-by-side iframes would
// fight over the shared jar. Anything out of range falls back to origins[0].
// A single origin skips all of it: a bare numeric parameter is then the
// application's own.
//
// The inbound Host is kept: an origin may key on it, and the stdlib default
// would rewrite it to the origin's own host.
func proxy(origins []*url.URL, ws int, log *slog.Logger) http.Handler {
	paths := &remembered{origin: map[string]int{}}
	// index is the origin a request was routed to, found in the list by the
	// host it was sent to rather than read off the request; -1 for none.
	index := func(host string) int {
		for i, o := range origins {
			if o.Host == host {
				return i
			}
		}
		return -1
	}
	return &httputil.ReverseProxy{
		Transport: transport(origins),
		Rewrite: func(r *httputil.ProxyRequest) {
			origin := origins[0]
			if len(origins) > 1 {
				// A segment Atoi accepts is exactly a bare numeric parameter:
				// valued ones ("1=foo") carry '=' and fail the parse. The first
				// wins; every routing segment is dropped from the forward.
				ix, explicit := 0, false
				kept := make([]string, 0, 4)
				for seg := range strings.SplitSeq(r.In.URL.RawQuery, "&") {
					if n, err := strconv.Atoi(seg); err == nil {
						if !explicit {
							ix, explicit = n, true
						}
						continue
					}
					if seg != "" {
						kept = append(kept, seg)
					}
				}
				upgrade := r.In.Header.Get("Upgrade") != ""
				// followed is whether the pick came from the request's own
				// signals — its index, or a Referer that names one, directly
				// or through a path already routed — which is what a path is
				// remembered for. A cookie or the default is a guess, and a
				// guess remembered would outlive being wrong.
				followed := explicit
				if !explicit {
					switch n, ok := refererIndex(r.In); {
					case upgrade && ws >= 0:
						// A handshake carries no Referer and no per-tab signal
						// of any kind, so the declaration is the only thing
						// that can route it.
						ix = ws
					case ok:
						ix, followed = n, true
					default:
						if n, ok := paths.of(r.In); ok {
							ix, followed = n, true
							break
						}
						cookie, err := r.In.Cookie(Cookie)
						if err == nil {
							if n, err := strconv.Atoi(cookie.Value); err == nil {
								ix = n
							}
						}
						if upgrade && err != nil {
							// Nothing to route on: no parameter, no
							// declaration, no cookie. It still goes to origin
							// 0 — a client explicit enough to be broken by a
							// refusal is working by luck today — but silence
							// here is the worst available failure: the page
							// loads, the socket connects to the wrong origin,
							// the app half-works, and the tunnel is the last
							// thing anybody suspects (cnuss/libtunnel#159).
							log.Warn("websocket could not be routed and fell back to the default origin; mark the origin that owns websockets with the +ws scheme suffix (http+ws://host)",
								"url", r.In.URL.String(), "origin", origins[0].Redacted())
						}
					}
				}
				if ix < 0 || ix >= len(origins) {
					ix = 0
				}
				if followed {
					paths.keep(r.In, ix)
				}
				origin = origins[ix]
				r.Out.URL.RawQuery = strings.Join(kept, "&")
				if explicit && navigation(r.In) {
					// ModifyResponse below answers an explicit top-level pick
					// with the sticky cookie; the outbound context carries the
					// index over.
					r.Out = r.Out.WithContext(context.WithValue(r.Out.Context(), stickyKey{}, ix))
				}
				log.Debug("routing to origin", "ix", ix, "url", r.In.URL.String())
			}
			// Scheme and host only: an origin typed with a path is reached at
			// its root, the way the tunnel engine reached it.
			r.SetURL(&url.URL{Scheme: origin.Scheme, Host: origin.Host})
			r.Out.Host = r.In.Host
			// The forwarding headers, as they arrived. With Rewrite set the
			// stdlib strips them from the outbound request, and an origin that
			// builds absolute URLs from X-Forwarded-Proto then falls back to
			// its own scheme — plain http on a dev server — and an OAuth
			// redirect_uri comes out http:// against an https:// callback
			// (#201). They come from cloudflared, the one hop in front of
			// this, so they are passed on untouched. Not SetXForwarded: that
			// derives the scheme from this server's own inbound connection,
			// which is plain http from cloudflared, and would say http again.
			for _, h := range forwarding {
				if v, ok := r.In.Header[h]; ok {
					r.Out.Header[h] = v
				}
			}
		},
		// An origin that cannot be reached is the one failure here an
		// operator can act on, so it is said at warn, naming the origin. A
		// visitor who went away first is not a failure of anything.
		//
		// Nothing the visitor sent reaches the line, or the answer below. The
		// origin is named from the list, found by the host the request was
		// routed to rather than read off it, and the error is the dial's, not
		// the url.Error around it that carries the request's path and query,
		// with any line break taken out besides.
		//
		// When nothing answered the dial at all — refused, no route, nothing
		// within the dial's timeout — the visitor gets a page saying so. The
		// router says nothing on the console: the warn line is its report,
		// and what the operator sees is the builder's to show. Before,
		// tunneld :3999 with nothing listening answered a bodiless 502, and
		// the edge painted its own "Bad gateway · Host Error" page over it.
		//
		// A 503 rather than a 502, because the edge replaces an origin's 502
		// with its own page and is expected to pass a 503 through with its
		// body — to be confirmed live. It is also the truer status: the
		// service behind the address is unavailable, and Retry-After says how
		// soon to ask again. Any other failure — an origin that took the
		// connection and hung up, a malformed answer — stays a bare 502:
		// something is listening there, and a page saying nothing is would be
		// wrong.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			ix := index(r.URL.Host)
			origin := "unknown"
			if ix >= 0 {
				origin = origins[ix].Redacted()
			}
			if errors.Is(err, context.Canceled) {
				log.Debug("request ended before the origin answered", "origin", origin)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			var dial *net.OpError
			unanswered := ix >= 0 && errors.As(err, &dial) && dial.Op == "dial"
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			reason := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\n", " "), "\r", " ")
			log.Warn("origin did not answer", "origin", origin, "err", reason)
			if !unanswered {
				w.WriteHeader(http.StatusBadGateway)
				return
			}

			// The page is for a browser loading one: a GET or HEAD that takes
			// HTML, for a document or a frame rather than a fetch or an asset
			// (no Sec-Fetch-Dest is a client from before the header, and
			// counts), and never a WebSocket handshake. Anything else gets the
			// same news in a line of text. Either way it is never stored, so a
			// visit after the origin is up gets the origin.
			host := origins[ix].Host
			// What the page and the line tell someone to start something
			// on: "port 3999" for an origin on this machine, which is how a
			// person says it and what a dev server is started on, and the
			// host whole for one elsewhere. Both are a call to act, not a
			// diagnosis: whoever shared the link starts it, and whoever was
			// sent it asks them to.
			where := host
			if name, port, err := net.SplitHostPort(host); err == nil {
				if ip := net.ParseIP(name); name == "localhost" || ip != nil && ip.IsLoopback() {
					where = "port " + port
				}
			}
			h := w.Header()
			h.Set("Retry-After", strconv.Itoa(retryAfter))
			h.Set(unreachableHeader, host)
			dest := r.Header.Get("Sec-Fetch-Dest")
			if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
				r.Header.Get("Upgrade") == "" &&
				strings.Contains(strings.Join(r.Header.Values("Accept"), ","), "text/html") &&
				(dest == "" || dest == "document" || dest == "iframe" || dest == "frame") {
				var page bytes.Buffer
				err := unreachableTmpl.Execute(&page, struct {
					Where, Mark string
					RetryAfter  int
				}{where, unreachableHeader, retryAfter})
				if err == nil {
					h.Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write(page.Bytes())
					return
				}
				log.Error("unreachable page render failed", "error", err)
			}
			h.Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "nothing is running on %s yet: start a process on it, or ask whoever shared this address to\n", where)
		},
		// Two jobs on the way back. A response the origin said nothing about
		// caching goes out no-store (#179). And with several origins, an
		// explicit top-level pick is answered with the sticky cookie; Rewrite
		// put the index on the outbound context for it, and only then.
		ModifyResponse: func(resp *http.Response) error {
			if ix, ok := resp.Request.Context().Value(stickyKey{}).(int); ok {
				cookie := &http.Cookie{Name: Cookie, Value: strconv.Itoa(ix), Path: "/"}
				resp.Header.Add("Set-Cookie", cookie.String())
			}
			return nil
		},
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
}

// redirect canonicalizes referer-routed navigations onto an explicit ?n URL,
// defending referer routing against decay: a GET/HEAD document or iframe
// navigation with no routing parameter of its own but a same-host referer
// that carries one (a link click inside a routed page) is answered 307 to the
// same URL plus that parameter. The new document's URL then re-pins the
// origin, so its own subresources — whose Referer is the new URL — keep
// routing instead of falling back to the default. A single origin passes
// everything through.
func redirect(n int, next http.Handler) http.Handler {
	if n < 2 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dest := r.Header.Get("Sec-Fetch-Dest")
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			(dest == "document" || dest == "iframe" || dest == "frame") {
			if _, explicit := bareIndex(r.URL.RawQuery); !explicit {
				// A path opening "//" (or "/\", which browsers normalize to
				// it) would echo into Location as a scheme-relative absolute
				// URL — an open redirect off the tunnel host. Those
				// navigations proxy un-canonicalized.
				if ix, ok := refererIndex(r); ok &&
					!strings.HasPrefix(r.URL.Path, "//") && !strings.HasPrefix(r.URL.Path, "/\\") {
					u := *r.URL
					if u.RawQuery != "" {
						u.RawQuery += "&"
					}
					u.RawQuery += strconv.Itoa(ix)
					http.Redirect(w, r, u.RequestURI(), http.StatusTemporaryRedirect)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// remembered is the origin each subresource path was routed to, by the
// request's own signals, so a request whose Referer is that subresource can
// follow it there. A page's own subresources carry the page's URL as their
// Referer and route by its ?n; what those subresources load in turn — a font
// from a stylesheet, an @import, a module a script imports — carries the
// subresource's URL instead, which has no ?n, and without this fell through
// to the cookie or origin 0: a stylesheet served by origin 1 whose font was
// asked of origin 0.
//
// Only subresources, and never the root: every tile's document is "/", or a
// page that redirect gives its own ?n, so a document's path names no origin.
// Keyed by path alone; two origins serving the same path is the one case this
// cannot tell apart, and the more recent routing wins. Bounded, since a run
// may serve for weeks: past maxRemembered an arbitrary path is forgotten for
// each one kept.
type remembered struct {
	mu     sync.Mutex
	origin map[string]int
}

// maxRemembered bounds a router's remembered paths.
var maxRemembered = 4096

// keep remembers which origin r's path was routed to, if r is a subresource.
func (p *remembered) keep(r *http.Request, ix int) {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "", "document", "iframe", "frame":
		return
	}
	if r.URL.Path == "/" || r.Header.Get("Upgrade") != "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.origin[r.URL.Path]; !ok && len(p.origin) >= maxRemembered {
		for path := range p.origin {
			delete(p.origin, path)
			break
		}
	}
	p.origin[r.URL.Path] = ix
}

// of is the origin r's same-host Referer was routed to, when its Referer is a
// path this router remembers.
func (p *remembered) of(r *http.Request) (int, bool) {
	ref, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || ref.Host != r.Host || ref.Path == "" {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ix, ok := p.origin[ref.Path]
	return ix, ok
}

// refererIndex resolves the routing index from a same-host Referer header:
// the document URL of the page (or iframe) the request originates from, whose
// query carries the bare ?n parameter. A cross-host referer never routes.
func refererIndex(r *http.Request) (int, bool) {
	ref, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || ref.Host != r.Host {
		return 0, false
	}
	return bareIndex(ref.RawQuery)
}

// bareIndex scans a raw query for the first bare numeric segment — the ?n
// routing directive. Valued parameters ("1=foo") carry '=' and fail the
// parse: application data, never routing.
func bareIndex(rawQuery string) (int, bool) {
	for seg := range strings.SplitSeq(rawQuery, "&") {
		if n, err := strconv.Atoi(seg); err == nil {
			return n, true
		}
	}
	return 0, false
}

// navigation reports whether a request is a top-level navigation — the only
// kind whose explicit ?n pick may write the tab-wide sticky cookie.
// Sec-Fetch-Dest names it outright in a modern browser; an absent header is
// treated as one so a client that predates the header (curl, an old browser)
// can still pin an origin. A WebSocket handshake is the exception that forces
// the check: it carries no Sec-Fetch-Dest at all, so the absent case used to
// catch every socket, letting whichever socket connected last re-pin every
// later parameter-less request (cnuss/libtunnel#159). An upgrade is never a
// navigation, whatever else it omits.
func navigation(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	dest := r.Header.Get("Sec-Fetch-Dest")
	return dest == "" || dest == "document"
}

// forwarding is the headers a proxy in front of the origins says where a
// request came from and how, which Rewrite passes on as they arrived.
var forwarding = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// stickyKey carries an explicit routing pick from Rewrite to ModifyResponse
// on the outbound request context.
type stickyKey struct{}

// transport dials the origins, adding TLS without verification when any
// origin is https — an origin on this machine presents whatever certificate
// its dev server made up, and the tunnel engine never verified one either.
// The TLS config only engages on https dials, so http origins share it.
//
// A clone of the default rather than a bare Transport, which would have no
// dial or handshake timeout and never reap an idle connection: one https dev
// server that hangs on accept would then hold requests to every origin behind
// this router.
func transport(origins []*url.URL) http.RoundTripper {
	for _, u := range origins {
		if u.Scheme == "https" {
			t := http.DefaultTransport.(*http.Transport).Clone()
			t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			return t
		}
	}
	return http.DefaultTransport
}
