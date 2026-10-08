package auth

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
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

var errLifetime = errors.New("auth: a token's lifetime cannot be negative")

// mintToken is an RFC 9068 access token for sub on host, signed with key,
// for lifetime; 0 has no exp, and a negative one is refused.
func mintToken(key ed25519.PrivateKey, host, sub string, now time.Time, lifetime time.Duration) (string, error) {
	if lifetime < 0 {
		return "", errLifetime
	}
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

// ownToken is whether r's Bearer is a token this tunnel minted for its owner
// and that still verifies: it lets r in, and never reaches the origin.
func (a *AuthImpl) ownToken(r *http.Request) bool {
	token, sent := bearerToken(r)
	return sent && selfIssued(token, r.Host) &&
		verifyToken(tokenKey(a.secret()), r.Host, a.owner(), token, a.now()) == nil
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

const (
	exchangeGrant   = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessTokenType = "urn:ietf:params:oauth:token-type:access_token"
	tokenScope      = "tunnel:token"
)

// tokenLifetimes is what expires_in may ask, in seconds; 0 is no expiry.
var tokenLifetimes = []int{604800, 2592000, 5184000, 7776000, 0}

// tokenError is an RFC 6749 §5.2 answer.
func tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

// token is RFC 8693 token exchange for the owner: the provider's subject
// token (scope tunnel:token, issued to the provider itself, for this
// tunnel, naming its owner) traded for a token this tunnel signs, for one
// of tokenLifetimes. Nothing is kept; the subject token is checked offline.
func (a *AuthImpl) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Logged only once the provider's signature verified: anyone can reach
	// this, and a line per refusal would let them grow the log at will.
	verified, ok, reason := false, false, ""
	defer func() {
		if verified {
			a.log.Info("a token", "ok", ok, "reason", reason)
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		reason = "form"
		tokenError(w, http.StatusBadRequest, "invalid_request", "a form body is required")
		return
	}
	f := r.PostForm
	if f.Get("grant_type") != exchangeGrant {
		reason = "grant_type"
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "only token exchange is offered")
		return
	}
	if f.Get("subject_token") == "" || f.Get("subject_token_type") != accessTokenType {
		reason = "subject_token"
		tokenError(w, http.StatusBadRequest, "invalid_request", "an access token is the subject token")
		return
	}
	life, err := strconv.Atoi(f.Get("expires_in"))
	if err != nil || !slices.Contains(tokenLifetimes, life) {
		reason = "expires_in"
		tokenError(w, http.StatusBadRequest, "invalid_request", "expires_in must be 604800, 2592000, 5184000, 7776000 or 0")
		return
	}
	key := tokenKey(a.secret())
	if key == nil {
		reason = "starting"
		w.Header().Set("Retry-After", "2")
		tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "this tunnel is still starting")
		return
	}
	owner := a.owner()
	if owner == "" {
		reason = "owner"
		tokenError(w, http.StatusBadRequest, "invalid_grant", "this tunnel has no owner")
		return
	}
	c, err := a.oidc.VerifyAccessClaims(r.Context(), f.Get("subject_token"), "https://"+r.Host)
	if errors.Is(err, ErrUnavailable) {
		reason = "provider"
		w.Header().Set("Retry-After", "30")
		tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the provider could not be reached")
		return
	}
	verified = err == nil
	if err != nil || c.ClientID != a.server() || !slices.Contains(strings.Fields(c.Scope), tokenScope) || c.Subject != owner {
		reason = "subject"
		tokenError(w, http.StatusBadRequest, "invalid_grant", "not the owner's subject token for this tunnel")
		return
	}
	tok, err := mintToken(key, r.Host, owner, a.now(), time.Duration(life)*time.Second)
	if err != nil {
		reason = "sign"
		tokenError(w, http.StatusInternalServerError, "server_error", "the token could not be signed")
		return
	}
	ok = true
	body := map[string]any{"access_token": tok, "issued_token_type": accessTokenType, "token_type": "Bearer"}
	if life > 0 {
		body["expires_in"] = life
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
