package router

// The routing rules moved here from libtunnel's reverse proxy (#176), and
// their tests came with them: each builds the router directly and speaks
// plain HTTP to its listener — no tunnel, no edge.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
)

// The contract this implements is asserted in v1alpha1, which imports this
// package; naming it here would be the cycle.

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
func listOf(t *testing.T, srvs ...*httptest.Server) v1.Origins {
	t.Helper()
	urls := make([]*url.URL, len(srvs))
	for i, srv := range srvs {
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		urls[i] = u
	}
	return origins.New(origins.WithURL(urls...))
}

// route stands the router up over list for the life of the test and returns
// the base URL a client dials. ws is the origin that owns WebSockets, -1 for
// none.
func route(t *testing.T, list v1.Origins, ws int, front func(http.Handler) http.Handler, log *slog.Logger) string {
	t.Helper()
	r := New()
	u, err := r.Route(t.Context(), WithOrigins(list), WithWebSockets(ws), WithHandler(front), WithLog(log))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	t.Cleanup(r.Cancel)
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

// TestRouteALoneOriginIsRouted pins that one origin with nothing in front of
// it is routed like any other run, so tunneld's control path answers on every
// tunnel — and that it gets none of the routing: a bare numeric parameter is
// the application's own and reaches it untouched.
func TestRouteALoneOriginIsRouted(t *testing.T) {
	list := listOf(t, echo(t, "solo"))
	r := New()
	got, err := r.Route(t.Context(), WithOrigins(list))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	t.Cleanup(r.Cancel)
	if got.Host == list.At(0).Host {
		t.Fatalf("Route() = %v, the origin itself; want the router in front of it", got)
	}
	base := strings.TrimSuffix(got.String(), "/")
	for path, want := range map[string]string{"/?1&x": "solo|1&x", "/_tunneld/ping": "pong"} {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if _, body := get(t, http.DefaultClient, req); body != want {
			t.Errorf("GET %s = %q, want %q", path, body, want)
		}
	}
}

// TestRouteNothingIsAnError pins that an empty list is refused rather than
// served, and so is no list at all: there is nothing a request could reach.
func TestRouteNothingIsAnError(t *testing.T) {
	if _, err := New().Route(t.Context(), WithOrigins(origins.New())); err == nil {
		t.Error("Route() over no origins succeeded, want an error")
	}
	if _, err := New().Route(t.Context()); err == nil {
		t.Error("Route() with no WithOrigins succeeded, want an error")
	}
}

// TestOptions pins where a route's facts come from: New's defaults, then what
// New was given, then what Route is given for that route alone — on a copy,
// so the next route starts from the router's own again.
func TestOptions(t *testing.T) {
	if r := New(); r.Origins() != nil || r.WebSockets() != -1 || r.Handler() != nil || r.mux == nil || r.log != discard {
		t.Errorf("New() = origins %v, ws %d, handler set %v, mux made %v, discarding %v; want none, -1, false, true, true",
			r.Origins(), r.WebSockets(), r.Handler() != nil, r.mux != nil, r.log == discard)
	}
	if r := New(WithLog(nil)); r.log != discard {
		t.Error("WithLog(nil) replaced the logger, want the one it had kept")
	}

	// Which origin a route reaches is what answers through it.
	reaches := func(t *testing.T, r *RouterImpl, opts ...Option) string {
		t.Helper()
		u, err := r.Route(t.Context(), opts...)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
		_, body := get(t, http.DefaultClient, req)
		return body
	}
	a, b := listOf(t, echo(t, "A")), listOf(t, echo(t, "B"))
	r := New(WithOrigins(a), WithWebSockets(0))
	t.Cleanup(r.Cancel)
	if got := reaches(t, r); got != "A|" {
		t.Errorf("Route() with New's origins reaches %q, want A", got)
	}
	if got := reaches(t, r, WithOrigins(b)); got != "B|" {
		t.Errorf("Route(WithOrigins(b)) reaches %q, want B", got)
	}
	if r.Origins() != a || r.WebSockets() != 0 {
		t.Error("a route's options changed the router's own, want them applied to that route alone")
	}
	if got := reaches(t, r); got != "A|" {
		t.Errorf("the route after one with its own origins reaches %q, want New's (A)", got)
	}
}

// TestControlPath pins the prefix tunneld keeps for itself: under it the
// router's own mux answers, and what it has no pattern for is a 404 rather
// than an origin's page; everywhere else — paths a mux would clean included —
// the request reaches an origin exactly as it was sent. A lone origin has the
// ControlPath too.
func TestControlPath(t *testing.T) {
	// pathOf answers with its name and the path it was sent, unclean or not.
	pathOf := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s|%s", name, r.URL.EscapedPath())
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	r := New()
	u, err := r.Route(t.Context(), WithOrigins(listOf(t, pathOf("A"), pathOf("B"))))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	t.Cleanup(r.Cancel)
	base := strings.TrimSuffix(u.String(), "/")
	// A redirect answered is the result, not something to follow.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	for name, tc := range map[string]struct {
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		"ping":                       {"GET", "/_tunneld/ping", 200, "pong"},
		"ping answers HEAD as GET":   {"HEAD", "/_tunneld/ping", 200, ""},
		"ping takes no other method": {"POST", "/_tunneld/ping", 405, ""},
		"an unregistered one is not the origin's": {"GET", "/_tunneld/nope", 401, ""},
		"a lookalike prefix is the origin's":      {"GET", "/_tunneldx", 200, "A|/_tunneldx"},
		"a doubled slash reaches the origin":      {"GET", "/a//b", 200, "A|/a//b"},
		"a dot segment reaches the origin":        {"GET", "/a/../b", 200, "A|/a/../b"},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, base+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, body := get(t, noFollow, req)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("%s %s = %d %q, want %d", tc.method, tc.path, resp.StatusCode, body, tc.wantStatus)
			}
			if tc.wantBody != "" && body != tc.wantBody {
				t.Errorf("%s %s = %q, want %q", tc.method, tc.path, body, tc.wantBody)
			}
			if tc.wantStatus >= 400 && strings.Contains(body, "|") {
				t.Errorf("%s %s reached an origin (%q), want the prefix kept from them", tc.method, tc.path, body)
			}
			if tc.path == "/_tunneld/ping" && tc.wantStatus == 200 {
				if got := resp.Header.Get("Cache-Control"); got != "no-store" {
					t.Errorf("ping Cache-Control = %q, want no-store: every ping is asked of this process", got)
				}
			}
		})
	}
	u, err = r.Route(t.Context(), WithOrigins(listOf(t, pathOf("solo"))))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, strings.TrimSuffix(u.String(), "/")+"/_tunneld/ping", nil)
	if _, body := get(t, http.DefaultClient, req); body != "pong" {
		t.Errorf("a lone origin's ping = %q, want pong", body)
	}
}

// cacheOf stands in for a run's cache: String is the file it saved, Secret
// the secret it saved with.
type cacheOf struct {
	file   string
	secret []byte
	key    string
}

func (c cacheOf) String() string { return c.file }
func (c cacheOf) Secret() []byte { return c.secret }
func (c cacheOf) Key() string    { return c.key }

// runKey is the key cacheOf names its run by in these tests.
const runKey = "0123456789abcdef"

// tokenOf is the Authorization header that secret authorizes.
func tokenOf(secret []byte) string {
	return "token " + base64.StdEncoding.EncodeToString(secret)
}

// controlOf routes r over one origin and returns the ControlPath URL of
// path under it.
func controlOf(t *testing.T, r *RouterImpl, path string, opts ...Option) string {
	t.Helper()
	u, err := r.Route(t.Context(), append([]Option{WithOrigins(listOf(t, echo(t, "A")))}, opts...)...)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	t.Cleanup(r.Cancel)
	return strings.TrimSuffix(u.String(), "/") + ControlPath + path
}

// ask sends method to url with auth as its Authorization header, if any.
func ask(t *testing.T, method, url, auth string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return get(t, http.DefaultClient, req)
}

// TestAuthorize pins what guards the ControlPath: everything under it but
// ping needs "Authorization: token <base64 secret>", the secret the cache
// holds; anything else is a bare 401 before the mux is asked, so an
// unregistered path is no different from a registered one. With no secret,
// nothing but ping answers — not even to a token of nothing.
func TestAuthorize(t *testing.T) {
	secret := []byte("s3cr3t")
	env := controlOf(t, New(WithCache(cacheOf{"LIBTUNNEL_SPEC='x'\n", secret, runKey})), ".env")
	nope := strings.TrimSuffix(env, ".env") + "nope"
	ping := strings.TrimSuffix(env, ".env") + "ping"
	for name, tc := range map[string]struct {
		url, auth  string
		wantStatus int
	}{
		"no header":                     {env, "", 401},
		"another scheme":                {env, "Bearer " + base64.StdEncoding.EncodeToString(secret), 401},
		"a wrong token":                 {env, tokenOf([]byte("guess")), 401},
		"the secret unencoded":          {env, "token " + string(secret), 401},
		"the right token":               {env, tokenOf(secret), 200},
		"an unregistered path, no auth": {nope, "", 401},
		"an unregistered path, auth":    {nope, tokenOf(secret), 404},
		"ping needs nothing":            {ping, "", 200},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := ask(t, "GET", tc.url, tc.auth)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("GET %s = %d %q, want %d", tc.url, resp.StatusCode, body, tc.wantStatus)
			}
			if tc.wantStatus == 401 && body != "" {
				t.Errorf("a refusal said %q, want a bare 401", body)
			}
		})
	}

	// Over the wire a header's trailing space is trimmed, so "token " never
	// arrives as itself; asked of the guard directly it does, and a secret of
	// nothing still authorizes nothing.
	t.Run("a token of nothing, asked directly", func(t *testing.T) {
		guard := New(WithCache(cacheOf{"x\n", nil, runKey})).authorize(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("the guard let a token of nothing through")
		}))
		req := httptest.NewRequest(http.MethodGet, ControlPath+".env", nil)
		req.Header.Set("Authorization", "token ")
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("a token of nothing = %d, want 401", rec.Code)
		}
	})

	t.Run("no secret, nothing but ping", func(t *testing.T) {
		for _, c := range []Option{WithCache(cacheOf{"LIBTUNNEL_SPEC='x'\n", nil, runKey}), WithLog(nil)} {
			url := controlOf(t, New(c), ".env")
			for _, auth := range []string{"", "token ", tokenOf(nil)} {
				if resp, _ := ask(t, "GET", url, auth); resp.StatusCode != 401 {
					t.Errorf("GET .env with no secret and %q = %d, want 401", auth, resp.StatusCode)
				}
			}
			if resp, _ := ask(t, "GET", strings.TrimSuffix(url, ".env")+"ping", ""); resp.StatusCode != 200 {
				t.Errorf("ping with no secret = %d, want 200", resp.StatusCode)
			}
		}
	})
}

// TestCacheKeyHeader pins X-Cache-Key: every answer under the ControlPath
// names the run by its cache key — ping, a served endpoint, the mux's own 404,
// and a refusal too — and only a cache that does not know its run yet names
// nothing.
func TestCacheKeyHeader(t *testing.T) {
	secret := []byte("s3cr3t")
	base := strings.TrimSuffix(controlOf(t, New(WithCache(cacheOf{"x\n", secret, runKey})), ""), "/")
	for name, tc := range map[string]struct {
		path, auth string
		want       string
	}{
		"ping":                         {"/ping", "", runKey},
		"a served endpoint":            {"/.env", tokenOf(secret), runKey},
		"the mux's 404":                {"/nope", tokenOf(secret), runKey},
		"a refusal":                    {"/.env", "", runKey},
		"a refusal of an unknown path": {"/nope", "", runKey},
	} {
		t.Run(name, func(t *testing.T) {
			resp, _ := ask(t, "GET", base+tc.path, tc.auth)
			if got := resp.Header.Get(CacheKeyHeader); got != tc.want {
				t.Errorf("GET %s (%d): %s = %q, want %q", tc.path, resp.StatusCode, CacheKeyHeader, got, tc.want)
			}
		})
	}
	t.Run("a cache that does not know its run", func(t *testing.T) {
		url := strings.TrimSuffix(controlOf(t, New(WithCache(cacheOf{"x\n", secret, ""})), ""), "/") + "/ping"
		if resp, _ := ask(t, "GET", url, ""); resp.Header.Values(CacheKeyHeader) != nil {
			t.Errorf("ping named a run the cache does not know: %q", resp.Header.Values(CacheKeyHeader))
		}
	})
}

// TestEnv pins ControlPath+".env", asked with the right token: absent until
// WithCache puts it on the mux, then a remote copy of the file the run's
// cache last saved, byte for byte and asked for fresh on every request; a
// bare 404 before anything is saved, and never stored on the way. WithCache
// applied twice — at New and again for a route — replaces the cache rather
// than registering the pattern twice, which a ServeMux would panic on.
func TestEnv(t *testing.T) {
	secret := []byte("s3cr3t")
	auth := tokenOf(secret)

	t.Run("absent without WithCache", func(t *testing.T) {
		// No cache is no secret, so the refusal comes first.
		if resp, body := ask(t, "GET", controlOf(t, New(), ".env"), auth); resp.StatusCode != 401 || strings.Contains(body, "|") {
			t.Errorf("GET .env = %d %q, want a 401 from the router", resp.StatusCode, body)
		}
	})

	t.Run("the saved file, as it is", func(t *testing.T) {
		const saved = "LIBTUNNEL_SPEC='{\"v\":1}'\nTUNNELD_LOG='debug'\n"
		c := cacheOf{saved, secret, runKey}
		resp, body := ask(t, "GET", controlOf(t, New(WithCache(c)), ".env"), auth)
		if resp.StatusCode != 200 || body != saved {
			t.Errorf("GET .env = %d %q, want the saved file", resp.StatusCode, body)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		if resp, _ := ask(t, "POST", controlOf(t, New(WithCache(c)), ".env"), auth); resp.StatusCode != 405 {
			t.Errorf("POST .env = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("a real cache serves the file it wrote", func(t *testing.T) {
		dir := t.TempDir()
		shown := listOf(t, echo(t, "shown"))
		c := cache.New(cache.WithDir(dir))
		url := controlOf(t, New(WithCache(c)), ".env")
		if resp, _ := ask(t, "GET", url, auth); resp.StatusCode != 401 {
			t.Errorf("GET .env before any save = %d, want 401: no secret yet", resp.StatusCode)
		}
		for _, spec := range []string{"saved-spec", "resaved-spec"} {
			c.Save(cache.WithOrigins(shown), cache.WithSpec(spec), cache.WithSecret(secret),
				cache.WithTracking(map[string]string{"TUNNELD_LOG": "debug"}))
			onDisk, err := os.ReadFile(filepath.Join(dir, shown.Key()+".env"))
			if err != nil {
				t.Fatal(err)
			}
			if _, body := ask(t, "GET", url, auth); body != string(onDisk) {
				t.Errorf("GET .env = %q, want the file on disk:\n%s", body, onDisk)
			}
		}
	})

	t.Run("nothing saved is a bare 404", func(t *testing.T) {
		if resp, body := ask(t, "GET", controlOf(t, New(WithCache(cacheOf{"", secret, runKey})), ".env"), auth); resp.StatusCode != 404 || body != "" {
			t.Errorf("GET .env with nothing saved = %d %q, want a bare 404", resp.StatusCode, body)
		}
	})

	t.Run("applied again, it replaces rather than registers twice", func(t *testing.T) {
		r := New(WithCache(cacheOf{"first\n", secret, runKey}))
		if _, body := ask(t, "GET", controlOf(t, r, ".env", WithCache(cacheOf{"second\n", secret, runKey})), auth); body != "second\n" {
			t.Errorf("GET .env = %q, want the later cache's file", body)
		}
	})
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
	base := route(t, listOf(t, echo(t, "solo")), -1, front, discard)
	for path, want := range map[string]string{"/front": "front", "/?x": "solo|x"} {
		req, _ := http.NewRequest("GET", base+path, nil)
		if _, body := get(t, http.DefaultClient, req); body != want {
			t.Errorf("GET %s = %q, want %q", path, body, want)
		}
	}
}

// TestRouteOutlivesItsContextUntilCancelled pins the router's lifetime: it
// keeps serving after the context it was routed under ends — the tunnel
// forwarding here drains past the run — and stops accepting once Cancel is
// called. Watched on the listener itself, polled to a deadline, since the
// close runs on a goroutine of its own and a busy runner may take a while to
// schedule it.
func TestRouteOutlivesItsContextUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r := New()
	u, err := r.Route(ctx, WithOrigins(listOf(t, echo(t, "A"), echo(t, "B"))))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	t.Cleanup(r.Cancel)

	cancel()
	time.Sleep(50 * time.Millisecond) // long enough for a close tied to ctx to have run
	resp, err := http.Get(u.String())
	if err != nil {
		t.Fatalf("GET after the context ended: %v, want the router still serving", err)
	}
	resp.Body.Close()

	r.Cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", u.Host, time.Second)
		if err != nil {
			return
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the router still accepts connections after Cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRouteWarnsOfAnOriginItCannotReach pins that an origin that is down is
// said at warn, naming the origin, rather than only at the edge as a bare 502:
// tunneld :3000 :4000 with :4000 not up yet, and /?1 used to be a blank page
// with no line anywhere. The request's own path stays out of the line.
func TestRouteWarnsOfAnOriginItCannotReach(t *testing.T) {
	down := echo(t, "B")
	list := listOf(t, echo(t, "A"), down)
	down.Close()

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	base := route(t, list, -1, nil, logger)

	req, _ := http.NewRequest("GET", base+"/secret-path?1", nil)
	resp, _ := get(t, http.DefaultClient, req)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	got := logs.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, list.At(1).Host) {
		t.Errorf("log = %q, want a warning naming %s", got, list.At(1).Host)
	}
	if strings.Contains(got, "secret-path") {
		t.Errorf("log = %q, want the request path left out", got)
	}
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
	base := route(t, listOf(t, host("A"), host("B")), -1, nil, discard)
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
	base := route(t, listOf(t, echo(t, "A"), tlsOrigin), -1, nil, discard)
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
	base := route(t, listOf(t, echo(t, "A"), echo(t, "B")), -1, nil, discard)

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
	base := route(t, listOf(t, echo(t, "A"), echo(t, "B")), -1, nil, discard)
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
		// The same two spelled percent-encoded decode to the same path, which
		// is what the guard reads, so they are refused the same way.
		"encodedSlashNoRedirect":     {method: "GET", path: "/%2Fevil.example/x", referer: "/?1", dest: "document", wantStatus: 200, wantBody: "B|"},
		"encodedBackslashNoRedirect": {method: "GET", path: "/%5Cevil.example/x", referer: "/?1", dest: "document", wantStatus: 200, wantBody: "B|"},
		// A control character browsers would strip arrives encoded, and the
		// Location it is echoed into keeps it encoded, so nothing collapses
		// into a scheme-relative URL.
		"encodedTabStaysEncoded": {method: "GET", path: "/%09/evil.example/x", referer: "/?1", dest: "document", wantStatus: 307, wantLocation: "/%09/evil.example/x?1"},
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
	base := route(t, listOf(t, echo(t, "A"), echo(t, "B")), 1, nil, discard)

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
	base := route(t, listOf(t, echo(t, "A"), echo(t, "B")), -1, nil, logger)

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
