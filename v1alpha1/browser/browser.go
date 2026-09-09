// Package browser puts a public address in front of a person: it launches
// whatever browser the host has, and it serves what that browser lands on
// when a tunnel carries more than one origin — the page that frames every
// origin, and the header surgery that lets those frames render.
//
// Its own subpackage because pkg/browser is a world the root has no reason to
// see, and because multiview.html travels with the code: go:embed cannot reach
// outside its own package directory.
package browser

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cnuss/libtunnel"
	pkgbrowser "github.com/pkg/browser"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Option configures a BrowserImpl at construction.
type Option = v1.Option[*BrowserImpl]

// BrowserImpl is the default browser: whatever the host has, launched as-is,
// pointed at the panel this same type serves below. Its launcher is seeded
// by New; a bare BrowserImpl{} has none and is not a supported construction.
type BrowserImpl struct {
	launch func(string) error
}

// New returns a BrowserImpl that launches the host's browser, then configured
// by opts.
func New(opts ...Option) *BrowserImpl {
	b := v1.Apply(&BrowserImpl{}, WithLaunch(pkgbrowser.OpenURL))
	return v1.Apply(b, opts...)
}

// WithLaunch replaces the browser launcher, so a test can observe the call
// without a window appearing on whoever is running it.
func WithLaunch(launch func(string) error) Option {
	return func(b *BrowserImpl) { b.launch = launch }
}

// pageHTML is the panel page: a rack panel of iframes, one per origin.
// Embedded rather than fetched, so the page is part of the binary and a
// tunnel serves it with nothing else installed and no outbound request.
//
//go:embed multiview.html
var pageHTML string

// pageTmpl is parsed once at init. A template that fails to parse is a build-time
// mistake in a file that ships inside the binary, so it panics here rather
// than surfacing as a 500 on somebody's first request.
var pageTmpl = template.Must(template.New("panel").Parse(pageHTML))

// pageData is what multiview.html renders from.
type pageData struct {
	// Host is the public hostname, taken from the request rather than the
	// tunnel, so the page names whatever address the visitor actually used.
	Host    string
	Origins []tile
}

// tile is one origin's tile in the panel: the index that routes to it, the
// local address it forwards to, and the relative URL that reaches it.
type tile struct {
	Index int
	Local string
	Route string
}

// Interceptors is what the tunnel registers when the panel is wanted: the
// page first, at the highest priority there is, and the scrubber behind it.
// The order is the contract — Command's RunE registers them in a loop and
// never looks at a priority itself.
//
// Nothing is registered when the panel is not wanted, so the caller registers
// what it is given without asking a second question first. It takes both the
// flag and something to compare: one origin framed alone is a worse view of
// it than the origin itself, so a lone origin keeps the bare address for
// itself. URL answers "" over exactly the same condition.
func (*BrowserImpl) Interceptors(enabled bool, origins []*url.URL, log v1.Logger) []libtunnel.Interceptor {
	if !enabled || len(origins) < 2 {
		return nil
	}
	return []libtunnel.Interceptor{
		{
			// Build the interceptor that serves the panel.
			//
			// Priority 1 is the highest there is, so nothing registered later can
			// shadow the one request that must never reach an origin.
			Priority: 1,
			// Report whether a request is somebody arriving at the tunnel itself,
			// which is what the panel answers. With several origins the bare
			// hostname has no better meaning: every origin has its own ?n, so the
			// address with nothing after it is the one that can show all of them.
			//
			// Three conditions narrow it, and each one is load-bearing:
			//
			// The path is exactly "/". An origin's own subresources — /app.js, /style.css
			// — carry no query either, and serving them a page of frames instead of the
			// file they asked for would break every origin that has any.
			//
			// The query is empty. Not merely free of a routing index: an app's root
			// legitimately takes parameters, and the caller often does not choose them.
			// An OAuth provider redirects to /?code=…&state=…, which carries no index and
			// would otherwise land on a page of frames with the sign-in silently lost.
			// Exposing an app mid-auth-flow is close to the median reason to reach for a
			// tunnel. The panel takes no parameters of its own, so it gives up nothing by
			// answering exactly one address.
			//
			// It is a top-level document, and it did not come from a page already on this
			// host. A frame navigating to "/" would otherwise draw the panel inside one of
			// the panel's own tiles, and a fetch() from an origin page would receive HTML
			// where it expected the origin's answer. A request with no Sec-Fetch-Dest at
			// all — curl, an older browser — counts as top-level, since nothing suggests
			// otherwise.
			//
			// And it is not an upgrade. That exception is what the empty-Sec-Fetch-Dest
			// case above costs: a WebSocket handshake sends no Sec-Fetch-Dest and no
			// Referer, so without this every handshake to the bare address matched, and an
			// app whose socket connects to "/" — xpra's client, and anything else dialing
			// the tunnel address itself rather than a subpath — was answered with the
			// panel's HTML instead of a handshake. It worked with --multiview=false and
			// failed with it on, which is not a shape anybody debugs quickly. An upgrade
			// is never a document, whatever else it omits.
			Match: func(r *http.Request) bool {
				if r.URL.Path != "/" || r.URL.RawQuery != "" {
					return false
				}
				if r.Header.Get("Upgrade") != "" {
					return false
				}
				switch r.Header.Get("Sec-Fetch-Dest") {
				case "document", "":
				default:
					return false
				}
				if referer, err := url.Parse(r.Header.Get("Referer")); err == nil && referer.Host == r.Host {
					return false
				}
				return true
			},
			Handler: func(ic libtunnel.InterceptCtx) libtunnel.InterceptCtx {
				return ic.WithHandler(func(w http.ResponseWriter, r *http.Request) {
					// Render the panel. A render failure is logged and answered with a
					// plain error rather than a half-written page: the response is
					// buffered by the template only up to the first write, so a partial
					// body is the one outcome worth avoiding.
					data := pageData{
						Host:    r.Host,
						Origins: make([]tile, 0, len(origins)),
					}
					for i, origin := range origins {
						// How a tile names the origin behind it: an http origin is named
						// by its host, because the scheme is the assumption and the host
						// is the thing the operator typed. Anything else keeps its
						// scheme, so a tile framing a container reads as dockerd://api
						// rather than as a bare hostname that happens to be a container
						// name.
						//
						// A +ws / +wss marker is dropped before that test, so the origin
						// that owns WebSockets is named exactly like every other HTTP
						// origin. The marker is routing configuration, not part of the
						// address — a tile shows what it frames, and which origin
						// sockets land on says nothing about that.
						scheme, _, _ := strings.Cut(origin.Scheme, "+")
						local := origin.Host
						if scheme != "http" && scheme != "https" {
							// A served origin's reference is its authority or
							// its path — a container name, a program's path —
							// and exactly one of the two is set.
							local = origin.Scheme + "://" + origin.Host + origin.Path
						}
						data.Origins = append(data.Origins, tile{
							Index: i,
							Local: local,
							// Relative, so the page works under whatever hostname served it.
							Route: "/?" + strconv.Itoa(i),
						})
					}

					var page strings.Builder
					if err := pageTmpl.Execute(&page, data); err != nil {
						log.Error("multiview render failed", "error", err)
						http.Error(w, "multiview: "+err.Error(), http.StatusInternalServerError)
						return
					}

					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					// The panel is a live view of whatever the origins are serving right now.
					w.Header().Set("Cache-Control", "no-store")
					if _, err := fmt.Fprint(w, page.String()); err != nil {
						log.Debug("multiview write failed", "error", err) // visitor went away
					}
				})
			},
		},
		{
			// Build the interceptor that lets the panel's own frames render.
			//
			// An origin is entitled to refuse being framed, and most that care say so
			// with X-Frame-Options: DENY or a CSP frame-ancestors directive. Through the
			// panel that refusal produces a blank tile, so the framing headers are
			// dropped — but only on the requests the panel itself makes, which is what
			// keeps this from being a blanket removal of somebody's clickjacking
			// protection.
			//
			// The narrowing is Sec-Fetch: the request must be a frame navigation
			// (Sec-Fetch-Dest) originating from this same tunnel (Sec-Fetch-Site). A
			// top-level visit keeps every header the origin sent, and so does an attempt
			// by another site to frame the tunnel — that arrives cross-site and is left
			// alone. A browser old enough not to send Sec-Fetch at all strips nothing,
			// which fails closed: a blank tile rather than a quietly weakened origin.
			//
			// Priority 2, behind the panel itself, so the page is served before
			// anything looks at framing.
			Priority: 2,
			// Report whether a request is one of the panel's own frames.
			Match: func(r *http.Request) bool {
				switch r.Header.Get("Sec-Fetch-Dest") {
				case "iframe", "frame":
				default:
					return false
				}
				return r.Header.Get("Sec-Fetch-Site") == "same-origin"
			},
			Handler: func(ic libtunnel.InterceptCtx) libtunnel.InterceptCtx {
				next := ic.Handler()
				return ic.WithHandler(func(w http.ResponseWriter, r *http.Request) {
					next(&asTile{ResponseWriter: w}, r)
				})
			},
		},
	}
}

// URL is the address the panel answers on: the tunnel's own URL, with nothing
// appended. Reported and opened as-is, and "" when there is no panel to
// answer — the same condition Interceptors registers nothing over, so the
// caller has one answer to read rather than a question to ask twice.
func (*BrowserImpl) URL(enabled bool, public *url.URL, origins []*url.URL) string {
	if !enabled || len(origins) < 2 {
		return ""
	}
	shown := *public
	shown.RawQuery = ""
	return shown.String()
}

// asTile drops the headers that stop an origin working inside a panel frame:
// the ones that refuse framing, and the one that suppresses the Referer its
// own assets are routed by. It scrubs at WriteHeader rather than after the
// fact because headers are immutable once written.
type asTile struct {
	http.ResponseWriter
	written bool
}

// Unwrap lets http.NewResponseController reach the real writer, so flushing
// and hijacking keep working through the wrapper — a streaming origin behind
// the panel would otherwise stall.
func (u *asTile) Unwrap() http.ResponseWriter { return u.ResponseWriter }

func (u *asTile) WriteHeader(code int) {
	if !u.written {
		u.written = true

		h := u.Header()

		// Remove Referrer-Policy, so the framed document goes back to the
		// browser default and its own subresources carry a Referer again.
		//
		// That Referer is not decoration: it is how the tunnel routes an
		// asset to the origin that asked for it. An origin sending
		// no-referrer — ordinary hardening, and what xpra's client does —
		// leaves every asset it requests with nothing to route by, so they
		// fall back to a cookie that is last-write-wins across tiles and get
		// served by whichever origin was framed most recently. The tile then
		// renders as unstyled markup, and the 404s come from an origin that
		// genuinely does not have those files, so nothing anywhere says the
		// routing went wrong.
		//
		// Dropped rather than rewritten because the browser default,
		// strict-origin-when-cross-origin, already sends the full URL for the
		// same-origin requests this is about and nothing more than the origin
		// for anything else. Narrowed the same way as the framing headers
		// below: only the panel's own frames, never a top-level visit.
		h.Del("Referrer-Policy")

		// Remove X-Frame-Options and CSP's frame-ancestors, and nothing else.
		// Both are dropped because frame-ancestors supersedes X-Frame-Options
		// in every current browser, so removing one alone would leave half
		// the origins blank; the rest of a policy — script-src, connect-src,
		// everything the origin relies on — is preserved directive by
		// directive.
		h.Del("X-Frame-Options")

		for _, key := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
			policies := h.Values(key)
			if len(policies) == 0 {
				continue
			}
			h.Del(key)
			for _, policy := range policies {
				// Return policy with any frame-ancestors directive removed,
				// or "" when that was the whole policy.
				kept := make([]string, 0, strings.Count(policy, ";")+1)
				for directive := range strings.SplitSeq(policy, ";") {
					trimmed := strings.TrimSpace(directive)
					if trimmed == "" {
						continue
					}
					name, _, _ := strings.Cut(trimmed, " ")
					if strings.EqualFold(name, "frame-ancestors") {
						continue
					}
					kept = append(kept, trimmed)
				}
				if result := strings.Join(kept, "; "); result != "" {
					h.Add(key, result)
				}
			}
		}
	}
	u.ResponseWriter.WriteHeader(code)
}

// Write covers the handler that never calls WriteHeader: without this the
// implicit 200 would be written by the wrapped writer, past the scrub.
func (u *asTile) Write(b []byte) (int, error) {
	if !u.written {
		u.WriteHeader(http.StatusOK)
	}
	return u.ResponseWriter.Write(b)
}

// Open launches a browser on addr. It does not wait, and the context is
// accepted only because the contract carries one.
//
// Waiting for the edge to serve addr is no longer this package's job. It used
// to probe over HTTP, which asked the right question from the wrong place: the
// tunnel's own event stream already knows when the edge has accepted a
// connection, so the counter holds that answer and the caller waits there
// before calling this. What is left is the launch, and the two promises around
// it.
//
// A failure goes to the debug log and nowhere else: the tunnel is up and
// serving either way, and a headless host — a server, a container, CI — is a
// normal place to run this, not a broken one. A warning on stderr told those
// runs, every time, about a thing they were never going to do. --no-open (or
// v1.NoOpenEnv) skips the attempt entirely, and --log-level=debug is where to
// look when a browser was wanted and none appeared.
//
// pkg/browser wires the spawned process's output to its package-level Stdout,
// which defaults to os.Stdout — the stream a running tunnel keeps for its
// addresses. Both are pointed at stderr before the child can write a word.
// They are package globals, so this is process-wide; tunneld owns its process,
// and an embedding program gets the same guarantee it wants anyway.
func (b *BrowserImpl) Open(_ context.Context, addr string, stderr io.Writer, log v1.Logger) {
	pkgbrowser.Stdout, pkgbrowser.Stderr = stderr, stderr
	if err := b.launch(addr); err != nil {
		log.Debug("could not open a browser", "url", addr, "error", err)
	}
}
