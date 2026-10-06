package auth

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

const password = "Pizza für alle 🍕"
const value = `Basic realm="stored.example", pw="` + vector + `"`

// origin is what auth guards in these tests: it echoes what reached it.
func origin() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cookie", r.Header.Get("Cookie"))
		w.Header().Set("X-Authorization", r.Header.Get("Authorization"))
		io.WriteString(w, "origin")
	})
}

func protected(t *testing.T) *AuthImpl {
	t.Helper()
	a := New(WithSecret(func() []byte { return []byte("s3cr3t") }))
	if err := a.Set(value); err != nil {
		t.Fatal(err)
	}
	return a
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestHeader pins the gate as the auth says it: X-Tunneld-Authenticate, in
// public form, the stored realm included; empty when public. Never the value
// as stored.
func TestHeader(t *testing.T) {
	a := New()
	if name, v := a.Header(); name != v1.AuthenticateHeader || v != "" {
		t.Errorf("public Header() = %q, %q; want %s and empty", name, v, v1.AuthenticateHeader)
	}
	if err := a.Set(value); err != nil {
		t.Fatal(err)
	}
	if _, v := a.Header(); v != `Basic realm="stored.example", charset="UTF-8"` {
		t.Errorf("Header() = %q, want the public form", v)
	}
}

func TestSet(t *testing.T) {
	a := New()
	if err := a.Set("Basic nope"); err == nil {
		t.Error("an invalid value was accepted")
	}
	if _, v := a.Header(); a.Value() != "" || v != "" {
		t.Error("a refused Set changed the value")
	}
	if err := a.Set(value); err != nil {
		t.Fatal(err)
	}
	if a.Value() != value {
		t.Errorf("Value = %q", a.Value())
	}
	if _, got := a.Header(); got != `Basic realm="stored.example", charset="UTF-8"` {
		t.Errorf("Header = %q", got)
	}
	a.Set("")
	if _, v := a.Header(); v != "" {
		t.Error("a challenge after clearing")
	}
}

func TestHandler(t *testing.T) {
	a := protected(t)
	h := a.Handler(origin())
	cookie := mintCookie(cookieKey([]byte("s3cr3t")), "basic", value, a.now())

	t.Run("unset passes", func(t *testing.T) {
		rec := serve(New().Handler(origin()), httptest.NewRequest("GET", "/x", nil))
		if rec.Body.String() != "origin" || rec.Header().Get("Cloudflare-CDN-Cache-Control") != "" {
			t.Errorf("unset: %d %q", rec.Code, rec.Body)
		}
	})
	t.Run("cookie passes, stripped, edge told not to cache", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		req.AddCookie(&http.Cookie{Name: "app", Value: "1"})
		rec := serve(h, req)
		if rec.Body.String() != "origin" || strings.Contains(rec.Header().Get("X-Cookie"), CookieName) || !strings.Contains(rec.Header().Get("X-Cookie"), "app=1") {
			t.Errorf("cookie: %d %q, origin saw cookie %q", rec.Code, rec.Body, rec.Header().Get("X-Cookie"))
		}
		if rec.Header().Get("Cloudflare-CDN-Cache-Control") != "no-store" {
			t.Error("a passed response was not marked for the edge")
		}
	})
	t.Run("cookie from before a Set fails", func(t *testing.T) {
		b := protected(t)
		b.Set(`Basic pw="` + vector + `"`)
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Accept", "*/*")
		req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		if rec := serve(b.Handler(origin()), req); rec.Code != 401 {
			t.Errorf("stale cookie = %d, want 401", rec.Code)
		}
	})
	t.Run("a 401 with no realm stored names the host", func(t *testing.T) {
		b := protected(t)
		b.Set(`Basic pw="` + vector + `"`)
		req := httptest.NewRequest("GET", "/x", nil)
		req.Host = "0t8qsb6pq3.tunneled.pizza"
		rec := serve(b.Handler(origin()), req)
		if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="0t8qsb6pq3.tunneled.pizza", charset="UTF-8"` {
			t.Errorf("WWW-Authenticate = %q, want the request's host as the realm", got)
		}
	})
	t.Run("basic right passes, header stripped, cookie set", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.SetBasicAuth("anyone", password)
		rec := serve(h, req)
		if rec.Body.String() != "origin" || rec.Header().Get("X-Authorization") != "" {
			t.Errorf("basic: %d, origin saw %q", rec.Code, rec.Header().Get("X-Authorization"))
		}
		if !strings.Contains(rec.Header().Get("Set-Cookie"), CookieName+"=v1.") {
			t.Error("a Basic success set no cookie")
		}
	})
	t.Run("basic sent unprompted to a sibling path is still stripped", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/other/sibling.js", nil)
		req.SetBasicAuth("", password)
		if rec := serve(h, req); rec.Header().Get("X-Authorization") != "" {
			t.Error("a consumed Authorization reached the origin")
		}
	})
	for name, set := range map[string]func(*http.Request){
		"basic wrong":    func(r *http.Request) { r.SetBasicAuth("", "nope") },
		"basic empty":    func(r *http.Request) { r.SetBasicAuth("", "") },
		"nothing at all": func(*http.Request) {},
		"fetch for html": func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "cors"); r.Header.Set("Accept", "text/html") },
		"html post":      func(r *http.Request) { r.Method = "POST"; r.Header.Set("Accept", "text/html") },
		"head non-html":  func(r *http.Request) { r.Method = "HEAD" },
	} {
		t.Run(name+" is a bodyless 401", func(t *testing.T) {
			req := httptest.NewRequest("GET", "/x", nil)
			req.Host = "0t8qsb6pq3.tunneled.pizza"
			set(req)
			rec := serve(h, req)
			if rec.Code != 401 || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
				t.Fatalf("%d, %d bytes, Content-Type %q; want a bodyless 401", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
			}
			if got := rec.Header().Values("WWW-Authenticate"); len(got) != 1 || got[0] != `Basic realm="stored.example", charset="UTF-8"` {
				t.Errorf("WWW-Authenticate = %q, want the stored realm", got)
			}
			// Cache-Control is the router's to write, on everything it serves.
			if rec.Header().Get("Content-Length") != "0" {
				t.Errorf("Content-Length %q", rec.Header().Get("Content-Length"))
			}
		})
	}
	for name, set := range map[string]func(*http.Request){
		"navigate":           func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "navigate") },
		"no sec-fetch, html": func(r *http.Request) { r.Header.Set("Accept", "text/html,*/*") },
		"no sec-fetch, head": func(r *http.Request) { r.Method = "HEAD"; r.Header.Set("Accept", "text/html") },
	} {
		t.Run(name+" is a 303 to login", func(t *testing.T) {
			req := httptest.NewRequest("GET", "/app/page?x=1&y=2", nil)
			set(req)
			rec := serve(h, req)
			want := "/_tunneld/login?next=" + url.QueryEscape("/app/page?x=1&y=2")
			if rec.Code != 303 || rec.Header().Get("Location") != want {
				t.Errorf("%d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), want)
			}
		})
	}
}

func TestLogin(t *testing.T) {
	a := protected(t)
	mux := http.NewServeMux()
	for pattern, h := range a.Handlers("/_tunneld/") {
		mux.HandleFunc(pattern, h)
	}
	post := func(pw, next string) *httptest.ResponseRecorder {
		form := url.Values{"password": {pw}, "next": {next}}
		req := httptest.NewRequest("POST", "/_tunneld/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "h.tunneled.pizza"
		return serve(mux, req)
	}

	get := serve(mux, httptest.NewRequest("GET", "/_tunneld/login?next=/app", nil))
	for _, want := range []string{`name="password"`, `autocomplete="current-password"`, `name="username"`, `autocomplete="username"`, `value="/app"`} {
		if get.Code != 200 || !strings.Contains(get.Body.String(), want) {
			t.Errorf("GET login: %d, missing %s", get.Code, want)
		}
	}
	if rec := post(password, "/app"); rec.Code != 303 || rec.Header().Get("Location") != "/app" ||
		!strings.Contains(rec.Header().Get("Set-Cookie"), "HttpOnly") || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=2592000") {
		t.Errorf("right password: %d %q %q", rec.Code, rec.Header().Get("Location"), rec.Header().Get("Set-Cookie"))
	}
	if rec := post("wrong", "/app"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "isn") || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("wrong password: %d", rec.Code)
	}
	for _, bad := range []string{"//evil.example", `/\evil.example`, "https://evil.example", "/_tunneld/logout", ""} {
		if rec := post(password, bad); rec.Header().Get("Location") != "/" {
			t.Errorf("next %q went to %q, want /", bad, rec.Header().Get("Location"))
		}
	}
	nosecret := New()
	nosecret.Set(value)
	m2 := http.NewServeMux()
	for p, h := range nosecret.Handlers("/_tunneld/") {
		m2.HandleFunc(p, h)
	}
	form := url.Values{"password": {password}}
	req := httptest.NewRequest("POST", "/_tunneld/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := serve(m2, req); rec.Code != 503 || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("no secret: %d, Retry-After %q; want 503/2", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := serve(mux, httptest.NewRequest("PUT", "/_tunneld/login", nil)); rec.Code != 405 {
		t.Errorf("PUT = %d, want 405", rec.Code)
	}
	for _, method := range []string{"GET", "POST"} {
		rec := serve(mux, httptest.NewRequest(method, "/_tunneld/logout?next=/app", nil))
		if rec.Code != 303 || rec.Header().Get("Location") != "/app" || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
			t.Errorf("%s logout: %d %q %q", method, rec.Code, rec.Header().Get("Location"), rec.Header().Get("Set-Cookie"))
		}
	}
	public := New()
	mp := http.NewServeMux()
	for p, h := range public.Handlers("/_tunneld/") {
		mp.HandleFunc(p, h)
	}
	if rec := serve(mp, httptest.NewRequest("GET", "/_tunneld/login?next=/app", nil)); rec.Code != 303 || rec.Header().Get("Location") != "/app" {
		t.Errorf("login with nothing set: %d %q; want 303 /app", rec.Code, rec.Header().Get("Location"))
	}
	form = url.Values{"password": {"x"}, "next": {"/app"}}
	req = httptest.NewRequest("POST", "/_tunneld/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := serve(mp, req); rec.Code != 303 {
		t.Errorf("POST login with nothing set: %d; want 303", rec.Code)
	}
	// The page holds a form another site's frame could dress up as something
	// else; the panel's own tiles are same-origin frames, and may show it.
	csp := get.Header().Get("Content-Security-Policy")
	for _, want := range []string{"frame-ancestors 'self'", "default-src 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("login page CSP %q, missing %s", csp, want)
		}
	}
}

// TestBasicFailuresAreLogged pins that a wrong Basic password is logged as
// a failed form login is, and a right one (an API client sending it on
// every request) is not.
func TestBasicFailuresAreLogged(t *testing.T) {
	var buf bytes.Buffer
	a := protected(t)
	WithLog(slog.New(slog.NewTextHandler(&buf, nil)))(a)
	h := a.Handler(origin())
	ask := func(pw string) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.SetBasicAuth("", pw)
		serve(h, req)
	}
	ask(password)
	if strings.Contains(buf.String(), "a login") {
		t.Errorf("a right Basic password was logged:\n%s", buf.String())
	}
	ask("wrong")
	if !strings.Contains(buf.String(), "a login") || !strings.Contains(buf.String(), "via=basic") || !strings.Contains(buf.String(), "ok=false") {
		t.Errorf("a wrong Basic password was not logged as a failed login:\n%s", buf.String())
	}
	// Once the address is held, each refusal is a 429 before any check: one
	// line per request would let anyone grow the log at request rate.
	for range 10 {
		ask("wrong")
	}
	if n := strings.Count(buf.String(), "a login"); n > 5 {
		t.Errorf("%d login lines for 11 wrong passwords, want only the 5 checked", n)
	}
}

// TestEveryAuthCookieIsTried pins a sibling tunnel's cookie: any tunnel on
// the shared domain can set one named like ours, which the browser may send
// first. The visitor's own still lets them in.
func TestEveryAuthCookieIsTried(t *testing.T) {
	a := protected(t)
	good := mintCookie(cookieKey([]byte("s3cr3t")), "basic", value, a.now())
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", CookieName+"=v1.forged.mac; "+CookieName+"="+good)
	if rec := serve(a.Handler(origin()), req); rec.Code != 200 || strings.Contains(rec.Header().Get("X-Cookie"), CookieName) {
		t.Errorf("own cookie behind a sibling's: %d, origin saw %q; want 200, neither", rec.Code, rec.Header().Get("X-Cookie"))
	}
	// Only the first few are tried: each costs an HMAC, and a request may
	// carry thousands.
	req.Header.Set("Cookie", strings.Repeat(CookieName+"=v1.forged.mac; ", maxAuthCookies)+CookieName+"="+good)
	if rec := serve(a.Handler(origin()), req); rec.Code == 200 {
		t.Errorf("a cookie behind %d forged ones was tried", maxAuthCookies)
	}
}

// newVector is a second password's hash, for rotating to.
const newVector = "$pbkdf2-sha256$i=100000$dHVubmVsLnBpenphL3YwMg$SwD1pS+RqRBIgMZ4nJP7ZDAi5GjNdYnIu59/gU6LefI"

// TestRotationRevokesAnInFlightPassword pins that rotating a password revokes
// the old one even when a request verified it while Set ran: that check's
// answer belongs to the old value, not to whatever the value is when it lands
// in the verified cache.
func TestRotationRevokesAnInFlightPassword(t *testing.T) {
	a := protected(t)
	key := cookieKey([]byte("s3cr3t"))
	old := a.state.Load()
	if err := a.Set(`Basic pw="` + newVector + `"`); err != nil {
		t.Fatal(err)
	}
	// The request that loaded the old state finishes now, after Set emptied
	// the cache, and records what it verified.
	if ok, _ := a.guard.check(context.Background(), "", key, old.value, password, old.verifyAny); !ok {
		t.Fatal("the in-flight check did not verify against the old value")
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetBasicAuth("", password)
	if rec := serve(a.Handler(origin()), req); rec.Code != 401 || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("the old password after rotation = %d, cookie %q; want 401 and none", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	req = httptest.NewRequest("GET", "/x", nil)
	req.SetBasicAuth("", "new-password-123")
	if rec := serve(a.Handler(origin()), req); rec.Code != 200 {
		t.Errorf("the new password = %d, want 200", rec.Code)
	}
}

// TestSafeNext pins where a login or logout may send a browser: a path on
// this host, outside the control path, in any spelling. A control character
// (a browser strips a tab, turning "/\t/evil" into "//evil"), a dot-segment or
// an escaped character that reaches the control path once cleaned or
// decoded, and anything with a scheme or a host, all go to "/".
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/app":                  "/app",
		"/app/":                 "/app/",
		"/app?x=1&y=2":          "/app?x=1&y=2",
		"/":                     "/",
		"":                      "/",
		"app":                   "/",
		"//evil.example":        "/",
		`/\evil.example`:        "/",
		"/\t/evil.example":      "/",
		"/\n/evil.example":      "/",
		"https://evil.example":  "/",
		"/_tunneld/logout":      "/",
		"/./_tunneld/logout":    "/",
		"/x/../_tunneld/logout": "/",
		"/%5Ftunneld/logout":    "/",
		"/_tunneld":             "/",
		"/a/b/../c":             "/a/c",
		"/%2F%2Fevil.example":   "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCookieHolderBasic pins what reaches the origin when a browser carries
// both the cookie and a Basic header (Chrome resends cached Basic): the
// tunnel's password is stripped, the origin's own credential is not.
func TestCookieHolderBasic(t *testing.T) {
	a := protected(t)
	h := a.Handler(origin())
	cookie := mintCookie(cookieKey([]byte("s3cr3t")), "basic", value, a.now())
	ask := func(user, pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/x", nil)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		req.SetBasicAuth(user, pw)
		return serve(h, req)
	}
	if rec := ask("", password); rec.Code != 200 || rec.Header().Get("X-Authorization") != "" {
		t.Errorf("the tunnel's password reached the origin: %d %q", rec.Code, rec.Header().Get("X-Authorization"))
	}
	for range 6 {
		rec := ask("admin", "s3cret")
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("X-Authorization"), "Basic ") {
			t.Fatalf("the origin's own credential: %d %q, want 200 with it intact", rec.Code, rec.Header().Get("X-Authorization"))
		}
	}
}

// TestEdgeHeaderWinsOverTheOrigins pins that a protected tunnel's answer
// tells the edge exactly one thing, no-store, even when the origin set its
// own Cloudflare-CDN-Cache-Control: two values could let the edge keep a
// copy for visitors who never logged in. Flushing (streaming, upgrades)
// still reaches the real writer.
func TestEdgeHeaderWinsOverTheOrigins(t *testing.T) {
	a := protected(t)
	cookie := mintCookie(cookieKey([]byte("s3cr3t")), "basic", value, a.now())
	var flushErr error
	h := a.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cloudflare-CDN-Cache-Control", "max-age=600")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.WriteHeader(200)
		flushErr = http.NewResponseController(w).Flush()
	}))
	req := httptest.NewRequest("GET", "/asset.js", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	rec := serve(h, req)
	if got := rec.Header().Values("Cloudflare-CDN-Cache-Control"); len(got) != 1 || got[0] != "no-store" {
		t.Errorf("Cloudflare-CDN-Cache-Control = %q, want exactly no-store", got)
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Errorf("the origin's own Cache-Control was touched: %q", rec.Header().Get("Cache-Control"))
	}
	if flushErr != nil || !rec.Flushed {
		t.Errorf("flush through auth = %v, flushed %v", flushErr, rec.Flushed)
	}
}
