package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

var tokenNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func TestTokenKeyIsItsOwn(t *testing.T) {
	secret := []byte("s3cr3t")
	if tokenKey(secret).Equal(assertionKey(secret)) {
		t.Error("the token key is the assertion key; each job gets its own label")
	}
	if tokenKey(nil) != nil {
		t.Error("a key from no secret")
	}
}

func TestMintedTokenShape(t *testing.T) {
	key := tokenKey([]byte("s3cr3t"))
	tok, err := mintToken(key, ssoHost, "github:1", tokenNow, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		t.Fatal(err)
	}
	if typ, _ := parsed.Headers[0].ExtraHeaders[jose.HeaderType].(string); typ != "at+jwt" {
		t.Errorf("typ %q", typ)
	}
	var c struct {
		jwt.Claims
		ClientID string `json:"client_id"`
	}
	if err := parsed.Claims(key.Public(), &c); err != nil {
		t.Fatal(err)
	}
	host := "https://" + ssoHost
	if c.Issuer != host || !c.Audience.Contains(host) || c.Subject != "github:1" ||
		c.ClientID != host+"/_tunneld/client.json" || len(c.ID) < 16 ||
		!c.Expiry.Time().Equal(tokenNow.Add(30*24*time.Hour)) || !c.IssuedAt.Time().Equal(tokenNow) {
		t.Errorf("claims %+v", c)
	}
	forever, _ := mintToken(key, ssoHost, "github:1", tokenNow, 0)
	p, _ := jwt.ParseSigned(forever, []jose.SignatureAlgorithm{jose.EdDSA})
	var f jwt.Claims
	_ = p.Claims(key.Public(), &f)
	if f.Expiry != nil {
		t.Error("lifetime 0 still has exp")
	}
}

func TestMintRefusesANegativeLifetime(t *testing.T) {
	if tok, err := mintToken(tokenKey([]byte("s3cr3t")), ssoHost, "github:1", tokenNow, -time.Hour); err == nil || tok != "" {
		t.Errorf("minted %q, %v; want no token and an error", tok, err)
	}
}

func TestVerifyToken(t *testing.T) {
	key := tokenKey([]byte("s3cr3t"))
	good, _ := mintToken(key, ssoHost, "github:1", tokenNow, 7*24*time.Hour)
	forever, _ := mintToken(key, ssoHost, "github:1", tokenNow, 0)
	if err := verifyToken(key, ssoHost, "github:1", good, tokenNow.Add(time.Hour)); err != nil {
		t.Errorf("good: %v", err)
	}
	if err := verifyToken(key, ssoHost, "github:1", forever, tokenNow.Add(10*365*24*time.Hour)); err != nil {
		t.Errorf("no expiry, years later: %v", err)
	}
	other, _ := mintToken(key, "other.tunneled.pizza", "github:1", tokenNow, time.Hour)
	tampered := good[:strings.LastIndex(good, ".")+1] + "AAAA"
	for name, tc := range map[string]struct {
		key   []byte
		owner string
		tok   string
		at    time.Time
	}{
		"expired":                  {[]byte("s3cr3t"), "github:1", good, tokenNow.Add(8 * 24 * time.Hour)},
		"from the future":          {[]byte("s3cr3t"), "github:1", good, tokenNow.Add(-time.Hour)},
		"another owner now":        {[]byte("s3cr3t"), "github:2", good, tokenNow},
		"after the secret changed": {[]byte("n3w"), "github:1", good, tokenNow},
		"another tunnel's":         {[]byte("s3cr3t"), "github:1", other, tokenNow},
		"tampered":                 {[]byte("s3cr3t"), "github:1", tampered, tokenNow},
		"garbage":                  {[]byte("s3cr3t"), "github:1", "nope", tokenNow},
		"no owner":                 {[]byte("s3cr3t"), "", good, tokenNow},
		"no secret":                {nil, "github:1", good, tokenNow},
	} {
		if err := verifyToken(tokenKey(tc.key), ssoHost, tc.owner, tc.tok, tc.at); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestSelfIssued(t *testing.T) {
	tok, _ := mintToken(tokenKey([]byte("s3cr3t")), ssoHost, "github:1", tokenNow, time.Hour)
	if !selfIssued(tok, ssoHost) {
		t.Error("own token not recognised")
	}
	if selfIssued(tok, "other.tunneled.pizza") || selfIssued("listed", ssoHost) || selfIssued("", ssoHost) {
		t.Error("recognised something else")
	}
}

func exchangeAuth(t *testing.T, o *oidcOf, owner string) *AuthImpl {
	t.Helper()
	a := New(WithSecret(func() []byte { return []byte("s3cr3t") }), WithOidc(o),
		WithAuthorizationServer(func() string { return fakeIssuer }))
	a.now = func() time.Time { return tokenNow }
	if err := a.Set(value); err != nil {
		t.Fatal(err)
	}
	return owned(a, owner)
}

func exchange(a *AuthImpl, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/_tunneld/token", strings.NewReader(form.Encode()))
	r.Host = ssoHost
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.token(rec, r)
	return rec
}

func exchangeForm(subject, expiresIn string) url.Values {
	return url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {subject},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"expires_in":         {expiresIn},
	}
}

func TestExchangeMints(t *testing.T) {
	for _, life := range []int{604800, 2592000, 5184000, 7776000, 0} {
		a := exchangeAuth(t, fakeOidc(), "github:1")
		rec := exchange(a, exchangeForm("subject", strconv.Itoa(life)))
		var body struct {
			AccessToken     string `json:"access_token"`
			IssuedTokenType string `json:"issued_token_type"`
			TokenType       string `json:"token_type"`
			ExpiresIn       *int   `json:"expires_in"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" ||
			body.TokenType != "Bearer" || body.IssuedTokenType != "urn:ietf:params:oauth:token-type:access_token" {
			t.Fatalf("%d: %d %s", life, rec.Code, rec.Body)
		}
		if (life == 0) != (body.ExpiresIn == nil) || (life != 0 && *body.ExpiresIn != life) {
			t.Errorf("%d: expires_in %v", life, body.ExpiresIn)
		}
		if err := verifyToken(tokenKey([]byte("s3cr3t")), ssoHost, "github:1", body.AccessToken, tokenNow); err != nil {
			t.Errorf("%d: minted token does not verify: %v", life, err)
		}
	}
}

func TestExchangeRefuses(t *testing.T) {
	base := exchangeForm("subject", "2592000")
	with := func(k, v string) url.Values {
		f := url.Values{}
		for key, vs := range base {
			f[key] = vs
		}
		if v == "" {
			f.Del(k)
		} else {
			f.Set(k, v)
		}
		return f
	}
	for name, tc := range map[string]struct {
		form  url.Values
		owner string
		down  bool
		code  int
		err   string
	}{
		"another grant":           {with("grant_type", "authorization_code"), "github:1", false, 400, "unsupported_grant_type"},
		"no subject token":        {with("subject_token", ""), "github:1", false, 400, "invalid_request"},
		"another token type":      {with("subject_token_type", "urn:ietf:params:oauth:token-type:id_token"), "github:1", false, 400, "invalid_request"},
		"expires_in missing":      {with("expires_in", ""), "github:1", false, 400, "invalid_request"},
		"expires_in not allowed":  {with("expires_in", "30"), "github:1", false, 400, "invalid_request"},
		"expires_in not a number": {with("expires_in", "forever"), "github:1", false, 400, "invalid_request"},
		"no owner":                {base, "", false, 400, "invalid_grant"},
		"not a provider token":    {with("subject_token", "nope"), "github:1", false, 400, "invalid_grant"},
		"an MCP client's token":   {with("subject_token", "mcp-client"), "github:1", false, 400, "invalid_grant"},
		"without tunnel:token":    {with("subject_token", "no-scope"), "github:1", false, 400, "invalid_grant"},
		"someone else's":          {with("subject_token", "subject-other"), "github:1", false, 400, "invalid_grant"},
		"provider down":           {base, "github:1", true, 503, "temporarily_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			o := fakeOidc()
			o.down = tc.down
			rec := exchange(exchangeAuth(t, o, tc.owner), tc.form)
			var body struct{ Error string }
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != tc.code || body.Error != tc.err || rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%d %q; want %d %q", rec.Code, body.Error, tc.code, tc.err)
			}
			if strings.Contains(rec.Body.String(), "access_token") {
				t.Error("refused, but a token went out")
			}
		})
	}
	t.Run("no secret yet", func(t *testing.T) {
		a := owned(New(WithOidc(fakeOidc()), WithAuthorizationServer(func() string { return fakeIssuer })), "github:1")
		rec := exchange(a, base)
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "2" {
			t.Errorf("%d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
		}
	})
	t.Run("GET", func(t *testing.T) {
		a := exchangeAuth(t, fakeOidc(), "github:1")
		r := httptest.NewRequest("GET", "/_tunneld/token", nil)
		rec := httptest.NewRecorder()
		a.token(rec, r)
		if rec.Code != 405 || rec.Header().Get("Allow") != "POST" {
			t.Errorf("%d Allow %q", rec.Code, rec.Header().Get("Allow"))
		}
	})
}

func TestExchangeLogsNoToken(t *testing.T) {
	var buf bytes.Buffer
	a := exchangeAuth(t, fakeOidc(), "github:1")
	WithLog(slog.New(slog.NewTextHandler(&buf, nil)))(a)
	rec := exchange(a, exchangeForm("subject", "604800"))
	var body struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	log := buf.String()
	if !strings.Contains(log, `msg="a token"`) || !strings.Contains(log, "ok=true") {
		t.Errorf("log %q", log)
	}
	if body.AccessToken == "" || strings.Contains(log, body.AccessToken) {
		t.Error("the minted token reached the log")
	}
}

func TestGateTakesItsOwnTokens(t *testing.T) {
	mintAt := func(secret string, at time.Time) string {
		tok, _ := mintToken(tokenKey([]byte(secret)), ssoHost, "github:1", at, time.Hour)
		return tok
	}
	mint := func(secret string) string { return mintAt(secret, time.Now()) }
	good := mint("s3cr3t")
	ask := func(a *AuthImpl, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/x", nil)
		r.Host = ssoHost
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		return serve(a.Handler(origin()), r)
	}
	both := value + ", " + ssoValue
	for name, v := range map[string]string{"Password": value, "Single Sign-On": ssoValue, "both": both} {
		t.Run(name, func(t *testing.T) {
			a := New(WithSecret(func() []byte { return []byte("s3cr3t") }), WithOidc(fakeOidc()))
			if err := a.Set(v); err != nil {
				t.Fatal(err)
			}
			owned(a, "github:1")
			rec := ask(a, good)
			if rec.Code != 200 || rec.Header().Get("X-Authorization") != "" {
				t.Errorf("%d, origin saw Authorization %q; want 200, stripped", rec.Code, rec.Header().Get("X-Authorization"))
			}
			for bad, tok := range map[string]string{
				"expired":                  mintAt("s3cr3t", time.Now().Add(-3*time.Hour)),
				"after the secret changed": mint("0ld"),
			} {
				if rec := ask(a, tok); rec.Code != 401 || rec.Header().Get("X-Authorization") != "" {
					t.Errorf("%s: %d", bad, rec.Code)
				}
			}
			owned(a, "github:2")
			if rec := ask(a, good); rec.Code != 401 {
				t.Errorf("after the owner changed: %d", rec.Code)
			}
		})
	}
	t.Run("a provider token on a Password tunnel is still ignored", func(t *testing.T) {
		a := owned(pwAuth(t, fakeOidc()), "github:1")
		r := httptest.NewRequest("GET", "/x", nil)
		r.Host = ssoHost
		r.Header.Set("Authorization", "Bearer listed")
		if rec := serve(a.Handler(origin()), r); rec.Code != 401 || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%d %q; want the Basic challenge", rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	})
	t.Run("a public tunnel passes everything, untouched", func(t *testing.T) {
		a := owned(New(WithSecret(func() []byte { return []byte("s3cr3t") })), "github:1")
		if rec := ask(a, good); rec.Code != 200 || rec.Header().Get("X-Authorization") != "Bearer "+good {
			t.Errorf("%d, origin saw %q", rec.Code, rec.Header().Get("X-Authorization"))
		}
	})
	t.Run("an ownerless tunnel takes none", func(t *testing.T) {
		a := protected(t)
		if rec := ask(a, good); rec.Code != 401 {
			t.Errorf("%d", rec.Code)
		}
	})
	t.Run("on Single Sign-On a bad one is invalid_token", func(t *testing.T) {
		a := owned(ssoAuth(t, fakeOidc()), "github:1")
		rec := ask(a, mint("0ld"))
		if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
			t.Errorf("%d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	})
}

// TestCookieHolderTunnelToken pins that a tunnel token never reaches the
// origin, a tunneld cookie beside it or not; an origin's own Bearer still does.
func TestCookieHolderTunnelToken(t *testing.T) {
	a := owned(protected(t), "github:1")
	h := a.Handler(origin())
	cookie := mintCookie(cookieKey([]byte("s3cr3t")), "basic", "", value, a.now())
	tok, _ := mintToken(tokenKey([]byte("s3cr3t")), ssoHost, "github:1", time.Now(), time.Hour)
	ask := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Host = ssoHost
		req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		req.Header.Set("Authorization", "Bearer "+bearer)
		return serve(h, req)
	}
	if rec := ask(tok); rec.Code != 200 || rec.Header().Get("X-Authorization") != "" {
		t.Errorf("the tunnel token reached the origin: %d %q", rec.Code, rec.Header().Get("X-Authorization"))
	}
	if rec := ask("the-apps-own"); rec.Code != 200 || rec.Header().Get("X-Authorization") != "Bearer the-apps-own" {
		t.Errorf("the origin's own Bearer: %d %q, want it intact", rec.Code, rec.Header().Get("X-Authorization"))
	}
}

// TestExchangeLogsOnlyVerifiedAsks pins that the exchange, which anyone can
// reach, writes a line only for a subject token the provider signed: a
// refusal before that would let anyone grow the log at request rate.
func TestExchangeLogsOnlyVerifiedAsks(t *testing.T) {
	for name, tc := range map[string]struct {
		form   url.Values
		logged bool
	}{
		"another grant":          {url.Values{"grant_type": {"password"}}, false},
		"expires_in not allowed": {exchangeForm("subject", "30"), false},
		"not a provider token":   {exchangeForm("nope", "604800"), false},
		"someone else's":         {exchangeForm("subject-other", "604800"), true},
		"minted":                 {exchangeForm("subject", "604800"), true},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			a := exchangeAuth(t, fakeOidc(), "github:1")
			WithLog(slog.New(slog.NewTextHandler(&buf, nil)))(a)
			exchange(a, tc.form)
			if got := strings.Contains(buf.String(), `msg="a token"`); got != tc.logged {
				t.Errorf("logged %v, want %v: %q", got, tc.logged, buf.String())
			}
		})
	}
}
