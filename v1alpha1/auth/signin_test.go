package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// muxOf is a's Handlers on a ServeMux, as the router puts them.
func muxOf(a *AuthImpl) *http.ServeMux {
	m := http.NewServeMux()
	for p, h := range a.Handlers("/_tunneld/") {
		m.HandleFunc(p, h)
	}
	return m
}

func at(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = ssoHost
	return r
}

// TestSeal pins the flow cookie: only this tunnel's key opens what it
// sealed, and a changed byte opens nothing.
func TestSeal(t *testing.T) {
	key := flowKey([]byte("s3cr3t"))
	f := flow{State: "s", Nonce: "n", Verifier: "v", Next: "/app", Exp: 1}
	tok, err := seal(key, f)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := unseal(key, tok); !ok || got != f {
		t.Errorf("unseal = %+v, %v; want %+v", got, ok, f)
	}
	b, _ := base64.RawURLEncoding.DecodeString(tok)
	b[len(b)-1] ^= 1
	for name, bad := range map[string]struct {
		key []byte
		tok string
	}{
		"another tunnel's key": {flowKey([]byte("other")), tok},
		"the cookie key":       {cookieKey([]byte("s3cr3t")), tok},
		"tampered":             {key, base64.RawURLEncoding.EncodeToString(b)},
		"garbage":              {key, "nope"},
		"no key":               {nil, tok},
	} {
		if _, ok := unseal(bad.key, bad.tok); ok {
			t.Errorf("%s opened the flow", name)
		}
	}
	if flowKey(nil) != nil {
		t.Error("an empty secret derived a key")
	}
}

// TestClientDocument pins tunneld's Client ID Metadata Document: its own URL
// as client_id, the hostname as its name, one callback, and the key derived
// from the tunnel secret; none before there is a secret.
func TestClientDocument(t *testing.T) {
	m := muxOf(New(WithSecret(func() []byte { return []byte("s3cr3t") })))
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, at("GET", "/_tunneld/client.json"))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %v", rec.Code, rec.Body, err)
	}
	var key map[string]any
	raw, _ := json.Marshal(assertionJWK(assertionKey([]byte("s3cr3t"))))
	_ = json.Unmarshal(raw, &key)
	want := map[string]any{
		"client_id": testClient, "client_name": ssoHost,
		"redirect_uris":              []any{"https://" + ssoHost + "/_tunneld/callback"},
		"token_endpoint_auth_method": "private_key_jwt",
		"jwks":                       map[string]any{"keys": []any{key}},
		"grant_types":                []any{"authorization_code"},
		"response_types":             []any{"code"}, "scope": "openid profile",
	}
	if !equalJSON(got, want) {
		t.Errorf("client document = %v, want %v", got, want)
	}
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, at("POST", "/_tunneld/client.json"))
	if rec.Code != 405 {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	muxOf(New()).ServeHTTP(rec, at("GET", "/_tunneld/client.json"))
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("no secret: %d, Retry-After %q; want 503/2", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// started is the sign-in a GET of target began: where it sent the browser,
// and what it sealed.
func started(t *testing.T, a *AuthImpl, target string) (*url.URL, flow, *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	muxOf(a).ServeHTTP(rec, at("GET", target))
	if rec.Code != 303 {
		t.Fatalf("GET %s = %d %q, want 303", target, rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, flowCookie) {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no flow cookie")
	}
	f, ok := unseal(flowKey([]byte("s3cr3t")), cookie.Value)
	if !ok {
		t.Fatal("the flow cookie does not open")
	}
	return loc, f, cookie
}

// TestSignIn pins where the login page sends a browser on a tunnel set to
// Bearer (OIDC Core §3.1.2.1): the provider's authorization endpoint, as
// this tunnel's own client, code flow with S256 PKCE, the hostname as the
// resource, a fresh state and nonce sealed in a cookie only the callback
// sees.
func TestSignIn(t *testing.T) {
	a := ssoAuth(t, fakeOidc())
	loc, f, cookie := started(t, a, "/_tunneld/login?next=/app")
	q := loc.Query()
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != fakeIssuer+"/oauth/authorize" {
		t.Errorf("sent to %q, want the provider's authorization endpoint", got)
	}
	sum := sha256.Sum256([]byte(f.Verifier))
	for k, want := range map[string]string{
		"response_type": "code", "client_id": testClient, "redirect_uri": "https://" + ssoHost + "/_tunneld/callback",
		"scope": "openid profile", "resource": "https://" + ssoHost, "state": f.State, "nonce": f.Nonce,
		"code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]), "code_challenge_method": "S256", "prompt": "",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if len(f.State) < 43 || len(f.Nonce) < 43 || len(f.Verifier) < 43 || f.Next != "/app" {
		t.Errorf("flow = %+v, want 32-byte state, nonce and verifier and next /app", f)
	}
	// __Host-: no sibling tunnel on the shared domain can set one, or shadow
	// this one with a longer path.
	if !strings.HasPrefix(cookie.Name, "__Host-") || cookie.Path != "/" || cookie.Domain != "" ||
		!cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 600 {
		t.Errorf("flow cookie %+v", cookie)
	}
	if loc, _, _ := started(t, a, "/_tunneld/login?next=/app&sso=1&prompt=login"); loc.Query().Get("prompt") != "login" {
		t.Error("prompt=login was not passed on")
	}
	if _, f2, _ := started(t, a, "/_tunneld/login?next=/app"); f2.State == f.State {
		t.Error("two sign-ins shared a state")
	}

	t.Run("with a password too, the page offers both", func(t *testing.T) {
		b := ssoAuth(t, fakeOidc())
		if err := b.Set(`Basic pw="` + vector + `", ` + ssoValue); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		muxOf(b).ServeHTTP(rec, at("GET", "/_tunneld/login?next=/app"))
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, `name="password"`) || !strings.Contains(body, "Sign in with tunnel.pizza") ||
			!strings.Contains(body, "sso=1") {
			t.Errorf("%d, page:\n%s", rec.Code, body)
		}
		if loc, _, _ := started(t, b, "/_tunneld/login?next=/app&sso=1"); !strings.HasPrefix(loc.String(), fakeIssuer) {
			t.Errorf("the link went to %q", loc)
		}
	})
	t.Run("the provider down", func(t *testing.T) {
		o := fakeOidc()
		o.down = true
		rec := httptest.NewRecorder()
		muxOf(ssoAuth(t, o)).ServeHTTP(rec, at("GET", "/_tunneld/login?next=/app"))
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), "couldn") {
			t.Errorf("%d %q, want a 503 page saying tunnel.pizza couldn't be reached", rec.Code, rec.Body)
		}
	})
	t.Run("no secret yet", func(t *testing.T) {
		n := New(WithOidc(fakeOidc()))
		n.Set(ssoValue)
		rec := httptest.NewRecorder()
		muxOf(n).ServeHTTP(rec, at("GET", "/_tunneld/login"))
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "2" {
			t.Errorf("%d, Retry-After %q; want 503, 2", rec.Code, rec.Header().Get("Retry-After"))
		}
		if !strings.Contains(rec.Body.String(), "Sign in with tunnel.pizza") {
			t.Errorf("the page offers no way to try again:\n%s", rec.Body)
		}
	})
	t.Run("logout stays out: the sign-in is a link, never started for you", func(t *testing.T) {
		rec := httptest.NewRecorder()
		muxOf(a).ServeHTTP(rec, at("GET", "/_tunneld/logout?next=/app"))
		body := rec.Body.String()
		if rec.Code != 200 || rec.Header().Get("Location") != "" || !strings.Contains(body, "Signed out of "+ssoHost+".") ||
			!strings.Contains(body, "Sign in with tunnel.pizza") || !strings.Contains(body, "sso=1") {
			t.Errorf("%d to %q, page:\n%s", rec.Code, rec.Header().Get("Location"), body)
		}
		for _, c := range rec.Result().Cookies() {
			if strings.HasPrefix(c.Name, flowCookie) && c.MaxAge >= 0 {
				t.Errorf("logout began a sign-in: %s", c.Name)
			}
		}
	})
	t.Run("a password POST to a bearer-only tunnel goes to sign in", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := at("POST", "/_tunneld/login")
		req.Body = http.NoBody
		muxOf(a).ServeHTTP(rec, req)
		if rec.Code != 303 || !strings.HasPrefix(rec.Header().Get("Location"), "/_tunneld/login?next=") {
			t.Errorf("%d to %q", rec.Code, rec.Header().Get("Location"))
		}
	})
}

// TestCallback pins the callback (OIDC Core §3.1.2.5, RFC 9207): the sealed
// flow, its state and the provider's iss, then the code traded and the ID
// token checked; a listed sub gets the auth cookie and lands where it
// started, an unlisted one a 403 page naming who they signed in as, and
// anything else the "Sign-in failed" page with no cookie.
func TestCallback(t *testing.T) {
	// callback is a sign-in from login to callback against o: edit changes
	// the callback's query and the sealed flow; o is told the flow's
	// verifier and nonce, as the provider would know them, before edit runs.
	callback := func(t *testing.T, o *oidcOf, edit func(q url.Values, f *flow, o *oidcOf)) (*AuthImpl, *httptest.ResponseRecorder) {
		t.Helper()
		a := ssoAuth(t, o)
		_, f, cookie := started(t, a, "/_tunneld/login?next=/app")
		o.verifier, o.nonce = f.Verifier, f.Nonce
		q := url.Values{"code": {"c0de"}, "state": {f.State}, "iss": {fakeIssuer}}
		if edit != nil {
			edit(q, &f, o)
			sealed, err := seal(flowKey([]byte("s3cr3t")), f)
			if err != nil {
				t.Fatal(err)
			}
			cookie.Value = sealed
		}
		req := at("GET", "/_tunneld/callback?"+q.Encode())
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		muxOf(a).ServeHTTP(rec, req)
		return a, rec
	}
	authCookie := func(rec *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range rec.Result().Cookies() {
			if c.Name == CookieName && c.Value != "" {
				return c
			}
		}
		return nil
	}

	t.Run("listed: the cookie, and back to where it started", func(t *testing.T) {
		a, rec := callback(t, fakeOidc(), nil)
		c := authCookie(rec)
		if rec.Code != 303 || rec.Header().Get("Location") != "/app" || c == nil {
			t.Fatalf("%d to %q, cookie %v; want 303 /app with the auth cookie", rec.Code, rec.Header().Get("Location"), c)
		}
		cleared := false
		for _, k := range rec.Result().Cookies() {
			if strings.HasPrefix(k.Name, flowCookie) && k.MaxAge < 0 && k.Path == "/" && k.Secure {
				cleared = true
			}
		}
		if !cleared {
			t.Error("the flow cookie was not cleared")
		}
		req := at("GET", "/x")
		req.AddCookie(c)
		if rec := serve(a.Handler(origin()), req); rec.Code != 200 || rec.Header().Get("X-Sub") != "github:1" {
			t.Errorf("the cookie at the gate: %d, sub %q; want 200 github:1", rec.Code, rec.Header().Get("X-Sub"))
		}
	})
	t.Run("the code exchange is signed with the tunnel's own key", func(t *testing.T) {
		o := fakeOidc()
		callback(t, o, nil)
		tok, err := jwt.ParseSigned(o.assertion, []jose.SignatureAlgorithm{jose.EdDSA})
		if err != nil {
			t.Fatalf("assertion: %v", err)
		}
		var c jwt.Claims
		if err := tok.Claims(assertionJWK(assertionKey([]byte("s3cr3t"))).Key, &c); err != nil {
			t.Fatalf("assertion signature: %v", err)
		}
		if c.Issuer != testClient || c.Subject != testClient || !c.Audience.Contains(fakeIssuer) {
			t.Errorf("assertion iss %q sub %q aud %v; want the client twice and the issuer", c.Issuer, c.Subject, c.Audience)
		}
	})
	t.Run("the owner, though not listed, signs in", func(t *testing.T) {
		o := fakeOidc()
		o.sub, o.username = "github:9", "owner"
		a := owned(ssoAuth(t, o), "github:9")
		_, f, cookie := started(t, a, "/_tunneld/login?next=/app")
		o.verifier, o.nonce = f.Verifier, f.Nonce
		req := at("GET", "/_tunneld/callback?"+url.Values{"code": {"c0de"}, "state": {f.State}, "iss": {fakeIssuer}}.Encode())
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		muxOf(a).ServeHTTP(rec, req)
		if rec.Code != 303 || rec.Header().Get("Location") != "/app" || authCookie(rec) == nil {
			t.Errorf("%d to %q, cookie %v; want 303 /app with the auth cookie", rec.Code, rec.Header().Get("Location"), authCookie(rec))
		}
	})
	t.Run("not listed: a 403 naming who", func(t *testing.T) {
		o := fakeOidc()
		o.sub, o.username = "github:9", "mallory"
		_, rec := callback(t, o, nil)
		body := rec.Body.String()
		if rec.Code != 403 || !strings.Contains(body, "Signed in as mallory") || !strings.Contains(body, "Use another account") ||
			!strings.Contains(body, "prompt=login") || authCookie(rec) != nil {
			t.Errorf("%d, page:\n%s", rec.Code, body)
		}
	})
	for name, tc := range map[string]struct {
		edit   func(q url.Values, f *flow, o *oidcOf)
		status int
		says   string
	}{
		"another state":           {func(q url.Values, _ *flow, _ *oidcOf) { q.Set("state", "other") }, 200, "Sign-in failed"},
		"no iss":                  {func(q url.Values, _ *flow, _ *oidcOf) { q.Del("iss") }, 200, "Sign-in failed"},
		"another iss":             {func(q url.Values, _ *flow, _ *oidcOf) { q.Set("iss", "https://evil.example") }, 200, "Sign-in failed"},
		"cancelled":               {func(q url.Values, _ *flow, _ *oidcOf) { q.Set("error", "access_denied") }, 200, "cancelled"},
		"an error":                {func(q url.Values, _ *flow, _ *oidcOf) { q.Set("error", "server_error") }, 200, "Sign-in failed"},
		"an expired flow":         {func(_ url.Values, f *flow, _ *oidcOf) { f.Exp = time.Now().Add(-time.Second).Unix() }, 200, "Sign-in failed"},
		"another nonce":           {func(_ url.Values, _ *flow, o *oidcOf) { o.nonce = "other" }, 200, "Sign-in failed"},
		"a refused code":          {func(q url.Values, _ *flow, _ *oidcOf) { q.Set("code", "other") }, 200, "Sign-in failed"},
		"the token endpoint down": {func(_ url.Values, _ *flow, o *oidcOf) { o.exchangeDown = true }, 503, "couldn"},
		"the provider down":       {func(_ url.Values, _ *flow, o *oidcOf) { o.down = true }, 503, "couldn"},
	} {
		t.Run(name, func(t *testing.T) {
			_, rec := callback(t, fakeOidc(), tc.edit)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.says) || authCookie(rec) != nil {
				t.Errorf("%d, cookie %v, page:\n%s", rec.Code, authCookie(rec), rec.Body)
			}
		})
	}
	t.Run("two sign-ins at once, both finish", func(t *testing.T) {
		// A browser keeps one cookie per name and path: two tabs signing in
		// at once must not share one.
		o := fakeOidc()
		a := ssoAuth(t, o)
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		site, _ := url.Parse("https://" + ssoHost + "/")
		_, first, c1 := started(t, a, "/_tunneld/login?next=/one")
		jar.SetCookies(site, []*http.Cookie{c1})
		_, second, c2 := started(t, a, "/_tunneld/login?next=/two")
		jar.SetCookies(site, []*http.Cookie{c2})
		for _, f := range []flow{first, second} {
			o.verifier, o.nonce = f.Verifier, f.Nonce
			req := at("GET", "/_tunneld/callback?"+url.Values{"code": {"c0de"}, "state": {f.State}, "iss": {fakeIssuer}}.Encode())
			for _, c := range jar.Cookies(site) {
				req.AddCookie(c)
			}
			rec := httptest.NewRecorder()
			muxOf(a).ServeHTTP(rec, req)
			if rec.Code != 303 || rec.Header().Get("Location") != f.Next || authCookie(rec) == nil {
				t.Errorf("the sign-in for %s: %d to %q, cookie %v", f.Next, rec.Code, rec.Header().Get("Location"), authCookie(rec))
			}
		}
	})
	t.Run("no flow cookie", func(t *testing.T) {
		rec := httptest.NewRecorder()
		muxOf(ssoAuth(t, fakeOidc())).ServeHTTP(rec, at("GET", "/_tunneld/callback?code=c0de&state=s&iss="+url.QueryEscape(fakeIssuer)))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Sign-in failed") || authCookie(rec) != nil {
			t.Errorf("%d %q", rec.Code, rec.Body)
		}
	})
}
