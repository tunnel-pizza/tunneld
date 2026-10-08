package auth

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// tokenInfo labels the HKDF that turns the tunnel secret into the key its
// tokens are signed with; changing it ends every token every tunnel minted.
const tokenInfo = "tunneld token key"

// tokenSkew is how far iat and exp may be off from this machine's clock.
const tokenSkew = 60 * time.Second

var errToken = errors.New("auth: not a token this tunnel minted for its owner")

// tokenKey is the key this tunnel signs and checks its own tokens with,
// derived from its secret, so a new secret (Reset Tunnel) ends them all.
// Nil when there is no secret yet.
func tokenKey(secret []byte) ed25519.PrivateKey {
	if len(secret) == 0 {
		return nil
	}
	seed, _ := hkdf.Key(sha256.New, secret, nil, tokenInfo, ed25519.SeedSize)
	return ed25519.NewKeyFromSeed(seed)
}

type tokenClaims struct {
	jwt.Claims
	ClientID string `json:"client_id"`
}

// mintToken is an RFC 9068 access token for sub on host, signed with key,
// for lifetime; 0 has no exp.
func mintToken(key ed25519.PrivateKey, host, sub string, now time.Time, lifetime time.Duration) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key}, (&jose.SignerOptions{}).WithType("at+jwt"))
	if err != nil {
		return "", err
	}
	self := "https://" + host
	c := tokenClaims{
		Claims: jwt.Claims{
			Issuer:   self,
			Audience: jwt.Audience{self},
			Subject:  sub,
			IssuedAt: jwt.NewNumericDate(now),
			ID:       random(16),
		},
		ClientID: self + v1.ControlPath + "client.json",
	}
	if lifetime > 0 {
		c.Expiry = jwt.NewNumericDate(now.Add(lifetime))
	}
	return jwt.Signed(signer).Claims(c).Serialize()
}

// verifyToken is nil when token is one key signed for host's owner: EdDSA,
// at+jwt, iss and aud host, sub owner, iat present and not ahead, exp (when
// present) not past.
func verifyToken(key ed25519.PrivateKey, host, owner, token string, now time.Time) error {
	if key == nil || owner == "" {
		return errToken
	}
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(tok.Headers) != 1 {
		return errToken
	}
	if typ, _ := tok.Headers[0].ExtraHeaders[jose.HeaderType].(string); typ != "at+jwt" {
		return errToken
	}
	var c tokenClaims
	if err := tok.Claims(key.Public(), &c); err != nil {
		return errToken
	}
	self := "https://" + host
	if c.Subject != owner || c.IssuedAt == nil || c.IssuedAt.Time().After(now.Add(tokenSkew)) {
		return errToken
	}
	if err := c.ValidateWithLeeway(jwt.Expected{Issuer: self, AnyAudience: jwt.Audience{self}, Time: now}, tokenSkew); err != nil {
		return errToken
	}
	return nil
}

// selfIssued is whether token says, unverified, that host issued it: which
// rule at the gate judges it, never whether it passes.
func selfIssued(token, host string) bool {
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return false
	}
	var c jwt.Claims
	return tok.UnsafeClaimsWithoutVerification(&c) == nil && c.Issuer == "https://"+host
}
