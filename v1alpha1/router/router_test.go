package router

// The routing rules moved here from libtunnel's reverse proxy (#176), and
// their tests came with them: each builds the router directly and speaks
// plain HTTP to its listener — no tunnel, no edge.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
)

// The contract this implements is asserted in v1alpha1, which imports this
// package; naming it here would be the cycle.

// discard is the logger for cases that assert nothing about logs.
var discard = slog.New(slog.DiscardHandler)

// echo is an origin that answers with its name and the query it was
// forwarded, which is everything routing can change about a request.
func echo(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s", name, r.URL.RawQuery)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// listOf is the servers as an origin list, the way the binder hands one over.
func listOf(t *testing.T, ws int, srvs ...*httptest.Server) v1.Origins {
	t.Helper()
	urls := make([]*url.URL, len(srvs))
	for i, srv := range srvs {
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		urls[i] = u
	}
	return origins.New(origins.WithURL(urls...), origins.WithWebSocket(ws))
}

// route stands the router up over list for the life of the test and returns
// the base URL a client dials.
func route(t *testing.T, list v1.Origins, front func(http.Handler) http.Handler, log *slog.Logger) string {
	t.Helper()
	u, err := New().Route(t.Context(), list, front, log)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	return strings.TrimSuffix(u.String(), "/")
}

// get sends one request with the headers a case names and returns the body.
func get(t *testing.T, client *http.Client, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// TestRouteALoneOriginIsItsOwnAddress pins that one origin with nothing in
// front of it is not routed at all: the tunnel is handed the origin itself,
// exactly as before there was routing to do, and a bare numeric parameter is
// the application's own.
func TestRouteALoneOriginIsItsOwnAddress(t *testing.T) {
	list := listOf(t, -1, echo(t, "solo"))
	got, err := New().Route(t.Context(), list, nil, discard)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got != list.At(0) {
		t.Errorf("Route() = %v, want the origin itself (%v)", got, list.At(0))
	}
}

// TestRouteNothingIsAnError pins that an empty list is refused rather than
// served: there is nothing a request could reach.
func TestRouteNothingIsAnError(t *testing.T) {
	if _, err := New().Route(t.Context(), origins.New(), nil, discard); err == nil {
		t.Error("Route() over no origins succeeded, want an error")
	}
}

// TestRouteAppliesTheFront pins that what is put in front answers before any
// origin is chosen — the panel's page never reaches an origin — and that a
// lone origin is still served through it rather than skipped.
func TestRouteAppliesTheFront(t *testing.T) {
	front := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/front" {
				fmt.Fprint(w, "front")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	base := route(t, listOf(t, -1, echo(t, "solo")), front, discard)
	for path, want := range map[string]string{"/front": "front", "/?x": "solo|x"} {
		req, _ := http.NewRequest("GET", base+path, nil)
		if _, body := get(t, http.DefaultClient, req); body != want {
			t.Errorf("GET %s = %q, want %q", path, body, want)
		}
	}
}

// TestRouteEndsWithTheContext pins the router's lifetime: it serves while the
// run does and stops answering once the run's context ends.
func TestRouteEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	u, err := New().Route(ctx, listOf(t, -1, echo(t, "A"), echo(t, "B")), nil, discard)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if _, err := http.Get(u.String()); err != nil {
		t.Fatalf("GET while live: %v", err)
	}
	cancel()
	// Close runs on the AfterFunc goroutine, so give it the one round trip it
	// needs rather than a sleep.
	for range 100 {
		if _, err := http.Get(u.String()); err != nil {
			return
		}
	}
	t.Error("the router still answers after its context ended")
}

// TestRoutePreservesTheHost pins that the Host a request arrived with is the
// one the origin sees: an origin may key on it, and the stdlib default would
// rewrite it to the origin's own address.
func TestRoutePreservesTheHost(t *testing.T) {
	host := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s|%s", name, r.Host)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	base := route(t, listOf(t, -1, host("A"), host("B")), nil, discard)
	req, _ := http.NewRequest("GET", base+"/?1", nil)
	req.Host = "foo.tunneled.pizza"
	if _, body := get(t, http.DefaultClient, req); body != "B|foo.tunneled.pizza" {
		t.Errorf("body = %q, want the inbound Host at origin 1", body)
	}
}

// TestRouteDialsHTTPSUnverified pins that an https origin is reached however
// its certificate reads: an origin on this machine presents whatever its dev
// server made up.
func TestRouteDialsHTTPSUnverified(t *testing.T) {
	tlsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "tls")
	}))
	t.Cleanup(tlsOrigin.Close)
	base := route(t, listOf(t, -1, echo(t, "A"), tlsOrigin), nil, discard)
	req, _ := http.NewRequest("GET", base+"/?1", nil)
	if _, body := get(t, http.DefaultClient, req); body != "tls" {
		t.Errorf("body = %q, want the https origin's answer", body)
	}
}

// TestMultiOriginRouting pins the routing contract: a bare numeric query
// param (?n, empty value) routes the request to origins[n] and sets the
// sticky cookie; the sticky cookie routes param-less requests; the routing
// param never reaches the origin; anything out of range, non-numeric, or
// carrying a value falls through to origins[0].
func TestMultiOriginRouting(t *testing.T) {
	base := route(t, listOf(t, -1, echo(t, "A"), echo(t, "B")), nil, discard)

	for name, tc := range map[string]struct {
		path     string
		cookie   string // inbound sticky cookie value; "" = none
		referer  string // Referer header; a bare path is prefixed with base
		dest     string // Sec-Fetch-Dest header; "" = not sent
		upgrade  bool   // send a WebSocket handshake's Upgrade/Connection pair
		wantBody string // "<origin name>|<forwarded raw query>"
		wantSet  string // expected Set-Cookie value; "" = no Set-Cookie
	}{
		"naked":                  {path: "/", wantBody: "A|"},
		"explicitSecond":         {path: "/?1", wantBody: "B|", wantSet: "1"},
		"explicitKeepsOthers":    {path: "/?1&x=y", wantBody: "B|x=y", wantSet: "1"},
		"valuedParamNotRouting":  {path: "/?1=foo", wantBody: "A|1=foo"},
		"nonNumericNotRouting":   {path: "/?abc", wantBody: "A|abc"},
		"stickyCookie":           {path: "/", cookie: "1", wantBody: "B|"},
		"explicitBeatsCookie":    {path: "/?0", cookie: "1", wantBody: "A|", wantSet: "0"},
		"outOfRangeFallsBack":    {path: "/?9", wantBody: "A|", wantSet: "0"},
		"garbageCookieFallsBack": {path: "/", cookie: "x", wantBody: "A|"},

		// Referer routing: a same-host referer whose query carries the bare
		// parameter routes the request — an iframe's (or page's) subresources
		// follow their document URL without touching the shared cookie.
		"refererRoutesSubresource": {path: "/asset.js", referer: "/?1", wantBody: "B|"},
		"refererBeatsCookie":       {path: "/", cookie: "0", referer: "/?1", wantBody: "B|"},
		"paramBeatsReferer":        {path: "/?0", referer: "/?1", wantBody: "A|", wantSet: "0"},
		"crossHostRefererIgnored":  {path: "/", referer: "https://evil.example/?1", wantBody: "A|"},
		"valuedRefererNotRouting":  {path: "/", referer: "/?1=foo", wantBody: "A|"},

		// The sticky cookie is a top-level concern: an explicit pick inside an
		// iframe must not churn the tab-wide jar (two side-by-side iframes
		// would fight over it).
		"iframeExplicitNoSticky":   {path: "/?1", dest: "iframe", wantBody: "B|"},
		"documentExplicitStickies": {path: "/?1", dest: "document", wantBody: "B|", wantSet: "1"},

		// A WebSocket handshake carries no Sec-Fetch-Dest at all, which the
		// sticky-cookie branch used to read as "a top-level navigation". A
		// socket is not a navigation: it must route on its own ?n but leave
		// the tab-wide cookie alone, or the last socket to connect re-pins
		// every later parameter-less request (cnuss/libtunnel#159).
		"websocketExplicitNoSticky": {path: "/sock?1", upgrade: true, wantBody: "B|"},
		"websocketFollowsCookie":    {path: "/sock", cookie: "1", upgrade: true, wantBody: "B|"},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest("GET", base+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: Cookie, Value: tc.cookie})
			}
			if tc.referer != "" {
				ref := tc.referer
				if !strings.HasPrefix(ref, "http") {
					ref = base + ref
				}
				req.Header.Set("Referer", ref)
			}
			if tc.dest != "" {
				req.Header.Set("Sec-Fetch-Dest", tc.dest)
			}
			if tc.upgrade {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			resp, body := get(t, http.DefaultClient, req)
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			gotSet := ""
			for _, c := range resp.Cookies() {
				if c.Name == Cookie {
					gotSet = c.Value
				}
			}
			if gotSet != tc.wantSet {
				t.Errorf("Set-Cookie %s = %q, want %q", Cookie, gotSet, tc.wantSet)
			}
		})
	}
}

// TestMultiOriginRedirect pins the canonicalizing redirect that defends
// referer routing against decay: a GET document/iframe navigation with no
// routing parameter of its own but a same-host referer that carries one is
// answered 307 to the same URL plus that parameter — the new document's URL
// re-pins the origin, so its own subresources keep routing. Everything else
// passes through to the proxy.
func TestMultiOriginRedirect(t *testing.T) {
	base := route(t, listOf(t, -1, echo(t, "A"), echo(t, "B")), nil, discard)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for name, tc := range map[string]struct {
		method       string
		path         string
		referer      string // bare path, prefixed with base
		dest         string
		wantStatus   int
		wantLocation string // when redirected
		wantBody     string // when proxied
	}{
		"iframeNavRedirects":   {method: "GET", path: "/page2?x=y", referer: "/?1", dest: "iframe", wantStatus: 307, wantLocation: "/page2?x=y&1"},
		"documentNavRedirects": {method: "GET", path: "/page2", referer: "/?1", dest: "document", wantStatus: 307, wantLocation: "/page2?1"},
		"zeroIndexRedirects":   {method: "GET", path: "/page2", referer: "/?0", dest: "iframe", wantStatus: 307, wantLocation: "/page2?0"},
		"explicitNoRedirect":   {method: "GET", path: "/page2?x=y&1", referer: "/?0", dest: "document", wantStatus: 200, wantBody: "B|x=y"},
		"noDestNoRedirect":     {method: "GET", path: "/page2?x=y", referer: "/?1", dest: "", wantStatus: 200, wantBody: "B|x=y"},
		"postNoRedirect":       {method: "POST", path: "/submit", referer: "/?1", dest: "document", wantStatus: 200, wantBody: "B|"},
		"noRefererNoRedirect":  {method: "GET", path: "/page2", referer: "", dest: "document", wantStatus: 200, wantBody: "A|"},

		// A path opening "//" (or the backslash variant browsers normalize to
		// it) would echo into Location as a scheme-relative absolute URL — an
		// open redirect. Those navigations proxy un-canonicalized instead.
		"schemeRelativeNoRedirect": {method: "GET", path: "//evil.example/x", referer: "/?1", dest: "document", wantStatus: 200, wantBody: "B|"},
		"backslashNoRedirect":      {method: "GET", path: "/\\evil.example/x", referer: "/?1", dest: "document", wantStatus: 200, wantBody: "B|"},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, base+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.referer != "" {
				req.Header.Set("Referer", base+tc.referer)
			}
			if tc.dest != "" {
				req.Header.Set("Sec-Fetch-Dest", tc.dest)
			}
			resp, body := get(t, noFollow, req)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantLocation != "" {
				if got := resp.Header.Get("Location"); got != tc.wantLocation {
					t.Errorf("Location = %q, want %q", got, tc.wantLocation)
				}
				return
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestWebSocketOriginRouting pins the +ws designation (cnuss/libtunnel#159):
// a handshake carries no Referer and no per-tab signal of any kind, so
// without a declaration it can only be guessed at. With one, an
// operator-stated fact beats the per-browser cookie guess — but never an
// explicit ?n, so a page that carries its own index (and every iframe in a
// multiview panel) is unaffected. Non-upgrade traffic ignores it entirely.
func TestWebSocketOriginRouting(t *testing.T) {
	// origins[1] owns WebSockets.
	base := route(t, listOf(t, 1, echo(t, "A"), echo(t, "B")), nil, discard)

	for name, tc := range map[string]struct {
		path     string
		cookie   string
		upgrade  bool
		wantBody string
	}{
		"unroutableSocketGoesToDeclaredOrigin": {path: "/hmr", upgrade: true, wantBody: "B|"},
		"declarationBeatsCookie":               {path: "/hmr", cookie: "0", upgrade: true, wantBody: "B|"},
		"explicitIndexBeatsDeclaration":        {path: "/sock?0", upgrade: true, wantBody: "A|"},
		"plainRequestIgnoresDeclaration":       {path: "/page", wantBody: "A|"},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest("GET", base+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: Cookie, Value: tc.cookie})
			}
			if tc.upgrade {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			if _, body := get(t, http.DefaultClient, req); body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestUnroutableWebSocketWarns pins the diagnosis (cnuss/libtunnel#159):
// with no declaration and nothing to route on, the handshake still falls back
// to origin 0 — a client explicit enough to be broken by a refusal is working
// by luck today — but it says so, naming the socket and its own fix. Silence
// is the worst available failure here: the page loads, the socket connects,
// the app half-works, and the tunnel is the last thing anybody suspects.
func TestUnroutableWebSocketWarns(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	base := route(t, listOf(t, -1, echo(t, "A"), echo(t, "B")), nil, logger)

	req, err := http.NewRequest("GET", base+"/hmr", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	get(t, http.DefaultClient, req)

	got := logs.String()
	if !strings.Contains(got, "/hmr") {
		t.Errorf("warning does not name the socket: %q", got)
	}
	if !strings.Contains(got, "+ws") {
		t.Errorf("warning does not name its own fix (+ws): %q", got)
	}
}
