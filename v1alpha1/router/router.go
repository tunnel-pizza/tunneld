// Package router is what puts several origins behind one tunnel hostname: a
// loopback server in front of them that decides, per request, which one it
// reaches, with the multiview panel and the framing-header scrub in front of
// that.
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
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"

	v1 "github.com/tunnel-pizza/tunneld/v1"
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
const ControlPath = "/_tunneld/"

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
	// handler wraps the routing handler: the display's panel and its framing
	// scrub, which answer or reshape a request before any origin is chosen.
	// Nil is nothing in front.
	handler func(http.Handler) http.Handler
	log     v1.Logger
	// mux answers the ControlPath: the router's own endpoints, registered on
	// the mux New makes. Shared by every route, since no route registers on
	// it.
	mux *http.ServeMux

	// live is what Cancel stops: every route this router has serving. A
	// pointer, so the copy each Route configures still reaches it.
	live *routes
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
func New(opts ...Option) *RouterImpl {
	mux := http.NewServeMux()
	// Answered here, by tunneld, and never by an origin: a 200 says the edge,
	// the tunnel and the router are all up, whatever state the origins are in.
	// Not stored anywhere on the way, so every ping is asked of this process.
	mux.HandleFunc("GET "+ControlPath+"ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "pong")
	})
	return v1.Apply(&RouterImpl{ws: -1, log: discard, mux: mux, live: &routes{}}, opts...)
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

// WithHandler sets what wraps the routing handler — the display's Panel — or
// nothing, when nil.
func WithHandler(handler func(http.Handler) http.Handler) Option {
	return func(r *RouterImpl) { r.handler = handler }
}

// WithLog sets where the router says what it did. Nil keeps the one it has.
func WithLog(log v1.Logger) Option {
	return func(r *RouterImpl) {
		if log != nil {
			r.log = log
		}
	}
}

// Origins, WebSockets and Handler read back what the options set, for a caller
// standing in for a router that wants to see what it was handed without
// standing one up.
func (r *RouterImpl) Origins() v1.Origins                      { return r.dialable }
func (r *RouterImpl) WebSockets() int                          { return r.ws }
func (r *RouterImpl) Handler() func(http.Handler) http.Handler { return r.handler }

// Route stands a loopback server up in front of the origins and returns the
// one address the tunnel should forward to.
//
// opts are this route's, applied over the router's own on a copy: what New
// was given stays the default for every route, and one route's origins never
// leak into the next.
//
// One origin with nothing in front of it needs no router: its own address is
// returned and nothing is served, which is the tunnel exactly as it was
// before there was routing to do — and so no ControlPath either.
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
	dialable, ws, front, log := route.dialable, route.ws, route.handler, route.log
	if dialable == nil || dialable.Len() == 0 {
		return nil, errors.New("router: no origins to route to")
	}
	if dialable.Len() == 1 && front == nil {
		return dialable.At(0), nil
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("router: listen: %w", err)
	}
	var h http.Handler = redirect(dialable.Len(), proxy(dialable.URLs(), ws, log))
	origins, mux := h, route.mux
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, ControlPath) {
			mux.ServeHTTP(w, r)
			return
		}
		origins.ServeHTTP(w, r)
	})
	if front != nil {
		h = front(h)
	}
	routing, stop := context.WithCancel(context.WithoutCancel(ctx))
	srv := &http.Server{
		Handler:     h,
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

// Cancel takes down every route this router has serving: their listeners
// close and their requests, upgraded ones included, end. A router with none —
// never routed, or routed a lone origin it did not stand in front of — has
// nothing to stop.
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
// else the sticky Cookie, else origins[0]. The declaration sits above the
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
	p := &httputil.ReverseProxy{
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
				if !explicit {
					switch n, ok := refererIndex(r.In); {
					case upgrade && ws >= 0:
						// A handshake carries no Referer and no per-tab signal
						// of any kind, so the declaration is the only thing
						// that can route it.
						ix = ws
					case ok:
						ix = n
					default:
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
		},
		// An origin that cannot be reached is the one failure here an
		// operator can act on, and the edge shows it as a bare 502 with no
		// word of which origin or why: tunneld :3000 :4000 with :4000 not up
		// yet, and /?1 is a blank page. So it is said at warn, naming the
		// origin. A visitor who went away first is not a failure of anything.
		//
		// Nothing the visitor sent reaches the line. The origin is named from
		// the list, found by the host the request was routed to rather than
		// read off it, and the error is the dial's, not the url.Error around
		// it that carries the request's path and query, with any line break
		// taken out besides.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			origin := "unknown"
			for _, o := range origins {
				if o.Host == r.URL.Host {
					origin = o.Redacted()
					break
				}
			}
			if errors.Is(err, context.Canceled) {
				log.Debug("request ended before the origin answered", "origin", origin)
			} else {
				var ue *url.Error
				if errors.As(err, &ue) {
					err = ue.Err
				}
				reason := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\n", " "), "\r", " ")
				log.Warn("origin did not answer", "origin", origin, "err", reason)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	if len(origins) > 1 {
		p.ModifyResponse = func(resp *http.Response) error {
			if ix, ok := resp.Request.Context().Value(stickyKey{}).(int); ok {
				cookie := &http.Cookie{Name: Cookie, Value: strconv.Itoa(ix), Path: "/"}
				resp.Header.Add("Set-Cookie", cookie.String())
			}
			return nil
		}
	}
	return p
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
