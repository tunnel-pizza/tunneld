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
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Cookie is the sticky-routing cookie an explicit top-level pick answers
// with, so a parameter-less follow-up — an address-bar visit, a bookmark —
// stays on the origin somebody chose.
const Cookie = "tunneld-origin"

// Option configures a RouterImpl at construction.
type Option = v1.Option[*RouterImpl]

// RouterImpl is the default Router. It has no tunables yet; New takes the
// variadic so adding one changes no caller.
type RouterImpl struct{}

// New returns a RouterImpl configured by opts.
func New(opts ...Option) *RouterImpl {
	return v1.Apply(&RouterImpl{}, opts...)
}

// Route stands a loopback server up in front of dialable and returns the one
// address the tunnel should forward to. front, when not nil, wraps the
// routing handler — the panel and the scrub, which answer or reshape a
// request before any origin is chosen.
//
// One origin with nothing in front of it needs no router: its own address is
// returned and nothing is served, which is the tunnel exactly as it was
// before there was routing to do.
//
// The server lives as long as ctx. Upgraded connections — a WebSocket through
// the proxy — are not ended by closing the server, so the requests are based
// on ctx as well, and the proxy drops the origin side of a socket when its
// request's context ends.
func (*RouterImpl) Route(ctx context.Context, dialable v1.Origins, front func(http.Handler) http.Handler, log v1.Logger) (*url.URL, error) {
	if dialable.Len() == 0 {
		return nil, errors.New("router: no origins to route to")
	}
	if dialable.Len() == 1 && front == nil {
		return dialable.At(0), nil
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("router: listen: %w", err)
	}
	ws, ok := dialable.WebSocket()
	if !ok {
		ws = -1
	}
	var h http.Handler = redirect(dialable.Len(), proxy(dialable.URLs(), ws, log))
	if front != nil {
		h = front(h)
	}
	srv := &http.Server{
		Handler:     h,
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	context.AfterFunc(ctx, func() { _ = srv.Close() })
	go func() { _ = srv.Serve(l) }()

	local := &url.URL{Scheme: "http", Host: l.Addr().String()}
	log.Info("routing origins", "listen", local.Host, "origins", dialable.Len())
	return local, nil
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
func transport(origins []*url.URL) http.RoundTripper {
	for _, u := range origins {
		if u.Scheme == "https" {
			return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		}
	}
	return http.DefaultTransport
}
