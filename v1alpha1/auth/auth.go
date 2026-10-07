package auth

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth/oidc"
)

// loginHTML is the page a browser gets in place of the browser's own
// credentials dialog. Embedded, like the router's pages, so a tunnel serves it
// with nothing installed and no outbound request.
//
//go:embed login.html
var loginHTML string

// loginTmpl is parsed once: a page that fails to parse is a mistake in a
// file that ships inside the binary, not a visitor's problem.
var loginTmpl = template.Must(template.New("login").Parse(loginHTML))

// Auth stands between a visitor and everything the tunnel serves: the
// origins, the panel, every terminal. The router keeps its control path
// outside it, and puts its Handlers (the login page, logout, the sign-in's
// client document and callback) under that path without asking for the
// secret, since a visitor logging in has none. Set is what the builder calls
// with the value it settled, and what a PATCH to .env calls through the
// cache; Value and Header say it back, privately and as a visitor may see it.
// Bearer is what the control path asks of a token on a tunnel set to Single
// Sign-On.
type Auth interface {
	Handler(next http.Handler) http.Handler
	Handlers(path string) map[string]func(http.ResponseWriter, *http.Request)
	Set(value string) error
	Value() string
	Header() (key, value string)
	Unauthorized(w http.ResponseWriter, r *http.Request)
	ResourceMetadata(w http.ResponseWriter, r *http.Request)
	Bearer(r *http.Request, resource string) (sub string, ok bool, err error)
}

// SubHeader is who a request Single Sign-On let in is from, as the origin
// gets it. The gate removes a visitor's own from every request.
const SubHeader = "X-Tunneld-Sub"

// What Bearer says of a token that was sent and did not let its holder in.
// The first two are oidc's, named here so a caller of Auth needs only auth.
var (
	ErrInvalidToken = oidc.ErrInvalidToken
	ErrUnavailable  = oidc.ErrUnavailable
	// ErrNotListed is a valid token whose subject the value does not list.
	ErrNotListed = errors.New("auth: the token's subject is not listed")
)

// Option configures an AuthImpl.
type Option = v1.Option[*AuthImpl]

// state is one Set's result, swapped whole so a request reads one value.
type state struct {
	value         string
	challenges    []Challenge
	schemes       []string
	pws           []phc
	basic, bearer bool
	subs          []string
}

// AuthImpl is the default Auth: what stands between a visitor and everything
// a tunnel serves.
type AuthImpl struct {
	secret func() []byte
	owner  func() string
	server func() string
	log    v1.Logger
	now    func() time.Time
	state  atomic.Pointer[state]
	guard  *guard
	oidc   oidc.Oidc
}

// New returns an AuthImpl with nothing set: everything passes.
func New(opts ...Option) *AuthImpl {
	a := &AuthImpl{
		secret: func() []byte { return nil },
		owner:  func() string { return "" },
		server: func() string { return "https://" + v1.DefaultProvider },
		log:    slog.New(slog.DiscardHandler),
		now:    time.Now,
	}
	a.state.Store(&state{})
	v1.Apply(a, opts...)
	if a.oidc == nil {
		// Read on every ask, so a server or clock set after New is the one
		// asked.
		a.oidc = oidc.New(
			oidc.WithIssuer(func() string { return a.server() }),
			oidc.WithClock(func() time.Time { return a.now() }),
			oidc.WithLog(a.log))
	}
	a.guard = newGuard(a.now)
	return a
}

// WithAuthorizationServer is where the authorization server the resource
// metadata names is read from, on every request: the provider's origin, which
// is only settled once the command has read its flags. Nil keeps tunnel.pizza.
func WithAuthorizationServer(server func() string) Option {
	return func(a *AuthImpl) {
		if server != nil {
			a.server = server
		}
	}
}

// WithOidc sets what asks the provider about a token and a sign-in. Nil
// keeps the one it has: by default, oidc.New asking the authorization
// server.
func WithOidc(o oidc.Oidc) Option {
	return func(a *AuthImpl) {
		if o != nil {
			a.oidc = o
		}
	}
}

// WithSecret is where the tunnel secret is read from, on every request: it
// exists only once the tunnel is up and changes on every respec.
func WithSecret(secret func() []byte) Option {
	return func(a *AuthImpl) {
		if secret != nil {
			a.secret = secret
		}
	}
}

// WithOwner is where the tunnel's owner is read from, on every request: who
// its mint named ("github:<id>"), "" for nobody, which changes with every
// new tunnel. The owner counts as listed: their token opens the MCP server
// on any tunnel, and the gate on one set to Single Sign-On. Nil keeps the
// one it has.
func WithOwner(owner func() string) Option {
	return func(a *AuthImpl) {
		if owner != nil {
			a.owner = owner
		}
	}
}

// WithLog sets where auth says what changed. Nil keeps the one it has.
func WithLog(log v1.Logger) Option {
	return func(a *AuthImpl) {
		if log != nil {
			a.log = log
		}
	}
}

// Set makes value the challenge, if it parses; "" is public. A refusal
// changes nothing.
func (a *AuthImpl) Set(value string) error {
	cs, err := Parse(value)
	if err != nil {
		return err
	}
	s := &state{value: value, challenges: cs}
	for _, c := range cs {
		s.schemes = append(s.schemes, c.scheme())
		if p, err := parsePHC(c.Params["pw"]); err == nil {
			s.pws = append(s.pws, p)
		}
		switch c.scheme() {
		case "basic":
			s.basic = true
		case "bearer":
			s.bearer = true
			s.subs, _ = parseSubs(c.Params["sub"])
		}
	}
	was := a.state.Swap(s)
	a.guard.forget()
	if was.value != value {
		// Names only, never the value: it holds the password's hash.
		if len(cs) > 0 {
			a.log.Info("protection on", "schemes", s.schemes)
		} else {
			a.log.Info("protection off")
		}
	}
	return nil
}

// Value is the challenge as last Set, private params and all.
func (a *AuthImpl) Value() string { return a.state.Load().value }

// Header is the gate as a mint request and the control path carry it:
// X-Tunneld-Authenticate, each challenge in public form, its realm as stored,
// comma-joined as one field value (RFC 9110 §11.6.1); empty when public.
// Never the value as stored: that is Value's.
func (a *AuthImpl) Header() (key, value string) {
	s := a.state.Load()
	out := make([]string, len(s.challenges))
	for i, c := range s.challenges {
		out[i] = c.Public()
	}
	return v1.AuthenticateHeader, strings.Join(out, ", ")
}

// verifyAny reports whether password verifies against any challenge's pw.
func (s *state) verifyAny(password string) bool {
	for _, p := range s.pws {
		if p.verify(password) {
			return true
		}
	}
	return false
}

// Handler stands in front of next, which is everything a visitor reaches
// (the router keeps its control path outside it). A visitor's own
// X-Tunneld-Sub is removed first, always. Then, in order: nothing set
// passes; the RFC 9728 metadata passes; a valid cookie passes; with a Bearer
// challenge, a Bearer credential is judged and passes or is refused by what
// went wrong; with a Basic one, Basic that verifies passes and gets the
// cookie; anything else is a 303 to the login page for a browser page load,
// a bodyless 401 for everyone else.
func (a *AuthImpl) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withoutSub(r)
		s := a.state.Load()
		if len(s.challenges) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		// RFC 9728's metadata has to be readable without credentials, and
		// behind the gate the origin's is out of reach: answered here.
		if (r.URL.Path == MetadataPath || r.URL.Path == MCPMetadataPath) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			a.ResourceMetadata(w, r)
			return
		}
		key := cookieKey(a.secret())
		if sub, ok := a.cookied(r, key, s); ok {
			// A browser that once answered the Basic dialog keeps sending it
			// beside the cookie. The tunnel's password never reaches the
			// origin; an origin's own Basic or Bearer credential still does.
			_, pw, basic := r.BasicAuth()
			a.pass(w, r, next, basic && s.basic && a.guard.matches(r.Context(), key, s.value, pw, s.verifyAny), sub)
			return
		}
		if _, sent := bearerToken(r); sent && s.bearer {
			sub, ok, err := a.Bearer(r, "https://"+r.Host)
			switch {
			case ok:
				a.pass(w, r, next, true, sub)
			case errors.Is(err, ErrUnavailable):
				refuse(w, http.StatusServiceUnavailable, "Retry-After", "30")
			case errors.Is(err, ErrNotListed):
				refuse(w, http.StatusForbidden)
			default:
				unauthorized(w, s.challenge(r.Host, "invalid_token"))
			}
			return
		}
		if _, pw, ok := r.BasicAuth(); ok && s.basic {
			good, retry := a.guard.check(r.Context(), r.Header.Get("CF-Connecting-IP"), key, s.value, pw, s.verifyAny)
			if !good && retry == 0 {
				// Checked and wrong only: an API client sends Basic on every
				// request, so a success would bury the failures, and a held
				// address is refused before any check, so a line per refusal
				// would let anyone grow the log at request rate.
				a.log.Info("a login", "via", "basic", "ok", false)
			}
			if retry > 0 {
				refuse(w, http.StatusTooManyRequests, "Retry-After", strconv.Itoa(retry))
				return
			}
			if good {
				if key != nil {
					a.setCookie(w, key, s.value, "basic", "")
				}
				a.pass(w, r, next, true, "")
				return
			}
		}
		if pageLoad(r) {
			http.Redirect(w, r, v1.ControlPath+"login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		unauthorized(w, s.challenge(r.Host, ""))
	})
}

// withoutSub is r without any header an app could read as SubHeader: every
// value, empty ones included, in any case, and spelled with underscores,
// which CGI-style stacks read as hyphens.
func withoutSub(r *http.Request) *http.Request {
	var forged []string
	for k := range r.Header {
		if strings.EqualFold(strings.ReplaceAll(k, "_", "-"), SubHeader) {
			forged = append(forged, k)
		}
	}
	if len(forged) == 0 {
		return r
	}
	r = r.Clone(r.Context())
	for _, k := range forged {
		delete(r.Header, k)
	}
	return r
}

// challenge is the gate's 401 for a visitor of host: each challenge in public
// form, a Basic stored with no realm naming host (RFC 7617), a Bearer's
// metadata host's; errCode, when a Bearer credential was sent and did not
// verify, on the Bearer (RFC 6750 §3.1).
func (s *state) challenge(host, errCode string) string {
	out := make([]string, len(s.challenges))
	for i, c := range s.challenges {
		if _, ok := c.Params["realm"]; !ok {
			c = c.withRealm(host)
		}
		out[i] = c.publicFor(host)
		if errCode != "" && c.scheme() == "bearer" {
			out[i] += `, error="` + errCode + `"`
		}
	}
	return strings.Join(out, ", ")
}

// bearerToken is r's Authorization credential when its scheme is Bearer,
// compared case-insensitively (RFC 9110 §11.1). The header is the only place
// one is read from (RFC 6750 §2.1).
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	return token, strings.EqualFold(scheme, "Bearer") && token != ""
}

// Bearer is the subject behind r's access token when the value holds a
// Bearer challenge or the mint named an owner, the provider says the token is
// its own for resource (RFC 9068 §4), and its sub is listed or the owner. ok
// is false otherwise: with err nil when there was no token to ask about
// (neither, or no Bearer credential), and ErrInvalidToken, ErrNotListed (sub
// then says who it was) or ErrUnavailable when there was.
func (a *AuthImpl) Bearer(r *http.Request, resource string) (sub string, ok bool, err error) {
	s := a.state.Load()
	token, sent := bearerToken(r)
	if !sent || !a.byToken(s) {
		return "", false, nil
	}
	sub, err = a.oidc.VerifyAccess(r.Context(), token, resource)
	if err != nil {
		return "", false, err
	}
	if !slices.Contains(a.listed(s), sub) {
		return sub, false, ErrNotListed
	}
	return sub, true, nil
}

// byToken is whether a provider token can open anything here: the value
// holds a Bearer challenge, or the mint named an owner.
func (a *AuthImpl) byToken(s *state) bool { return s.bearer || a.owner() != "" }

// listed is who may sign in: the value's sub, and the owner the mint named.
func (a *AuthImpl) listed(s *state) []string {
	if owner := a.owner(); owner != "" {
		return append(slices.Clip(s.subs), owner)
	}
	return s.subs
}

// maxAuthCookies is how many auth cookies a request gets tried: each is an
// HMAC, and a request may carry thousands.
const maxAuthCookies = 4

// cookied is the sub of r's first valid auth cookie, and whether it had one.
// Every one is tried: the origin behind this tunnel can set one by the same
// name on a longer path, which the browser sends ahead of tunneld's own.
func (a *AuthImpl) cookied(r *http.Request, key []byte, s *state) (sub string, ok bool) {
	cookies := r.CookiesNamed(CookieName)
	if len(cookies) > maxAuthCookies {
		cookies = cookies[:maxAuthCookies]
	}
	for _, c := range cookies {
		if sub, ok := readCookie(key, c.Value, s.value, s.schemes, a.listed(s), a.now()); ok {
			return sub, true
		}
	}
	return "", false
}

// pass hands r to next without auth's own cookies, without Authorization when
// auth's credential is what let it through, and naming sub, when Single
// Sign-On let it in: the origin never sees the password or the token, and
// Chrome sends cached Basic to sibling paths unprompted. The edge is told not
// to keep what it passes: its cache key ignores cookies, so an asset the
// owner loaded would otherwise be a HIT for a visitor who never logged in.
func (a *AuthImpl) pass(w http.ResponseWriter, r *http.Request, next http.Handler, consumed bool, sub string) {
	r = r.Clone(r.Context())
	var kept []string
	for _, c := range r.Cookies() {
		if c.Name != CookieName && !strings.HasPrefix(c.Name, flowCookie) {
			kept = append(kept, c.Name+"="+c.Value)
		}
	}
	r.Header.Del("Cookie")
	if len(kept) > 0 {
		r.Header.Set("Cookie", strings.Join(kept, "; "))
	}
	if consumed {
		r.Header.Del("Authorization")
	}
	if sub != "" {
		r.Header.Set(SubHeader, sub)
	}
	next.ServeHTTP(&edgeWriter{ResponseWriter: w}, r)
}

// edgeWriter makes Cloudflare-CDN-Cache-Control no-store the last word on
// the edge when the header goes out, after the origin's own headers are in:
// an origin that sets its own would otherwise replace or join it, and let
// the edge keep a copy for visitors who never logged in. The origin's
// Cache-Control, for the browser, is left alone. Unwrap lets
// http.ResponseController reach the real writer, for flushes and upgrades.
type edgeWriter struct {
	http.ResponseWriter
	wrote bool
}

func (e *edgeWriter) WriteHeader(code int) {
	if !e.wrote {
		e.wrote = true
		e.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
	}
	e.ResponseWriter.WriteHeader(code)
}

func (e *edgeWriter) Write(b []byte) (int, error) {
	if !e.wrote {
		e.WriteHeader(http.StatusOK)
	}
	return e.ResponseWriter.Write(b)
}

func (e *edgeWriter) Unwrap() http.ResponseWriter { return e.ResponseWriter }

// MetadataPath is where a tunnel's OAuth protected-resource metadata is
// (RFC 9728): the root's well-known path, which is the origins'. The router
// asks the origin first and answers with ResourceMetadata only when it has
// none; behind a gate, the gate answers it, since the origin is out of reach.
const MetadataPath = "/.well-known/oauth-protected-resource"

// MCPMetadataPath is the MCP server's own metadata (RFC 9728 §3.1, its path
// inserted after the well-known prefix): tunneld's, never the origins'.
const MCPMetadataPath = MetadataPath + v1.ControlPath + "mcp"

// Unauthorized is the 401 for whatever the password does not open, the
// ControlPath's and a lost grant's: RFC 9110 has every 401 carry a challenge,
// and this one is RFC 9728's, a Bearer naming this tunnel's resource
// metadata (the MCP server's own, for it, where a token can open it: a
// tunnel set to Single Sign-On, or one whose mint named an owner), which
// names the provider as the
// authorization server; with error="invalid_token" when a Bearer credential
// was sent (RFC 6750 §3.1). Password or not: the password opens the origins,
// never the ControlPath.
func (a *AuthImpl) Unauthorized(w http.ResponseWriter, r *http.Request) {
	meta := MetadataPath
	if r.URL.Path == v1.ControlPath+"mcp" && a.byToken(a.state.Load()) {
		meta = MCPMetadataPath
	}
	challenge := `Bearer resource_metadata="https://` + r.Host + meta + `"`
	if _, sent := bearerToken(r); sent {
		challenge += `, error="invalid_token"`
	}
	unauthorized(w, challenge)
}

// unauthorized is every 401 tunneld sends, the gate's and Unauthorized's: no
// body, and the challenge it is given.
func unauthorized(w http.ResponseWriter, challenge string) {
	w.Header().Set("WWW-Authenticate", challenge)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusUnauthorized)
}

// metadata is RFC 9728's protected-resource metadata: this tunnel's hostname
// as the resource, and the provider as the server a token for it comes from.
type metadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// ResourceMetadata answers MetadataPath, and MCPMetadataPath for the MCP
// server, to anyone: it is what a client refused by Unauthorized reads to
// learn where to get a token. The MCP server's is a bare 404 unless a token
// can open it (byToken): anywhere else a client would sign in for a token
// the tunnel then refuses.
func (a *AuthImpl) ResourceMetadata(w http.ResponseWriter, r *http.Request) {
	resource := "https://" + r.Host
	if r.URL.Path == MCPMetadataPath {
		if !a.byToken(a.state.Load()) {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resource += v1.ControlPath + "mcp"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(metadata{
		Resource:               resource,
		AuthorizationServers:   []string{a.server()},
		ScopesSupported:        []string{"openid", "profile", "offline_access"},
		BearerMethodsSupported: []string{"header"},
	})
}

// refuse answers status with headers only: an app's fetch or an SDK never
// meets a body it was not written for.
func refuse(w http.ResponseWriter, status int, kv ...string) {
	for i := 0; i+1 < len(kv); i += 2 {
		w.Header().Set(kv[i], kv[i+1])
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// pageLoad is a browser navigating, which a 401 would answer with the
// browser's own credentials dialog. Sec-Fetch-Mode decides; with no
// Sec-Fetch-* at all, a GET or HEAD asking for HTML.
func pageLoad(r *http.Request) bool {
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	if r.Header.Get("Sec-Fetch-Dest") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		return false
	}
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (a *AuthImpl) setCookie(w http.ResponseWriter, key []byte, value, scheme, sub string) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: mintCookie(key, scheme, sub, value, a.now()),
		Path: "/", MaxAge: int(cookieLife / time.Second), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// safeNext is next if it is a path on this host outside the control path,
// "/" otherwise, and in the form it is checked in. It is judged as a browser
// will read it, not as written:
//
//   - a control character or a backslash is refused outright: a browser strips
//     a tab, so "/\t/evil" arrives as "//evil", a host;
//   - anything that parses with a scheme or a host is someone else's;
//   - the path is unescaped and cleaned before the control-path check, so
//     "/./_tunneld/logout" and "/%5Ftunneld/logout" are what they become;
//   - what goes out is that cleaned path (and the query), so the check and
//     the redirect agree.
//
// A next of /_tunneld/logout would clear the cookie just set.
func safeNext(next string) string {
	if strings.ContainsFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	clean := path.Clean(u.Path)
	if clean == "/_tunneld" || strings.HasPrefix(clean, "/_tunneld/") {
		return "/"
	}
	if strings.HasSuffix(u.Path, "/") && clean != "/" {
		clean += "/"
	}
	return (&url.URL{Path: clean, RawQuery: u.RawQuery}).String()
}

// Handlers is the login page, logout, and the sign-in's client document and
// callback, under the router's control path.
func (a *AuthImpl) Handlers(path string) map[string]func(http.ResponseWriter, *http.Request) {
	return map[string]func(http.ResponseWriter, *http.Request){
		path + "login":       a.login,
		path + "logout":      a.logout,
		path + "client.json": a.client,
		path + "callback":    a.callback,
	}
}

// page is what the login page shows: the password form when the value has a
// Basic, the sign-in link when it has a Bearer.
type page struct {
	Host, Next, Error string
	Password, SSO     bool
	SignIn, Label     string
	// Notice is news that is not an error: "Signed out of <host>."
	Notice string
}

func (a *AuthImpl) render(w http.ResponseWriter, r *http.Request, status int, p page) {
	p.Host = r.Host
	if p.SSO {
		p.SignIn = cmp.Or(p.SignIn, signInURL(p.Next, ""))
		p.Label = cmp.Or(p.Label, "Sign in with tunnel.pizza")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is one inline stylesheet, a form posting to itself and a
	// link. Another site's frame could dress the password field up as
	// something else; the multiview panel's tiles are this origin's own
	// frames, and a tile that lands here must still show the form.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'self'; base-uri 'none'")
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, p)
}

func (a *AuthImpl) login(w http.ResponseWriter, r *http.Request) {
	s := a.state.Load()
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		q := r.URL.Query()
		next := safeNext(q.Get("next"))
		if len(s.challenges) == 0 {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		if s.bearer && (q.Get("sso") == "1" || !s.basic) {
			a.signIn(w, r, s, next, q.Get("prompt"))
			return
		}
		a.render(w, r, http.StatusOK, page{Next: next, Password: s.basic, SSO: s.bearer})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		_ = r.ParseForm()
		next := safeNext(r.PostForm.Get("next"))
		if len(s.challenges) == 0 {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		if !s.basic {
			http.Redirect(w, r, v1.ControlPath+"login?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		key := cookieKey(a.secret())
		if key == nil {
			w.Header().Set("Retry-After", "2")
			a.render(w, r, http.StatusServiceUnavailable, page{Next: next, Password: true, SSO: s.bearer, Error: "This tunnel is still starting. Try again in a moment."})
			return
		}
		good, retry := a.guard.check(r.Context(), r.Header.Get("CF-Connecting-IP"), key, s.value, r.PostForm.Get("password"), s.verifyAny)
		a.log.Info("a login", "via", "form", "ok", good, "held", retry > 0)
		switch {
		case retry > 0:
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			a.render(w, r, http.StatusTooManyRequests, page{Next: next, Password: true, SSO: s.bearer, Error: "Too many tries. Wait a minute, then try again."})
		case good:
			a.setCookie(w, key, s.value, "basic", "")
			http.Redirect(w, r, next, http.StatusSeeOther)
		default:
			a.render(w, r, http.StatusOK, page{Next: next, Password: true, SSO: s.bearer, Error: "That password isn't right."})
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *AuthImpl) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	next := safeNext(r.URL.Query().Get("next"))
	s := a.state.Load()
	if len(s.challenges) == 0 {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	// The login page itself, not a redirect to next: through the gate, a
	// Single Sign-On tunnel would send the browser to the provider, which
	// still has its session and remembered consent, and it would be signed
	// straight back in. Signing in again is the page's button.
	a.render(w, r, http.StatusOK, page{
		Next: next, Password: s.basic, SSO: s.bearer,
		Notice: "Signed out of " + r.Host + ".",
	})
}
