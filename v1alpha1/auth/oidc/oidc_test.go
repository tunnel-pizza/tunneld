package oidc

import (
	"cmp"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// host is the tunnel these tests' tokens are for.
const host = "h.tunneled.pizza"

// testKey and otherKey are generated once a run: RSA generation is slow, and
// no test needs a fresh one.
var (
	testKey  = sync.OnceValue(func() *rsa.PrivateKey { return generate() })
	otherKey = sync.OnceValue(func() *rsa.PrivateKey { return generate() })
)

func generate() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

// kidOf is key's RFC 7638 thumbprint, the kid the provider gives it.
func kidOf(key *rsa.PrivateKey) string {
	sum, err := (&jose.JSONWebKey{Key: &key.PublicKey}).Thumbprint(crypto.SHA256)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(sum)
}

// issuer stands in for the provider: its discovery document, its keys, and a
// token endpoint that answers whatever its token function says. Configured
// before it serves, so no request races a test changing it.
type issuer struct {
	*httptest.Server
	name     string
	maxAge   string
	token    func(form url.Values) (int, any)
	keys     atomic.Pointer[[]jose.JSONWebKey]
	jwksHits atomic.Int32
	metaHits atomic.Int32
	failHits atomic.Int32
	down     atomic.Bool
	// hold, when set, keeps every discovery waiting until it is closed.
	hold chan struct{}
}

func withToken(f func(url.Values) (int, any)) func(*issuer) { return func(i *issuer) { i.token = f } }
func withName(name string) func(*issuer)                    { return func(i *issuer) { i.name = name } }
func withMaxAge(v string) func(*issuer)                     { return func(i *issuer) { i.maxAge = v } }
func withHold(c chan struct{}) func(*issuer)                { return func(i *issuer) { i.hold = c } }

func newIssuer(t *testing.T, opts ...func(*issuer)) *issuer {
	t.Helper()
	iss := &issuer{maxAge: "max-age=3600"}
	for _, o := range opts {
		o(iss)
	}
	iss.publish(testKey())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if iss.down.Load() {
			iss.failHits.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		iss.metaHits.Add(1)
		if iss.hold != nil {
			<-iss.hold
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 cmp.Or(iss.name, iss.URL),
			"authorization_endpoint": iss.URL + "/oauth/authorize",
			"token_endpoint":         iss.URL + "/oauth/token",
			"jwks_uri":               iss.URL + "/.well-known/jwks.json",
		})
	})
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		if iss.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		iss.jwksHits.Add(1)
		w.Header().Set("Cache-Control", iss.maxAge)
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: *iss.keys.Load()})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if iss.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = r.ParseForm()
		status, body := http.StatusBadRequest, any(map[string]string{"error": "invalid_grant"})
		if iss.token != nil {
			status, body = iss.token(r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	iss.Server = httptest.NewServer(mux)
	t.Cleanup(iss.Close)
	return iss
}

// publish makes keys the set jwks.json serves.
func (i *issuer) publish(keys ...*rsa.PrivateKey) {
	set := make([]jose.JSONWebKey, len(keys))
	for n, k := range keys {
		set[n] = jose.JSONWebKey{Key: &k.PublicKey, KeyID: kidOf(k), Algorithm: string(jose.RS256), Use: "sig"}
	}
	i.keys.Store(&set)
}

// against is an OidcImpl asking iss.
func against(iss *issuer, opts ...Option) *OidcImpl {
	return New(append([]Option{WithIssuer(func() string { return iss.URL }), WithHTTPClient(iss.Client())}, opts...)...)
}

// sign is claims as a JWS by key, RS256, kid its thumbprint, typ as given
// ("" for none).
func sign(t *testing.T, key *rsa.PrivateKey, typ string, claims any) string {
	t.Helper()
	opts := &jose.SignerOptions{}
	if typ != "" {
		opts = opts.WithType(jose.ContentType(typ))
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kidOf(key)}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(s).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// accessToken is an RFC 9068 access token from iss for host, signed by key,
// as edit leaves its claims.
func accessToken(t *testing.T, iss *issuer, key *rsa.PrivateKey, edit func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss": iss.URL, "sub": "github:1", "aud": []string{"https://" + host},
		"client_id": "c", "scope": "openid profile", "jti": "j",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if edit != nil {
		edit(claims)
	}
	return sign(t, key, "at+jwt", claims)
}

// TestOptions pins the defaults, and that nil keeps each: tunnel.pizza as the
// issuer, a client with a timeout, the wall clock, a logger.
func TestOptions(t *testing.T) {
	for name, o := range map[string]*OidcImpl{
		"none":    New(),
		"all nil": New(WithIssuer(nil), WithHTTPClient(nil), WithClock(nil), WithLog(nil)),
	} {
		if o.issuer() != "https://tunnel.pizza" || o.http == nil || o.http.Timeout != 10*time.Second || o.now == nil || o.log == nil {
			t.Errorf("%s: issuer %q, client %v, clock %v, log %v", name, o.issuer(), o.http, o.now != nil, o.log != nil)
		}
	}
	asked := 0
	o := New(WithIssuer(func() string { asked++; return "https://p.example" }))
	o.issuer()
	o.issuer()
	if asked != 2 {
		t.Errorf("the issuer was read %d times for two asks, want it read on every one", asked)
	}
}

// TestVerifyAccess pins RFC 9068 §4 as tunneld applies it: typ at+jwt, RS256
// under a published key, iss exact, aud holding the resource, sub, exp and
// iat present and within a minute's skew. Anything else is ErrInvalidToken.
func TestVerifyAccess(t *testing.T) {
	iss := newIssuer(t)
	o := against(iss)
	site, mcp := "https://"+host, "https://"+host+"/_tunneld/mcp"
	now := time.Now()
	plain := func(typ string) string {
		return sign(t, testKey(), typ, map[string]any{"iss": iss.URL, "sub": "github:1", "aud": site, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	}
	hs256 := func() string {
		s, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")},
			(&jose.SignerOptions{}).WithType("at+jwt"))
		tok, _ := jwt.Signed(s).Claims(map[string]any{"iss": iss.URL, "sub": "github:1", "aud": site,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}).Serialize()
		return tok
	}
	none := func() string {
		h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"at+jwt"}`))
		p := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + iss.URL + `","sub":"github:1","aud":"` + site + `"}`))
		return h + "." + p + "."
	}
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	drop := func(k string) func(map[string]any) { return func(m map[string]any) { delete(m, k) } }
	for name, tc := range map[string]struct {
		token, resource string
		ok              bool
	}{
		"valid":                    {accessToken(t, iss, testKey(), nil), site, true},
		"aud as a string":          {accessToken(t, iss, testKey(), set("aud", site)), site, true},
		"aud holding mcp, for mcp": {accessToken(t, iss, testKey(), set("aud", []string{site, mcp})), mcp, true},
		"site aud, for mcp":        {accessToken(t, iss, testKey(), nil), mcp, false},
		"mcp aud only, for site":   {accessToken(t, iss, testKey(), set("aud", []string{mcp})), site, false},
		"another host":             {accessToken(t, iss, testKey(), set("aud", "https://other.tunneled.pizza")), site, false},
		"typ application/at+jwt":   {plain("application/at+jwt"), site, true},
		"typ JWT (an ID token)":    {plain("JWT"), site, false},
		"no typ":                   {plain(""), site, false},
		"HS256":                    {hs256(), site, false},
		"alg none":                 {none(), site, false},
		"another issuer":           {accessToken(t, iss, testKey(), set("iss", "https://evil.example")), site, false},
		"expired 30s ago (skew)":   {accessToken(t, iss, testKey(), set("exp", now.Add(-30*time.Second).Unix())), site, true},
		"expired 90s ago":          {accessToken(t, iss, testKey(), set("exp", now.Add(-90*time.Second).Unix())), site, false},
		"issued 90s ahead":         {accessToken(t, iss, testKey(), set("iat", now.Add(90*time.Second).Unix())), site, false},
		"no exp":                   {accessToken(t, iss, testKey(), drop("exp")), site, false},
		"no iat":                   {accessToken(t, iss, testKey(), drop("iat")), site, false},
		"no sub":                   {accessToken(t, iss, testKey(), drop("sub")), site, false},
		"an unpublished key":       {accessToken(t, iss, otherKey(), nil), site, false},
		"garbage":                  {"nope", site, false},
	} {
		t.Run(name, func(t *testing.T) {
			sub, err := o.VerifyAccess(t.Context(), tc.token, tc.resource)
			if tc.ok && (err != nil || sub != "github:1") {
				t.Fatalf("VerifyAccess = %q, %v; want github:1", sub, err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("VerifyAccess = %q, %v; want ErrInvalidToken", sub, err)
			}
		})
	}
}

// TestDiscovery pins the issuer's metadata: refused unless it names the
// issuer exactly (RFC 8414 §3.3), kept once fetched, forgotten when the
// issuer changes, and an issuer that cannot be asked is ErrUnavailable,
// never ErrInvalidToken.
func TestVerifyAccessClaims(t *testing.T) {
	iss := newIssuer(t)
	o := against(iss)
	tok := accessToken(t, iss, testKey(), func(c map[string]any) {
		c["client_id"] = "https://tunnel.pizza"
		c["scope"] = "tunnel:token"
	})
	c, err := o.VerifyAccessClaims(t.Context(), tok, "https://"+host)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "github:1" || c.ClientID != "https://tunnel.pizza" || c.Scope != "tunnel:token" {
		t.Errorf("claims %+v", c)
	}
	if _, err := o.VerifyAccessClaims(t.Context(), tok, "https://elsewhere.example"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("another resource: %v, want ErrInvalidToken", err)
	}
}

func TestDiscovery(t *testing.T) {
	t.Run("kept once fetched", func(t *testing.T) {
		iss := newIssuer(t)
		o := against(iss)
		for range 3 {
			if _, err := o.VerifyAccess(t.Context(), accessToken(t, iss, testKey(), nil), "https://"+host); err != nil {
				t.Fatal(err)
			}
		}
		if n := iss.metaHits.Load(); n != 1 {
			t.Errorf("discovery fetched %d times, want 1", n)
		}
		d, err := o.Endpoints(t.Context())
		want := Discovery{Issuer: iss.URL, AuthorizationEndpoint: iss.URL + "/oauth/authorize",
			TokenEndpoint: iss.URL + "/oauth/token", JWKSURI: iss.URL + "/.well-known/jwks.json"}
		if err != nil || d != want {
			t.Errorf("Endpoints = %+v, %v; want %+v", d, err, want)
		}
	})
	t.Run("a new issuer forgets the old", func(t *testing.T) {
		one, two := newIssuer(t), newIssuer(t)
		current := one.URL
		o := New(WithIssuer(func() string { return current }), WithHTTPClient(one.Client()))
		if _, err := o.VerifyAccess(t.Context(), accessToken(t, one, testKey(), nil), "https://"+host); err != nil {
			t.Fatal(err)
		}
		current = two.URL
		if _, err := o.VerifyAccess(t.Context(), accessToken(t, one, testKey(), nil), "https://"+host); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("the old issuer's token after the change = %v, want ErrInvalidToken", err)
		}
		if _, err := o.VerifyAccess(t.Context(), accessToken(t, two, testKey(), nil), "https://"+host); err != nil || two.metaHits.Load() != 1 {
			t.Errorf("the new issuer's token = %v, %d discoveries; want valid, 1", err, two.metaHits.Load())
		}
	})
	t.Run("an issuer that names another", func(t *testing.T) {
		iss := newIssuer(t, withName("https://evil.example"))
		if _, err := against(iss).VerifyAccess(t.Context(), accessToken(t, iss, testKey(), nil), "https://"+host); !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("an issuer that is down", func(t *testing.T) {
		iss := newIssuer(t)
		o := against(iss)
		iss.down.Store(true)
		if _, err := o.VerifyAccess(t.Context(), accessToken(t, iss, testKey(), nil), "https://"+host); !errors.Is(err, ErrUnavailable) {
			t.Errorf("VerifyAccess err = %v, want ErrUnavailable", err)
		}
		if _, err := o.Endpoints(t.Context()); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Endpoints err = %v, want ErrUnavailable", err)
		}
	})
}

// TestAnOutage pins the cache while the issuer is down: one fetch at a time,
// shared by everyone asking, and a failure is the answer for retryGap, so an
// outage costs one fetch per gap rather than one per request queued behind
// the last.
func TestAnOutage(t *testing.T) {
	iss := newIssuer(t)
	base := time.Now()
	var mu sync.Mutex
	now := base
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	o := against(iss, WithClock(clock))
	tok := accessToken(t, iss, testKey(), nil)
	iss.down.Store(true)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := o.VerifyAccess(t.Context(), tok, "https://"+host); !errors.Is(err, ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
		})
	}
	wg.Wait()
	for range 3 {
		if _, err := o.Endpoints(t.Context()); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Endpoints within the gap = %v, want ErrUnavailable", err)
		}
	}
	if n := iss.failHits.Load(); n != 1 {
		t.Errorf("the issuer was asked %d times within the gap, want 1", n)
	}
	mu.Lock()
	now = base.Add(retryGap)
	mu.Unlock()
	iss.down.Store(false)
	if _, err := o.VerifyAccess(t.Context(), tok, "https://"+host); err != nil {
		t.Errorf("the issuer back after the gap = %v, want the token valid", err)
	}
}

// TestAnAskerHangingUp pins a fetch as shared: the asker that started it
// hanging up neither ends it nor makes the next asker fetch again.
func TestAnAskerHangingUp(t *testing.T) {
	hold := make(chan struct{})
	iss := newIssuer(t, withHold(hold))
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	o := against(iss)
	tok := accessToken(t, iss, testKey(), nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := o.VerifyAccess(ctx, tok, "https://"+host); done <- err }()
	for iss.metaHits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Errorf("the asker that hung up = %v, want ErrUnavailable", err)
	}
	close(hold)
	if _, err := o.VerifyAccess(t.Context(), tok, "https://"+host); err != nil || iss.metaHits.Load() != 1 {
		t.Errorf("the next asker = %v after %d discoveries; want valid from the one fetch", err, iss.metaHits.Load())
	}
}

// TestKeyRotation pins the JWKS cache: a kid it has not seen is fetched for,
// but at most once a minute, so a rotation needs no restart and a stream of
// made-up kids cannot make every request a fetch; and a set past its max-age
// is fetched again.
func TestKeyRotation(t *testing.T) {
	iss := newIssuer(t, withMaxAge("max-age=300"))
	base := time.Now()
	now := base
	o := against(iss, WithClock(func() time.Time { return now }))
	site := "https://" + host
	if _, err := o.VerifyAccess(t.Context(), accessToken(t, iss, testKey(), nil), site); err != nil {
		t.Fatal(err)
	}
	iss.publish(testKey(), otherKey())
	if _, err := o.VerifyAccess(t.Context(), accessToken(t, iss, otherKey(), nil), site); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a new kid within the minute = %v, want ErrInvalidToken without a fetch", err)
	}
	if n := iss.jwksHits.Load(); n != 1 {
		t.Errorf("JWKS fetched %d times within the minute, want 1", n)
	}
	now = base.Add(61 * time.Second)
	if _, err := o.VerifyAccess(t.Context(), accessToken(t, iss, otherKey(), nil), site); err != nil {
		t.Errorf("the new kid a minute on = %v, want it fetched and valid", err)
	}
	if n := iss.jwksHits.Load(); n != 2 {
		t.Errorf("JWKS fetched %d times, want 2", n)
	}
	now = base.Add(7 * time.Minute)
	if _, err := o.VerifyAccess(t.Context(), accessToken(t, iss, testKey(), nil), site); err != nil {
		t.Fatal(err)
	}
	if n := iss.jwksHits.Load(); n != 3 {
		t.Errorf("JWKS fetched %d times past its max-age, want 3", n)
	}
}

// TestCacheFor pins how long an issuer's answer is kept: its max-age, within
// five minutes and a day.
func TestCacheFor(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"max-age=3600":        time.Hour,
		"public, max-age=600": 10 * time.Minute,
		"max-age=10":          5 * time.Minute,
		"max-age=999999":      24 * time.Hour,
		"no-store":            5 * time.Minute,
		"":                    5 * time.Minute,
		"max-age=nope":        5 * time.Minute,
	} {
		h := http.Header{}
		h.Set("Cache-Control", in)
		if got := cacheFor(h); got != want {
			t.Errorf("cacheFor(%q) = %v, want %v", in, got, want)
		}
	}
}

const testClient = "https://" + host + "/_tunneld/client.json"

// idToken is an ID token from iss for testClient with nonce "n0nce", signed
// by key, as edit leaves its claims.
func idToken(t *testing.T, iss *issuer, key *rsa.PrivateKey, edit func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss": iss.URL, "sub": "github:1", "aud": testClient, "nonce": "n0nce",
		"preferred_username": "alice", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if edit != nil {
		edit(claims)
	}
	return sign(t, key, "JWT", claims)
}

// TestExchange pins the code's trade at the token endpoint (RFC 6749 §4.1.3,
// RFC 7636 §4.5, RFC 7523 §2.2): a form POST, every parameter there, the
// client's assertion included; a refusal is ErrRefused, an issuer that cannot
// answer ErrUnavailable.
func TestExchange(t *testing.T) {
	want := url.Values{
		"grant_type": {"authorization_code"}, "code": {"c0de"}, "client_id": {testClient},
		"redirect_uri": {"https://" + host + "/_tunneld/callback"}, "code_verifier": {"v3rifier"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {"an-assertion"},
	}
	for name, tc := range map[string]struct {
		token   func(url.Values) (int, any)
		down    bool
		wantErr error
	}{
		"traded": {func(f url.Values) (int, any) {
			for k := range want {
				if f.Get(k) != want.Get(k) {
					return 400, map[string]string{"error": "invalid_request", "error_description": k}
				}
			}
			return 200, map[string]string{"id_token": "the-id-token", "token_type": "Bearer"}
		}, false, nil},
		"refused":          {func(url.Values) (int, any) { return 400, map[string]string{"error": "invalid_grant"} }, false, ErrRefused},
		"no id_token":      {func(url.Values) (int, any) { return 200, map[string]string{"access_token": "x"} }, false, ErrRefused},
		"the endpoint 500": {func(url.Values) (int, any) { return 500, map[string]string{} }, false, ErrUnavailable},
		// A refusal is RFC 6749 §5.2's: a 4xx naming an error. A rate limit,
		// or a proxy's page in front of the endpoint, is the issuer not
		// answering.
		"rate limited":      {func(url.Values) (int, any) { return 429, map[string]string{"error": "slow_down"} }, false, ErrUnavailable},
		"a proxy's page":    {func(url.Values) (int, any) { return 403, "<html>blocked</html>" }, false, ErrUnavailable},
		"a 4xx naming none": {func(url.Values) (int, any) { return 400, map[string]string{} }, false, ErrUnavailable},
		"the issuer down":   {nil, true, ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			iss := newIssuer(t, withToken(tc.token))
			o := against(iss)
			if tc.down {
				iss.down.Store(true)
			}
			got, err := o.Exchange(t.Context(), "c0de", "v3rifier", want.Get("redirect_uri"), testClient, "an-assertion")
			if tc.wantErr == nil && (err != nil || got != "the-id-token") {
				t.Fatalf("Exchange = %q, %v; want the-id-token", got, err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Exchange = %q, %v; want %v", got, err, tc.wantErr)
			}
		})
	}
}

// TestExchangePublic pins a public client's trade: no assertion, so neither
// assertion parameter is sent.
func TestExchangePublic(t *testing.T) {
	iss := newIssuer(t, withToken(func(f url.Values) (int, any) {
		if f.Has("client_assertion") || f.Has("client_assertion_type") {
			return 400, map[string]string{"error": "invalid_request"}
		}
		return 200, map[string]string{"id_token": "the-id-token"}
	}))
	if got, err := against(iss).Exchange(t.Context(), "c0de", "v3rifier", "https://"+host+"/_tunneld/callback", testClient, ""); err != nil || got != "the-id-token" {
		t.Fatalf("Exchange = %q, %v; want the-id-token", got, err)
	}
}

// TestVerifyID pins OIDC Core §3.1.3.7 as the sign-in applies it.
func TestVerifyID(t *testing.T) {
	iss := newIssuer(t)
	o := against(iss)
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	now := time.Now()
	for name, tc := range map[string]struct {
		token string
		ok    bool
	}{
		"valid":                         {idToken(t, iss, testKey(), nil), true},
		"another nonce":                 {idToken(t, iss, testKey(), set("nonce", "other")), false},
		"no nonce":                      {idToken(t, iss, testKey(), func(m map[string]any) { delete(m, "nonce") }), false},
		"another client":                {idToken(t, iss, testKey(), set("aud", "https://other/client.json")), false},
		"two audiences, no azp":         {idToken(t, iss, testKey(), set("aud", []string{testClient, "x"})), false},
		"two audiences, azp the client": {idToken(t, iss, testKey(), func(m map[string]any) { m["aud"] = []string{testClient, "x"}; m["azp"] = testClient }), true},
		"azp another":                   {idToken(t, iss, testKey(), set("azp", "x")), false},
		"another issuer":                {idToken(t, iss, testKey(), set("iss", "https://evil.example")), false},
		"expired":                       {idToken(t, iss, testKey(), set("exp", now.Add(-2*time.Minute).Unix())), false},
		"an access token":               {sign(t, testKey(), "at+jwt", map[string]any{"iss": iss.URL, "sub": "github:1", "aud": testClient, "nonce": "n0nce", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}), false},
		"an unpublished key":            {idToken(t, iss, otherKey(), nil), false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := o.VerifyID(t.Context(), tc.token, testClient, "n0nce")
			if tc.ok && (err != nil || c != (IDClaims{Subject: "github:1", PreferredUsername: "alice"})) {
				t.Fatalf("VerifyID = %+v, %v; want github:1 alice", c, err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("VerifyID = %+v, %v; want ErrInvalidToken", c, err)
			}
		})
	}
}
