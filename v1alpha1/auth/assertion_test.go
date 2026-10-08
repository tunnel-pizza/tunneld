package auth

import (
	"bytes"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// TestAssertionKey pins the client key: the same secret always derives the
// same key, and another secret another.
func TestAssertionKey(t *testing.T) {
	a := assertionKey([]byte("secret-one"))
	if !bytes.Equal(a, assertionKey([]byte("secret-one"))) {
		t.Fatal("assertionKey differs for the same secret")
	}
	if bytes.Equal(a, assertionKey([]byte("secret-two"))) {
		t.Fatal("assertionKey is the same for different secrets")
	}
}

// TestClientAssertion pins RFC 7523 §2.2's assertion: EdDSA under the
// published key, its kid, iss and sub the client, aud the issuer, a jti, one
// minute.
func TestClientAssertion(t *testing.T) {
	key := assertionKey([]byte("secret-one"))
	jwk := assertionJWK(key)
	now := time.Unix(1_800_000_000, 0)
	signed, err := clientAssertion(key, testClient, fakeIssuer, now)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.ParseSigned(signed, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Headers[0].KeyID != jwk.KeyID || jwk.KeyID == "" {
		t.Fatalf("kid = %q, want %q", tok.Headers[0].KeyID, jwk.KeyID)
	}
	var c jwt.Claims
	if err := tok.Claims(jwk.Key, &c); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateWithLeeway(jwt.Expected{Issuer: testClient, Subject: testClient, AnyAudience: jwt.Audience{fakeIssuer}, Time: now}, 0); err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || c.Expiry.Time().Sub(now) != time.Minute {
		t.Fatalf("jti %q, exp in %v; want a jti and one minute", c.ID, c.Expiry.Time().Sub(now))
	}
	again, _ := clientAssertion(key, testClient, fakeIssuer, now)
	if again == signed {
		t.Error("two assertions are the same: the jti must differ")
	}
}
