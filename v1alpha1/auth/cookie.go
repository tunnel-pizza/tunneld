package auth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// CookieName is the cookie a browser that logged in carries. __Host-, so it
// is host-only on Path=/: the shared domain is not a public suffix, and a
// sibling tunnel could otherwise set one that shadows it.
const CookieName = "__Host-tunneld-auth"

// cookieLife is how long a login lasts: Max-Age and the payload's exp.
const cookieLife = 30 * 24 * time.Hour

// cookieKey derives the cookie's key from the tunnel secret under its own
// label, never the secret itself: the secret is already the control path's
// token, and one key for two jobs lets an answer meant for one be replayed as
// the other. Nil when there is no secret yet.
func cookieKey(secret []byte) []byte {
	if len(secret) == 0 {
		return nil
	}
	key, err := hkdf.Key(sha256.New, secret, nil, "tunneld/auth/cookie", 32)
	if err != nil {
		return nil
	}
	return key
}

// payload is what a cookie says: the scheme that let the visitor in, who
// (for a scheme that names someone: Bearer), when, and until when.
type payload struct {
	S   string `json:"s"`
	Sub string `json:"sub,omitempty"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// mintCookie is v1.<payload>.<mac>, both base64url. sub is "" for Basic.
func mintCookie(key []byte, scheme, sub, value string, now time.Time) string {
	body, _ := json.Marshal(payload{S: scheme, Sub: sub, Iat: now.Unix(), Exp: now.Add(cookieLife).Unix()})
	p := base64.RawURLEncoding.EncodeToString(body)
	return "v1." + p + "." + base64.RawURLEncoding.EncodeToString(mac(key, p, value))
}

// mac covers the payload and the current value, so changing the password, the
// tier, or the tunnel (the key) signs every visitor out.
func mac(key []byte, p, value string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("v1." + p + "\n" + value))
	return m.Sum(nil)
}

// readCookie is the sub of token when it is a valid cookie for value: version
// v1, MAC matching in constant time, not expired, naming a scheme the value
// still has and, for Bearer, a sub it still lists. ok is false for anything
// else, never an error.
func readCookie(key []byte, token, value string, schemes, subs []string, now time.Time) (sub string, ok bool) {
	if key == nil {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", false
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, mac(key, parts[1], value)) {
		return "", false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	var p payload
	if err != nil || json.Unmarshal(body, &p) != nil {
		return "", false
	}
	if now.Unix() >= p.Exp || !slices.Contains(schemes, p.S) {
		return "", false
	}
	if p.S == "bearer" && !slices.Contains(subs, p.Sub) {
		return "", false
	}
	return p.Sub, true
}
