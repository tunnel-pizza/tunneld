package auth

import (
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
