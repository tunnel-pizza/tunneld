package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth/oidc"
)

const password = "Pizza für alle 🍕"
const value = `Basic realm="stored.example", pw="` + vector + `"`

// origin is what auth guards in these tests: it echoes what reached it.
func origin() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cookie", r.Header.Get("Cookie"))
		w.Header().Set("X-Authorization", r.Header.Get("Authorization"))
		w.Header().Set("X-Sub", r.Header.Get(SubHeader))
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

const (
	ssoHost    = "h.tunneled.pizza"
	ssoValue   = `Bearer realm="` + ssoHost + `", sub="github:1 github:2"`
	testClient = "https://" + ssoHost + "/_tunneld/client.json"
	fakeIssuer = "https://issuer.example"
)

// grantFor is who a fake token is, and what it is for.
type grantFor struct {
	sub string
	aud []string
}

// oidcOf stands in for the provider: a token in tokens is valid for its aud
// as its sub; the sign-in's code is "c0de" with verifier, and its ID token
// carries nonce and names sub and username. down makes every ask
// unavailable; exchangeDown the token endpoint alone. Asked from the test's
// own goroutine (httptest.ResponseRecorder), so plain fields do.
type oidcOf struct {
	oidc.Oidc
	tokens             map[string]grantFor
	down, exchangeDown bool
	verifier, nonce    string
	sub, username      string
	assertion          string
}

func fakeOidc() *oidcOf {
	site, mcp := []string{"https://" + ssoHost}, []string{"https://" + ssoHost + "/_tunneld/mcp"}
	return &oidcOf{
		tokens: map[string]grantFor{
			"listed":   {"github:1", site},
			"also":     {"github:2", append(site, mcp...)},
			"unlisted": {"github:9", site},
			"mcp-only": {"github:1", mcp},
		},
		sub: "github:1", username: "alice",
	}
}

func (o *oidcOf) Endpoints(context.Context) (oidc.Discovery, error) {
	if o.down {
		return oidc.Discovery{}, oidc.ErrUnavailable
	}
	return oidc.Discovery{Issuer: fakeIssuer, AuthorizationEndpoint: fakeIssuer + "/oauth/authorize",
		TokenEndpoint: fakeIssuer + "/oauth/token", JWKSURI: fakeIssuer + "/jwks.json"}, nil
}

func (o *oidcOf) VerifyAccess(_ context.Context, token, resource string) (string, error) {
	if o.down {
		return "", oidc.ErrUnavailable
	}
	g, ok := o.tokens[token]
	if !ok || !slices.Contains(g.aud, resource) {
		return "", oidc.ErrInvalidToken
	}
	return g.sub, nil
}

func (o *oidcOf) Exchange(_ context.Context, code, verifier, redirectURI, clientID, assertion string) (string, error) {
	o.assertion = assertion
	if o.down || o.exchangeDown {
		return "", oidc.ErrUnavailable
	}
	if code != "c0de" || verifier != o.verifier || redirectURI != "https://"+ssoHost+"/_tunneld/callback" || clientID != testClient {
		return "", oidc.ErrRefused
	}
	return "id-token", nil
}

func (o *oidcOf) VerifyID(_ context.Context, token, clientID, nonce string) (oidc.IDClaims, error) {
	if token != "id-token" || clientID != testClient || nonce == "" || nonce != o.nonce {
		return oidc.IDClaims{}, oidc.ErrInvalidToken
	}
	return oidc.IDClaims{Subject: o.sub, PreferredUsername: o.username}, nil
}

// owned is a with owner as the tunnel's owner, as its mint named it.
func owned(a *AuthImpl, owner string) *AuthImpl {
	WithOwner(func() string { return owner })(a)
	return a
}

// pwAuth is an auth set to the password value that asks o.
func pwAuth(t *testing.T, o *oidcOf) *AuthImpl {
	t.Helper()
	a := New(WithSecret(func() []byte { return []byte("s3cr3t") }), WithOidc(o))
	if err := a.Set(value); err != nil {
		t.Fatal(err)
	}
	return a
}

// ssoAuth is an auth set to ssoValue that asks o.
func ssoAuth(t *testing.T, o *oidcOf) *AuthImpl {
	t.Helper()
	a := New(WithSecret(func() []byte { return []byte("s3cr3t") }), WithOidc(o))
	if err := a.Set(ssoValue); err != nil {
		t.Fatal(err)
	}
	return a
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
	if err := a.Set(ssoValue); err != nil {
		t.Fatal(err)
	}
	want := `Bearer realm="h.tunneled.pizza", resource_metadata="https://h.tunneled.pizza/.well-known/oauth-protected-resource", scope="openid profile offline_access"`
	if _, v := a.Header(); v != want {
		t.Errorf("SSO Header() = %q, want %q", v, want)
	}
}

// TestUnauthorized pins the 401 for whatever the password does not open:
// RFC 9728's Bearer challenge naming this tunnel's protected-resource
// metadata, the MCP server's own for /_tunneld/mcp, password or not;
// error="invalid_token" when a Bearer credential was sent (RFC 6750 §3.1);
// no body.
func TestUnauthorized(t *testing.T) {
	root := `Bearer resource_metadata="https://h.example/.well-known/oauth-protected-resource"`
	mcp := `Bearer resource_metadata="https://h.example/.well-known/oauth-protected-resource/_tunneld/mcp"`
	for name, tc := range map[string]struct {
		a          *AuthImpl
		path, auth string
		want       string
	}{
		"public":                {New(), "/_tunneld/.env", "", root},
		"protected":             {protected(t), "/_tunneld/.env", "", root},
		"the mcp server, sso":   {ssoAuth(t, fakeOidc()), "/_tunneld/mcp", "", mcp},
		"the mcp server, else":  {protected(t), "/_tunneld/mcp", "", root},
		"a bearer that failed":  {New(), "/_tunneld/.env", "Bearer stale-grant", root + `, error="invalid_token"`},
		"a bearer at mcp, sso":  {ssoAuth(t, fakeOidc()), "/_tunneld/mcp", "bearer x", mcp + `, error="invalid_token"`},
		"a bearer at mcp":       {New(), "/_tunneld/mcp", "bearer x", root + `, error="invalid_token"`},
		"the mcp server, owned": {owned(New(), "github:9"), "/_tunneld/mcp", "", mcp},
		"a token, not bearer":   {New(), "/_tunneld/.env", "token x", root},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			req.Host = "h.example"
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			tc.a.Unauthorized(rec, req)
			if rec.Code != http.StatusUnauthorized || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "0" {
				t.Errorf("%d, %d bytes, Content-Length %q; want a bodyless 401", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
			}
			if got := rec.Header().Values("WWW-Authenticate"); len(got) != 1 || got[0] != tc.want {
				t.Errorf("WWW-Authenticate = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProtectedResource pins the metadata the 401 points at (RFC 9728): this
// tunnel as the resource, the provider as its authorization server.
func TestProtectedResource(t *testing.T) {
	for name, tc := range map[string]struct {
		opts   []Option
		server string
	}{
		"by default, tunnel.pizza": {nil, "https://tunnel.pizza"},
		"the provider it is given": {[]Option{WithAuthorizationServer(func() string { return "https://p.example" })}, "https://p.example"},
		"nil keeps tunnel.pizza":   {[]Option{WithAuthorizationServer(nil)}, "https://tunnel.pizza"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("GET", MetadataPath, nil)
			req.Host = "h.example"
			rec := httptest.NewRecorder()
			New(tc.opts...).ResourceMetadata(rec, req)
			if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("%d, Content-Type %q; want 200 application/json", rec.Code, rec.Header().Get("Content-Type"))
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"resource":                 "https://h.example",
				"authorization_servers":    []any{tc.server},
				"scopes_supported":         []any{"openid", "profile", "offline_access"},
				"bearer_methods_supported": []any{"header"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("metadata = %v, want %v", got, want)
			}
		})
	}
	// The MCP server's metadata only where a token can open it: on a tunnel
	// set to Single Sign-On. Anywhere else a client would sign in for a
	// token the tunnel then refuses.
	req := httptest.NewRequest("GET", MCPMetadataPath, nil)
	req.Host = "h.example"
	rec := httptest.NewRecorder()
	ssoAuth(t, fakeOidc()).ResourceMetadata(rec, req)
	var mcp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &mcp); err != nil || mcp["resource"] != "https://h.example/_tunneld/mcp" {
		t.Errorf("MCP metadata = %v, %v; want the MCP server as the resource", mcp, err)
	}
	// Or wherever the mint named an owner, whose token opens it on any tunnel.
	rec = httptest.NewRecorder()
	owned(protected(t), "github:9").ResourceMetadata(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"resource":"https://h.example/_tunneld/mcp"`) {
		t.Errorf("an owned tunnel's MCP metadata = %d %q, want the MCP server's", rec.Code, rec.Body)
	}
	for name, a := range map[string]*AuthImpl{"public": New(), "password": protected(t), "owned by nobody": owned(New(), "")} {
		rec := httptest.NewRecorder()
		a.ResourceMetadata(rec, req)
		if rec.Code != 404 || rec.Body.Len() != 0 {
			t.Errorf("%s: MCP metadata = %d %q, want a bare 404", name, rec.Code, rec.Body)
		}
	}
	if _, ok := New().Handlers("/_tunneld/")["/_tunneld/.well-known/oauth-protected-resource"]; ok {
		t.Error("the metadata is under the control path too; want it at the root alone")
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
	cookie := mintCookie(cookieKey([]byte("s3cr3t")), "basic", "", value, a.now())

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
	t.Run("the metadata needs no password, by GET or HEAD", func(t *testing.T) {
		for _, method := range []string{"GET", "HEAD"} {
			rec := serve(h, httptest.NewRequest(method, MetadataPath, nil))
			if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || strings.Contains(rec.Body.String(), "origin") {
				t.Errorf("%s metadata = %d %q; want tunneld's, in front of the origin", method, rec.Code, rec.Body)
			}
		}
		if rec := serve(h, httptest.NewRequest("POST", MetadataPath, nil)); rec.Code != 401 {
			t.Errorf("POST metadata = %d, want the gate's 401", rec.Code)
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
	// Signed out, and staying out: the login page itself, never a redirect
	// back through the gate, which on a Single Sign-On tunnel would sign
	// the browser straight back in.
	for _, method := range []string{"GET", "POST"} {
		rec := serve(mux, httptest.NewRequest(method, "/_tunneld/logout?next=/app", nil))
		body := rec.Body.String()
		if rec.Code != 200 || rec.Header().Get("Location") != "" || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") ||
			!strings.Contains(body, "Signed out of example.com.") || !strings.Contains(body, `name="password"`) ||
			!strings.Contains(body, `value="/app"`) {
			t.Errorf("%s logout: %d %q %q\n%s", method, rec.Code, rec.Header().Get("Location"), rec.Header().Get("Set-Cookie"), body)
		}
	}
	public := New()
	mp := http.NewServeMux()
	for p, h := range public.Handlers("/_tunneld/") {
		mp.HandleFunc(p, h)
	}
	// A public tunnel has nothing to stay out of: back to where it was.
	if rec := serve(mp, httptest.NewRequest("GET", "/_tunneld/logout?next=/app", nil)); rec.Code != 303 || rec.Header().Get("Location") != "/app" {
		t.Errorf("public logout: %d to %q, want 303 /app", rec.Code, rec.Header().Get("Location"))
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

// TestEveryAuthCookieIsTried pins a cookie named like ours sent first, as
// one the origin set on a longer path is. The visitor's own still lets them
// in.
func TestEveryAuthCookieIsTried(t *testing.T) {
	a := protected(t)
	good := mintCookie(cookieKey([]byte("s3cr3t")), "basic", "", value, a.now())
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", CookieName+"=v1.forged.mac; "+CookieName+"="+good)
	if rec := serve(a.Handler(origin()), req); rec.Code != 200 || strings.Contains(rec.Header().Get("X-Cookie"), CookieName) {
		t.Errorf("own cookie behind another: %d, origin saw %q; want 200, neither", rec.Code, rec.Header().Get("X-Cookie"))
	}
	// Only the first few are tried: each costs an HMAC, and a request may
	// carry thousands.
	req.Header.Set("Cookie", strings.Repeat(CookieName+"=v1.forged.mac; ", maxAuthCookies)+CookieName+"="+good)
	if rec := serve(a.Handler(origin()), req); rec.Code == 200 {
		t.Errorf("a cookie behind %d forged ones was tried", maxAuthCookies)
	}
}

// TestTheAuthCookie pins the login cookie: __Host-, so it is host-only on
// Path=/ and no sibling tunnel on the shared domain can set or shadow one;
// logout clears it, and it never reaches the origin.
func TestTheAuthCookie(t *testing.T) {
	a := protected(t)
	cookies := func(rec *httptest.ResponseRecorder) map[string]*http.Cookie {
		m := map[string]*http.Cookie{}
		for _, c := range rec.Result().Cookies() {
			m[c.Name] = c
		}
		return m
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetBasicAuth("", password)
	got := cookies(serve(a.Handler(origin()), req))
	c := got[CookieName]
	if CookieName != "__Host-tunneld-auth" || c == nil || c.Path != "/" || c.Domain != "" || !c.Secure || !c.HttpOnly {
		t.Errorf("the cookie set: %q %+v; want __Host-tunneld-auth, host-only on /", CookieName, c)
	}
	rec := httptest.NewRecorder()
	a.logout(rec, httptest.NewRequest("POST", "/_tunneld/logout", nil))
	got = cookies(rec)
	if c := got[CookieName]; c == nil || c.MaxAge >= 0 {
		t.Errorf("logout left %s: %+v", CookieName, c)
	}
	req = httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: mintCookie(cookieKey([]byte("s3cr3t")), "basic", "", value, a.now())})
	if rec := serve(a.Handler(origin()), req); rec.Code != 200 || rec.Header().Get("X-Cookie") != "" {
		t.Errorf("%d, origin saw cookies %q; want 200, none", rec.Code, rec.Header().Get("X-Cookie"))
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
	cookie := mintCookie(cookieKey([]byte("s3cr3t")), "basic", "", value, a.now())
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
	cookie := mintCookie(cookieKey([]byte("s3cr3t")), "basic", "", value, a.now())
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

// TestTheDefaultOidcAsksTheServer pins the default oidc: built after the
// options and reading the authorization server when asked, so one set after
// New is the issuer asked; WithOidc(nil) keeps it.
func TestTheDefaultOidcAsksTheServer(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			asked.Add(1)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	a := New(WithSecret(func() []byte { return []byte("s3cr3t") }), WithOidc(nil))
	WithAuthorizationServer(func() string { return srv.URL })(a)
	if err := a.Set(ssoValue); err != nil {
		t.Fatal(err)
	}
	// Shaped like an RS256 access token, so the key is looked up and the
	// issuer asked.
	enc := base64.RawURLEncoding.EncodeToString
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+enc([]byte(`{"alg":"RS256","typ":"at+jwt","kid":"k"}`))+"."+enc([]byte(`{}`))+"."+enc([]byte("sig")))
	if _, ok, err := a.Bearer(req, "https://"+ssoHost); ok || !errors.Is(err, ErrUnavailable) || asked.Load() != 1 {
		t.Errorf("Bearer = %v, %v with the server asked %d times; want ErrUnavailable from the server set after New", ok, err, asked.Load())
	}
}

// TestBearer pins Bearer's answers: no token to ask about (no Bearer
// challenge, no Bearer credential) is ok false with no error; a token that
// was sent is ok, or says why not.
func TestBearer(t *testing.T) {
	site := "https://" + ssoHost
	ask := func(a *AuthImpl, auth, resource string) (string, bool, error) {
		req := httptest.NewRequest("GET", "/x", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return a.Bearer(req, resource)
	}
	down := fakeOidc()
	down.down = true
	for name, tc := range map[string]struct {
		a        *AuthImpl
		auth     string
		resource string
		sub      string
		ok       bool
		err      error
	}{
		"public":           {New(WithOidc(fakeOidc())), "Bearer listed", site, "", false, nil},
		"basic only":       {protected(t), "Bearer listed", site, "", false, nil},
		"no credential":    {ssoAuth(t, fakeOidc()), "", site, "", false, nil},
		"the secret":       {ssoAuth(t, fakeOidc()), "token czNjcjN0", site, "", false, nil},
		"listed":           {ssoAuth(t, fakeOidc()), "Bearer listed", site, "github:1", true, nil},
		"scheme lowercase": {ssoAuth(t, fakeOidc()), "bearer listed", site, "github:1", true, nil},
		"not listed":       {ssoAuth(t, fakeOidc()), "Bearer unlisted", site, "github:9", false, ErrNotListed},
		"another resource": {ssoAuth(t, fakeOidc()), "Bearer listed", site + "/_tunneld/mcp", "", false, ErrInvalidToken},
		"not a token":      {ssoAuth(t, fakeOidc()), "Bearer g00d", site, "", false, ErrInvalidToken},
		"the issuer down":  {ssoAuth(t, down), "Bearer listed", site, "", false, ErrUnavailable},
		// The owner the mint named counts as listed, on any tunnel.
		"the owner, public":   {owned(New(WithOidc(fakeOidc())), "github:9"), "Bearer unlisted", site, "github:9", true, nil},
		"the owner, password": {owned(pwAuth(t, fakeOidc()), "github:2"), "Bearer also", site + "/_tunneld/mcp", "github:2", true, nil},
		"the owner, sso":      {owned(ssoAuth(t, fakeOidc()), "github:9"), "Bearer unlisted", site, "github:9", true, nil},
		"not the owner":       {owned(New(WithOidc(fakeOidc())), "github:9"), "Bearer listed", site, "github:1", false, ErrNotListed},
		"owned by nobody":     {owned(New(WithOidc(fakeOidc())), ""), "Bearer listed", site, "", false, nil},
	} {
		t.Run(name, func(t *testing.T) {
			sub, ok, err := ask(tc.a, tc.auth, tc.resource)
			if sub != tc.sub || ok != tc.ok || !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Errorf("Bearer = %q, %v, %v; want %q, %v, %v", sub, ok, err, tc.sub, tc.ok, tc.err)
			}
		})
	}
}

// TestBearerGate pins the gate on an SSO tunnel: a listed token reaches the
// origin without Authorization and with X-Tunneld-Sub; anything else is
// refused, bodyless, by what went wrong; a visitor's own X-Tunneld-Sub never
// reaches the origin, public tunnel or not.
func TestBearerGate(t *testing.T) {
	a := ssoAuth(t, fakeOidc())
	h := a.Handler(origin())
	public := `Bearer realm="h.tunneled.pizza", resource_metadata="https://h.tunneled.pizza/.well-known/oauth-protected-resource", scope="openid profile offline_access"`
	req := func(auth string) *http.Request {
		r := httptest.NewRequest("GET", "/x", nil)
		r.Host = ssoHost
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return r
	}
	t.Run("listed passes, stripped, named", func(t *testing.T) {
		r := req("Bearer listed")
		r.Header.Set(SubHeader, "github:2")
		rec := serve(h, r)
		if rec.Code != 200 || rec.Header().Get("X-Authorization") != "" || rec.Header().Get("X-Sub") != "github:1" {
			t.Errorf("%d, origin saw Authorization %q and sub %q; want 200, none, github:1",
				rec.Code, rec.Header().Get("X-Authorization"), rec.Header().Get("X-Sub"))
		}
		if rec.Header().Get("Cloudflare-CDN-Cache-Control") != "no-store" {
			t.Error("a passed response was not marked for the edge")
		}
	})
	t.Run("the panel and the terminals take it too", func(t *testing.T) {
		for _, target := range []string{"/", "/?n=1", "/attach?n=1", "/attach/embedded?n=2"} {
			r := httptest.NewRequest("GET", target, nil)
			r.Host = ssoHost
			r.Header.Set("Authorization", "Bearer listed")
			if rec := serve(h, r); rec.Code != 200 || rec.Header().Get("X-Sub") != "github:1" {
				t.Errorf("GET %s = %d, sub %q; want through the gate as github:1", target, rec.Code, rec.Header().Get("X-Sub"))
			}
		}
	})
	t.Run("the sign-in's cookie never reaches the origin", func(t *testing.T) {
		r := req("Bearer listed")
		r.AddCookie(&http.Cookie{Name: flowCookie + "-abc", Value: "sealed"})
		r.AddCookie(&http.Cookie{Name: "app", Value: "1"})
		if rec := serve(h, r); rec.Code != 200 || rec.Header().Get("X-Cookie") != "app=1" {
			t.Errorf("%d, origin saw cookies %q; want 200 and app=1 alone", rec.Code, rec.Header().Get("X-Cookie"))
		}
	})
	t.Run("not listed is a bodyless 403", func(t *testing.T) {
		rec := serve(h, req("Bearer unlisted"))
		if rec.Code != 403 || rec.Body.Len() != 0 || rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%d %q, WWW-Authenticate %q; want a bare 403", rec.Code, rec.Body, rec.Header().Get("WWW-Authenticate"))
		}
	})
	t.Run("a bad token is invalid_token", func(t *testing.T) {
		for _, tok := range []string{"nope", "mcp-only"} {
			rec := serve(h, req("Bearer "+tok))
			if rec.Code != 401 || rec.Body.Len() != 0 || rec.Header().Get("WWW-Authenticate") != public+`, error="invalid_token"` {
				t.Errorf("%s: %d, WWW-Authenticate %q", tok, rec.Code, rec.Header().Get("WWW-Authenticate"))
			}
		}
	})
	t.Run("nothing sent is the plain challenge", func(t *testing.T) {
		rec := serve(h, req(""))
		if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != public {
			t.Errorf("%d, WWW-Authenticate %q, want %q", rec.Code, rec.Header().Get("WWW-Authenticate"), public)
		}
	})
	t.Run("a basic sent to a bearer-only tunnel is refused, unchecked", func(t *testing.T) {
		r := req("")
		r.SetBasicAuth("", password)
		if rec := serve(h, r); rec.Code != 401 {
			t.Errorf("%d, want 401", rec.Code)
		}
	})
	t.Run("a page load goes to login", func(t *testing.T) {
		r := req("")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		if rec := serve(h, r); rec.Code != 303 || !strings.HasPrefix(rec.Header().Get("Location"), "/_tunneld/login?next=") {
			t.Errorf("%d to %q, want 303 to login", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("both challenges, both in the 401", func(t *testing.T) {
		b := ssoAuth(t, fakeOidc())
		if err := b.Set(`Basic pw="` + vector + `", ` + ssoValue); err != nil {
			t.Fatal(err)
		}
		rec := serve(b.Handler(origin()), req(""))
		if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="h.tunneled.pizza", charset="UTF-8", `+public {
			t.Errorf("WWW-Authenticate = %q", got)
		}
	})
	t.Run("the owner passes as if listed, token or cookie", func(t *testing.T) {
		g := owned(ssoAuth(t, fakeOidc()), "github:9").Handler(origin())
		if rec := serve(g, req("Bearer unlisted")); rec.Code != 200 || rec.Header().Get("X-Sub") != "github:9" {
			t.Errorf("the owner's token: %d, sub %q; want 200 github:9", rec.Code, rec.Header().Get("X-Sub"))
		}
		r := req("")
		r.AddCookie(&http.Cookie{Name: CookieName, Value: mintCookie(cookieKey([]byte("s3cr3t")), "bearer", "github:9", ssoValue, a.now())})
		if rec := serve(g, r); rec.Code != 200 || rec.Header().Get("X-Sub") != "github:9" {
			t.Errorf("the owner's cookie: %d, sub %q; want 200 github:9", rec.Code, rec.Header().Get("X-Sub"))
		}
	})
	t.Run("a password tunnel's owner still needs the password", func(t *testing.T) {
		g := owned(pwAuth(t, fakeOidc()), "github:9").Handler(origin())
		if rec := serve(g, req("Bearer unlisted")); rec.Code != 401 {
			t.Errorf("the owner's token at a password tunnel's gate = %d, want 401", rec.Code)
		}
	})
	t.Run("a cookie keeps an origin's own Bearer", func(t *testing.T) {
		r := req("Bearer the-origins-own")
		r.AddCookie(&http.Cookie{Name: CookieName, Value: mintCookie(cookieKey([]byte("s3cr3t")), "bearer", "github:2", ssoValue, a.now())})
		rec := serve(h, r)
		if rec.Code != 200 || rec.Header().Get("X-Authorization") != "Bearer the-origins-own" || rec.Header().Get("X-Sub") != "github:2" {
			t.Errorf("%d, origin saw %q as %q; want its own Bearer, github:2", rec.Code, rec.Header().Get("X-Authorization"), rec.Header().Get("X-Sub"))
		}
	})
	t.Run("the MCP metadata needs nothing", func(t *testing.T) {
		r := httptest.NewRequest("GET", MCPMetadataPath, nil)
		r.Host = ssoHost
		rec := serve(h, r)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"resource":"https://h.tunneled.pizza/_tunneld/mcp"`) {
			t.Errorf("%d %q", rec.Code, rec.Body)
		}
	})
	t.Run("the issuer down is a 503 to come back to", func(t *testing.T) {
		o := fakeOidc()
		o.down = true
		rec := serve(ssoAuth(t, o).Handler(origin()), req("Bearer listed"))
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "30" || rec.Body.Len() != 0 {
			t.Errorf("%d, Retry-After %q, %d bytes; want a bodyless 503, 30", rec.Code, rec.Header().Get("Retry-After"), rec.Body.Len())
		}
	})
	t.Run("a forged sub never reaches the origin", func(t *testing.T) {
		// Every spelling an app might read as the same header: CGI-style
		// stacks map _ to -, and a repeated header is joined or the last kept.
		seen := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range r.Header {
				if strings.EqualFold(strings.ReplaceAll(k, "_", "-"), SubHeader) {
					w.Header().Add("X-Forged", k+"="+strings.Join(v, ","))
				}
			}
		})
		forgeries := map[string]func(http.Header){
			"set":                func(h http.Header) { h.Set(SubHeader, "github:1") },
			"after an empty one": func(h http.Header) { h[SubHeader] = []string{"", "github:1"} },
			"empty":              func(h http.Header) { h[SubHeader] = []string{""} },
			"with underscores":   func(h http.Header) { h["X_tunneld_sub"] = []string{"github:1"} },
			"in another case":    func(h http.Header) { h["x-tunneld-sub"] = []string{"github:1"} },
		}
		for name, g := range map[string]*AuthImpl{"public": New(), "password": protected(t)} {
			for how, forge := range forgeries {
				r := req("")
				r.SetBasicAuth("", password)
				forge(r.Header)
				if rec := serve(g.Handler(seen), r); len(rec.Header().Values("X-Forged")) != 0 {
					t.Errorf("%s, %s: the origin saw %q", name, how, rec.Header().Values("X-Forged"))
				}
			}
		}
	})
}
