// Package oidc is how auth asks the provider about a token: its discovery
// document (OIDC Discovery 1.0, RFC 8414), its signing keys, RFC 9068 access
// tokens, and a sign-in's code and ID token (OIDC Core 1.0). It is auth's
// alone: v1alpha1 does not alias Oidc, and nothing outside auth takes one.
package oidc

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// What a token or an issuer did wrong, as errors.Is reads it.
var (
	// ErrInvalidToken is a token that does not verify here: RFC 6750's
	// invalid_token.
	ErrInvalidToken = errors.New("oidc: the token is not valid here")
	// ErrUnavailable is an issuer that could not be reached, or answered
	// with something that is not its metadata: nobody gets in, and a client
	// is told to come back rather than to sign in again.
	ErrUnavailable = errors.New("oidc: the issuer could not be reached")
	// ErrRefused is a token endpoint that answered and would not trade the
	// code.
	ErrRefused = errors.New("oidc: the issuer refused the code")
)

const (
	// clockSkew is how far exp and iat may be off from this machine's clock.
	clockSkew = 60 * time.Second
	// minCache and maxCache bound how long an issuer's answer is kept.
	minCache = 5 * time.Minute
	maxCache = 24 * time.Hour
	// refetchGap is how often a kid not in the set may fetch the set again.
	refetchGap = time.Minute
	// retryGap is how long a failed fetch stays the answer: an issuer that is
	// down costs one fetch per gap, not one per request.
	retryGap = 5 * time.Second
	// maxDocument bounds an issuer's answer.
	maxDocument = 64 << 10
)

// Oidc is what auth asks of the provider: where it signs people in, whether
// a token it was sent is one the provider issued, and a sign-in's code
// traded for who signed in.
type Oidc interface {
	Endpoints(ctx context.Context) (Discovery, error)
	VerifyAccess(ctx context.Context, token, resource string) (sub string, err error)
	VerifyAccessClaims(ctx context.Context, token, resource string) (AccessClaims, error)
	Exchange(ctx context.Context, code, verifier, redirectURI, clientID, assertion string) (idToken string, err error)
	VerifyID(ctx context.Context, token, clientID, nonce string) (IDClaims, error)
}

// Option configures an OidcImpl.
type Option = v1.Option[*OidcImpl]

// Discovery is what auth reads of the issuer's metadata (OIDC Discovery 1.0
// §3, RFC 8414 §2).
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// OidcImpl is the default Oidc: it asks the issuer over HTTP, keeping its
// metadata and keys for as long as it said, within minCache and maxCache,
// and forgetting both when the issuer changes.
type OidcImpl struct {
	issuer func() string
	http   *http.Client
	now    func() time.Time
	log    v1.Logger

	mu        sync.Mutex
	cached    string
	meta      *Discovery
	metaUntil time.Time
	keys      jose.JSONWebKeySet
	keysUntil time.Time
	keysAt    time.Time
	// fetching is closed when the fetch under way ends; nil when none is.
	fetching chan struct{}
	failedAt time.Time
	failure  error
}

var _ Oidc = (*OidcImpl)(nil)

// New returns an OidcImpl asking tunnel.pizza, with a 10 s client, the wall
// clock and a logger that discards.
func New(opts ...Option) *OidcImpl {
	return v1.Apply(&OidcImpl{
		issuer: func() string { return "https://" + v1.DefaultProvider },
		http:   &http.Client{Timeout: 10 * time.Second},
		now:    time.Now,
		log:    slog.New(slog.DiscardHandler),
	}, opts...)
}

// WithIssuer is where the issuer is read from, on every ask: the provider's
// origin, which is only settled once the command has read its flags. Nil
// keeps the one it has.
func WithIssuer(issuer func() string) Option {
	return func(o *OidcImpl) {
		if issuer != nil {
			o.issuer = issuer
		}
	}
}

// WithHTTPClient sets the client the issuer is asked with. Nil keeps the one
// it has.
func WithHTTPClient(c *http.Client) Option {
	return func(o *OidcImpl) {
		if c != nil {
			o.http = c
		}
	}
}

// WithClock sets what now is, for expiry and the caches. Nil keeps the one
// it has.
func WithClock(now func() time.Time) Option {
	return func(o *OidcImpl) {
		if now != nil {
			o.now = now
		}
	}
}

// WithLog sets where oidc says what it fetched. Nil keeps the one it has.
func WithLog(log v1.Logger) Option {
	return func(o *OidcImpl) {
		if log != nil {
			o.log = log
		}
	}
}

// cacheFor is how long an answer may be kept: its Cache-Control max-age,
// within minCache and maxCache; minCache when it gives none.
func cacheFor(h http.Header) time.Duration {
	for _, d := range strings.Split(h.Get("Cache-Control"), ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(d), "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				return min(max(time.Duration(n)*time.Second, minCache), maxCache)
			}
		}
	}
	return minCache
}

// get fetches u as JSON into dest and says how long it may be kept. Any
// failure to ask or to read is ErrUnavailable.
func (o *OidcImpl) get(ctx context.Context, u string, dest any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: %s answered %d", ErrUnavailable, u, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(dest); err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrUnavailable, u, err)
	}
	return cacheFor(resp.Header), nil
}

// discover is issuer's metadata, refused unless it names issuer exactly
// (RFC 8414 §3.3) and the three endpoints used here.
func (o *OidcImpl) discover(ctx context.Context, issuer string) (*Discovery, time.Duration, error) {
	var d Discovery
	ttl, err := o.get(ctx, issuer+"/.well-known/openid-configuration", &d)
	if err != nil {
		return nil, 0, err
	}
	if d.Issuer != issuer || d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, 0, fmt.Errorf("%w: %s's metadata names issuer %q", ErrUnavailable, issuer, d.Issuer)
	}
	o.log.Debug("the issuer's metadata", "issuer", issuer, "for", ttl)
	return &d, ttl, nil
}

// fresh forgets everything kept when the issuer is not the one it was kept
// for. Called with mu held.
func (o *OidcImpl) fresh(issuer string) {
	if o.cached != issuer {
		o.cached, o.meta, o.keys = issuer, nil, jose.JSONWebKeySet{}
		o.metaUntil, o.keysUntil, o.keysAt, o.failedAt, o.failure = time.Time{}, time.Time{}, time.Time{}, time.Time{}, nil
	}
}

// fetch has the issuer asked for its metadata, when it is stale, and its
// keys, when keys, then returns for the caller to look again. One fetch runs
// at a time, outside mu, shared by everyone waiting on it and not ended by
// any of them hanging up; a failure is the answer until retryGap has passed.
// Called with mu held; returns with it held.
func (o *OidcImpl) fetch(ctx context.Context, issuer string, keys bool) error {
	if o.fetching == nil {
		now := o.now()
		if o.failure != nil && now.Sub(o.failedAt) < retryGap {
			return o.failure
		}
		meta := o.meta
		if !now.Before(o.metaUntil) {
			meta = nil
		}
		o.fetching = make(chan struct{})
		go o.run(context.WithoutCancel(ctx), issuer, meta, keys, o.fetching)
	}
	done := o.fetching
	o.mu.Unlock()
	defer o.mu.Lock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
	}
}

// run is one fetch: the metadata when meta is nil, then the keys when keys.
// What it learned is kept only if the issuer is still the one asked.
func (o *OidcImpl) run(ctx context.Context, issuer string, meta *Discovery, keys bool, done chan struct{}) {
	var (
		metaTTL, keysTTL time.Duration
		set              jose.JSONWebKeySet
		err              error
	)
	fetched := meta == nil
	if fetched {
		meta, metaTTL, err = o.discover(ctx, issuer)
	}
	if err == nil && keys {
		keysTTL, err = o.get(ctx, meta.JWKSURI, &set)
		if err == nil {
			o.log.Debug("the issuer's keys", "issuer", issuer, "keys", len(set.Keys), "for", keysTTL)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fetching = nil
	defer close(done)
	if o.cached != issuer {
		return
	}
	now := o.now()
	if fetched && meta != nil {
		o.meta, o.metaUntil = meta, now.Add(metaTTL)
	}
	if err != nil {
		o.failedAt, o.failure = now, err
		return
	}
	o.failedAt, o.failure = time.Time{}, nil
	if keys {
		o.keys, o.keysUntil, o.keysAt = set, now.Add(keysTTL), now
	}
}

// Endpoints is the issuer's metadata, as the sign-in needs it.
func (o *OidcImpl) Endpoints(ctx context.Context) (Discovery, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		issuer := o.issuer()
		o.fresh(issuer)
		if o.meta != nil && o.now().Before(o.metaUntil) {
			return *o.meta, nil
		}
		if err := o.fetch(ctx, issuer, false); err != nil {
			return Discovery{}, err
		}
	}
}

// key is the issuer's RSA signing key kid. The set is fetched again once it
// is past its max-age, or when kid is not in it and the last fetch is a
// minute old or more: a rotation needs no restart, and a stream of made-up
// kids cannot make every request a fetch.
func (o *OidcImpl) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		issuer := o.issuer()
		o.fresh(issuer)
		now := o.now()
		found := o.keys.Key(kid)
		metaOK := o.meta != nil && now.Before(o.metaUntil)
		keysOK := now.Before(o.keysUntil) && (len(found) > 0 || now.Sub(o.keysAt) < refetchGap)
		if metaOK && keysOK {
			for _, k := range found {
				if pub, ok := k.Key.(*rsa.PublicKey); ok && (k.Use == "" || k.Use == "sig") {
					return pub, nil
				}
			}
			return nil, fmt.Errorf("%w: no RSA signing key %q", ErrInvalidToken, kid)
		}
		if err := o.fetch(ctx, issuer, !keysOK); err != nil {
			return nil, err
		}
	}
}

// verify checks token is a single RS256 JWS under one of the issuer's keys
// and reads its claims into dest. access says which kind it must be: an
// access token's typ is at+jwt (RFC 9068 §2.1) and nothing else's may be, so
// neither can stand in for the other.
func (o *OidcImpl) verify(ctx context.Context, token string, access bool, dest any) error {
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(tok.Headers) != 1 {
		return fmt.Errorf("%w: not an RS256 JWS", ErrInvalidToken)
	}
	h := tok.Headers[0]
	typ, _ := h.ExtraHeaders[jose.HeaderType].(string)
	if at := strings.EqualFold(typ, "at+jwt") || strings.EqualFold(typ, "application/at+jwt"); at != access {
		return fmt.Errorf("%w: typ %q", ErrInvalidToken, typ)
	}
	key, err := o.key(ctx, h.KeyID)
	if err != nil {
		return err
	}
	if err := tok.Claims(key, dest); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return nil
}

// accessClaims is what is read of an RFC 9068 access token.
type accessClaims struct {
	jwt.Claims
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
}

// AccessClaims is what a caller reads of an access token VerifyAccessClaims
// accepted: who, which client it was issued to, and its scope.
type AccessClaims struct {
	Subject, ClientID, Scope string
}

// VerifyAccess is the subject of token when it is an access token the issuer
// signed for resource (RFC 9068 §4): iss the issuer, aud holding resource,
// sub, exp and iat present, exp and iat within clockSkew.
func (o *OidcImpl) VerifyAccess(ctx context.Context, token, resource string) (string, error) {
	c, err := o.VerifyAccessClaims(ctx, token, resource)
	return c.Subject, err
}

// VerifyAccessClaims is VerifyAccess, with the token's client_id and scope.
func (o *OidcImpl) VerifyAccessClaims(ctx context.Context, token, resource string) (AccessClaims, error) {
	var c accessClaims
	if err := o.verify(ctx, token, true, &c); err != nil {
		return AccessClaims{}, err
	}
	if err := checkClaims(c.Claims, o.issuer(), resource, o.now()); err != nil {
		return AccessClaims{}, err
	}
	return AccessClaims{Subject: c.Subject, ClientID: c.ClientID, Scope: c.Scope}, nil
}

// checkClaims is what every token here must carry: iss the issuer, aud
// holding audience, sub, and exp and iat, neither more than clockSkew wrong.
func checkClaims(c jwt.Claims, issuer, audience string, now time.Time) error {
	if c.Subject == "" || c.Expiry == nil || c.IssuedAt == nil {
		return fmt.Errorf("%w: sub, exp and iat are required", ErrInvalidToken)
	}
	if err := c.ValidateWithLeeway(jwt.Expected{Issuer: issuer, AnyAudience: jwt.Audience{audience}, Time: now}, clockSkew); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if c.IssuedAt.Time().After(now.Add(clockSkew)) {
		return fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	return nil
}

// Exchange trades code for an ID token at the issuer's token endpoint, as
// clientID with PKCE's verifier (RFC 6749 §4.1.3, RFC 7636 §4.5), signed with
// assertion when there is one (RFC 7523 §2.2). An issuer that would not trade, a 4xx naming an error (RFC 6749
// §5.2), is ErrRefused; anything else that is not an ID token (a 5xx, a rate
// limit, a page from something in front of the endpoint) is ErrUnavailable.
func (o *OidcImpl) Exchange(ctx context.Context, code, verifier, redirectURI, clientID, assertion string) (string, error) {
	d, err := o.Endpoints(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	if assertion != "" {
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", assertion)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("%w: the token endpoint answered %d", ErrUnavailable, resp.StatusCode)
	}
	var body struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	decoded := json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(&body) == nil
	switch {
	case resp.StatusCode == http.StatusOK && body.IDToken != "":
		return body.IDToken, nil
	case resp.StatusCode == http.StatusOK:
		return "", fmt.Errorf("%w: no id_token", ErrRefused)
	case !decoded || body.Error == "":
		return "", fmt.Errorf("%w: the token endpoint answered %d, naming no error", ErrUnavailable, resp.StatusCode)
	}
	return "", fmt.Errorf("%w: %d %s", ErrRefused, resp.StatusCode, body.Error)
}

// IDClaims is who an ID token says signed in.
type IDClaims struct {
	Subject           string
	PreferredUsername string
}

// idClaims is what is read of an ID token (OIDC Core §2).
type idClaims struct {
	jwt.Claims
	Nonce             string `json:"nonce"`
	AuthorizedParty   string `json:"azp"`
	PreferredUsername string `json:"preferred_username"`
}

// VerifyID is who token says signed in, when it is an ID token the issuer
// signed for clientID carrying nonce (OIDC Core §3.1.3.7): RS256 under a
// published key, typ not at+jwt, iss the issuer, aud holding clientID, azp
// naming it when aud holds others or azp is there at all, sub, exp and iat
// within clockSkew.
func (o *OidcImpl) VerifyID(ctx context.Context, token, clientID, nonce string) (IDClaims, error) {
	var c idClaims
	if err := o.verify(ctx, token, false, &c); err != nil {
		return IDClaims{}, err
	}
	if err := checkClaims(c.Claims, o.issuer(), clientID, o.now()); err != nil {
		return IDClaims{}, err
	}
	if (len(c.Audience) > 1 || c.AuthorizedParty != "") && c.AuthorizedParty != clientID {
		return IDClaims{}, fmt.Errorf("%w: azp %q", ErrInvalidToken, c.AuthorizedParty)
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1 {
		return IDClaims{}, fmt.Errorf("%w: the nonce is not this sign-in's", ErrInvalidToken)
	}
	return IDClaims{Subject: c.Subject, PreferredUsername: c.PreferredUsername}, nil
}
