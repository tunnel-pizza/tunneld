package auth

import (
	"cmp"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth/oidc"
)

// flowCookie carries one sign-in from the login page to the callback: the
// state the provider echoes, the nonce the ID token must carry, the PKCE
// verifier and where to land. Sealed, so only this tunnel can read or make
// one. __Host-, so it is host-only on Path=/: the shared domain is not a
// public suffix, and a sibling tunnel could otherwise set one ahead of it.
const flowCookie = "__Host-tunneld-oauth"

// flowCookieFor is the flow cookie of the sign-in whose state is state: one
// per sign-in, since a browser keeps one cookie per name and two tabs may be
// signing in at once. "" for a state too short to have been ours.
func flowCookieFor(state string) string {
	if len(state) < 16 {
		return ""
	}
	return flowCookie + "-" + state[:16]
}

// flowLife is how long a sign-in may take.
const flowLife = 10 * time.Minute

type flow struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
	Exp      int64  `json:"exp"`
}

// flowKey derives the sealing key from the tunnel secret under its own
// label, as cookieKey does. Nil when there is no secret yet.
func flowKey(secret []byte) []byte {
	if len(secret) == 0 {
		return nil
	}
	key, err := hkdf.Key(sha256.New, secret, nil, "tunneld/auth/oauth", 32)
	if err != nil {
		return nil
	}
	return key
}

// seal is f under key, AES-256-GCM, as base64url(nonce || ciphertext).
func seal(key []byte, f flow) (string, error) {
	gcm, err := gcmOf(key)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, body, []byte(flowCookie))), nil
}

// unseal is the flow key sealed as token; false for anything else.
func unseal(key []byte, token string) (flow, bool) {
	gcm, err := gcmOf(key)
	if err != nil {
		return flow{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) < gcm.NonceSize() {
		return flow{}, false
	}
	body, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte(flowCookie))
	var f flow
	if err != nil || json.Unmarshal(body, &f) != nil {
		return flow{}, false
	}
	return f, true
}

func gcmOf(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("auth: no sealing key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// random is n random bytes, base64url.
func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// clientID is this tunnel's client at the provider: the URL of the Client ID
// Metadata Document it serves itself, so only it can claim its name.
func clientID(host string) string { return "https://" + host + v1.ControlPath + "client.json" }

// redirectURI is where the provider sends a signed-in browser back to.
func redirectURI(host string) string { return "https://" + host + v1.ControlPath + "callback" }

// signInURL is the login page's link that starts a sign-in, landing at next.
func signInURL(next, prompt string) string {
	q := url.Values{"sso": {"1"}, "next": {next}}
	if prompt != "" {
		q.Set("prompt", prompt)
	}
	return v1.ControlPath + "login?" + q.Encode()
}

// signIn sends the browser to the provider to sign in (OIDC Core §3.1.2.1):
// the code flow with S256 PKCE (RFC 7636) as this tunnel's own client, its
// hostname as the resource (RFC 8707), a fresh state and nonce in the sealed
// flow cookie. prompt is passed on only as "login".
func (a *AuthImpl) signIn(w http.ResponseWriter, r *http.Request, s *state, next, prompt string) {
	unreachable := page{Next: next, Password: s.basic, SSO: true, Error: "tunnel.pizza couldn't be reached. Try again."}
	key := flowKey(a.secret())
	if key == nil {
		w.Header().Set("Retry-After", "2")
		a.render(w, r, http.StatusServiceUnavailable, page{Next: next, Password: s.basic, SSO: true, Error: "This tunnel is still starting. Try again in a moment."})
		return
	}
	d, err := a.oidc.Endpoints(r.Context())
	if err != nil {
		a.log.Info("a login", "via", "sso", "ok", false, "reason", "the provider could not be reached")
		a.render(w, r, http.StatusServiceUnavailable, unreachable)
		return
	}
	authorize, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		a.render(w, r, http.StatusServiceUnavailable, unreachable)
		return
	}
	f := flow{State: random(32), Nonce: random(32), Verifier: random(32), Next: next, Exp: a.now().Add(flowLife).Unix()}
	sealed, err := seal(key, f)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: flowCookieFor(f.State), Value: sealed, Path: "/", MaxAge: int(flowLife / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	sum := sha256.Sum256([]byte(f.Verifier))
	q := authorize.Query()
	for k, v := range map[string]string{
		"response_type": "code", "client_id": clientID(r.Host), "redirect_uri": redirectURI(r.Host),
		"scope": "openid profile", "resource": "https://" + r.Host, "state": f.State, "nonce": f.Nonce,
		"code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]), "code_challenge_method": "S256",
	} {
		q.Set(k, v)
	}
	if prompt == "login" {
		q.Set("prompt", "login")
	}
	authorize.RawQuery = q.Encode()
	http.Redirect(w, r, authorize.String(), http.StatusSeeOther)
}

// client answers this tunnel's Client ID Metadata Document: its own URL as
// client_id, its hostname as its name, one callback, a public client.
func (a *AuthImpl) client(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id":                  clientID(r.Host),
		"client_name":                r.Host,
		"redirect_uris":              []string{redirectURI(r.Host)},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"scope":                      "openid profile",
	})
}

// callback finishes a sign-in (OIDC Core §3.1.2.5): the sealed flow,
// unexpired, and its state; iss the provider's issuer (RFC 9207 §2.4); the
// code traded and the ID token checked. A listed sub gets the auth cookie
// and lands at the flow's next; an unlisted one a 403 naming who they signed
// in as.
func (a *AuthImpl) callback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s := a.state.Load()
	q := r.URL.Query()
	var f flow
	ok := false
	if name := flowCookieFor(q.Get("state")); name != "" {
		if c, err := r.Cookie(name); err == nil {
			f, ok = unseal(flowKey(a.secret()), c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
	next := cmp.Or(f.Next, "/")
	if !s.bearer {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	fail := func(status int, msg string) {
		a.log.Info("a login", "via", "sso", "ok", false)
		a.render(w, r, status, page{Next: next, Password: s.basic, SSO: true, Error: msg})
	}
	switch {
	case !ok || a.now().Unix() >= f.Exp:
		fail(http.StatusOK, "Sign-in failed. Try again.")
		return
	case q.Get("error") == "access_denied":
		fail(http.StatusOK, "Sign-in was cancelled.")
		return
	case q.Get("error") != "", subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1:
		fail(http.StatusOK, "Sign-in failed. Try again.")
		return
	}
	d, err := a.oidc.Endpoints(r.Context())
	if err != nil {
		fail(http.StatusServiceUnavailable, "tunnel.pizza couldn't be reached. Try again.")
		return
	}
	if q.Get("iss") != d.Issuer {
		fail(http.StatusOK, "Sign-in failed. Try again.")
		return
	}
	token, err := a.oidc.Exchange(r.Context(), q.Get("code"), f.Verifier, redirectURI(r.Host), clientID(r.Host))
	var who oidc.IDClaims
	if err == nil {
		who, err = a.oidc.VerifyID(r.Context(), token, clientID(r.Host), f.Nonce)
	}
	switch {
	case errors.Is(err, ErrUnavailable):
		fail(http.StatusServiceUnavailable, "tunnel.pizza couldn't be reached. Try again.")
		return
	case err != nil:
		fail(http.StatusOK, "Sign-in failed. Try again.")
		return
	}
	if !slices.Contains(a.listed(s), who.Subject) {
		a.log.Info("a login", "via", "sso", "ok", false, "listed", false)
		a.render(w, r, http.StatusForbidden, page{
			Next: next, Password: s.basic, SSO: true, SignIn: signInURL(next, "login"), Label: "Use another account",
			Error: "Signed in as " + cmp.Or(who.PreferredUsername, who.Subject) + ", which isn't allowed on " + r.Host + ".",
		})
		return
	}
	key := cookieKey(a.secret())
	if key == nil {
		fail(http.StatusServiceUnavailable, "This tunnel is still starting. Try again in a moment.")
		return
	}
	a.setCookie(w, key, s.value, "bearer", who.Subject)
	a.log.Info("a login", "via", "sso", "ok", true)
	http.Redirect(w, r, next, http.StatusSeeOther)
}
