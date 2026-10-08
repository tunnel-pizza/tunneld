package auth

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// assertionInfo labels the HKDF that turns the tunnel secret into its client
// key; changing it changes every tunnel's key.
const assertionInfo = "tunneld client assertion key"

// assertionKey is this tunnel's client key at the provider, derived from its
// secret: whoever holds the spec can sign as the tunnel, as they can already
// act as it.
func assertionKey(secret []byte) ed25519.PrivateKey {
	seed, _ := hkdf.Key(sha256.New, secret, nil, assertionInfo, ed25519.SeedSize)
	return ed25519.NewKeyFromSeed(seed)
}

// assertionJWK is the key's public half as client.json publishes it, kid its
// RFC 7638 thumbprint.
func assertionJWK(key ed25519.PrivateKey) jose.JSONWebKey {
	jwk := jose.JSONWebKey{Key: key.Public(), Algorithm: string(jose.EdDSA), Use: "sig"}
	thumb, _ := jwk.Thumbprint(crypto.SHA256)
	jwk.KeyID = base64.RawURLEncoding.EncodeToString(thumb)
	return jwk
}

// clientAssertion is an RFC 7523 §2.2 assertion for clientID at issuer: one
// minute and a fresh jti, so the provider can refuse a replay.
func clientAssertion(key ed25519.PrivateKey, clientID, issuer string, now time.Time) (string, error) {
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", assertionJWK(key).KeyID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key}, opts)
	if err != nil {
		return "", err
	}
	return jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   clientID,
		Subject:  clientID,
		Audience: jwt.Audience{issuer},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(time.Minute)),
		ID:       random(16),
	}).Serialize()
}
