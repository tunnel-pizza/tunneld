package auth

import (
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
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
// outside it, and puts its Handlers (the login page, logout) under that path
// without asking for the secret, since a visitor logging in has none. Set is
// what the builder calls with the password it settled, and what a PATCH to
// .env calls through the cache; Value and Header say it back, privately and
// as a visitor may see it.
type Auth interface {
	Handler(next http.Handler) http.Handler
	Handlers(path string) map[string]func(http.ResponseWriter, *http.Request)
	Set(value string) error
	Value() string
	Header() (key, value string)
}

// Option configures an AuthImpl.
type Option = v1.Option[*AuthImpl]

// state is one Set's result, swapped whole so a request reads one value.
type state struct {
	value      string
	challenges []Challenge
	schemes    []string
	pws        []phc
}

// AuthImpl is the default Auth: what stands between a visitor and everything
// a tunnel serves.
type AuthImpl struct {
	secret func() []byte
	log    v1.Logger
	now    func() time.Time
	state  atomic.Pointer[state]
	guard  *guard
}

// New returns an AuthImpl with nothing set: everything passes.
func New(opts ...Option) *AuthImpl {
	a := &AuthImpl{secret: func() []byte { return nil }, log: slog.New(slog.DiscardHandler), now: time.Now}
	a.state.Store(&state{})
	v1.Apply(a, opts...)
	a.guard = newGuard(a.now)
	return a
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
	}
	was := a.state.Swap(s)
	a.guard.forget()
	if was.value != value {
		// Names only, never the value: it holds the password's hash.
		if len(cs) > 0 {
			a.log.Info("password protection on")
		} else {
			a.log.Info("password protection off")
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
// (the router keeps its control path outside it). In order: nothing set
// passes; a valid cookie passes; Basic that verifies passes and gets the
// cookie; anything else is a 303 to the login page for a browser page load,
// a bodyless 401 for everyone else.
func (a *AuthImpl) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := a.state.Load()
		if len(s.challenges) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		key := cookieKey(a.secret())
		if a.cookied(r, key, s) {
			// A browser that once answered the Basic dialog keeps sending it
			// beside the cookie. The tunnel's password never reaches the
			// origin; an origin's own Basic credential still does.
			_, pw, basic := r.BasicAuth()
			a.pass(w, r, next, basic && a.guard.matches(r.Context(), key, s.value, pw, s.verifyAny))
			return
		}
		if _, pw, ok := r.BasicAuth(); ok {
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
					a.setCookie(w, s, key)
				}
				a.pass(w, r, next, true)
				return
			}
		}
		if pageLoad(r) {
			http.Redirect(w, r, "/_tunneld/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		// Header's public form, but Basic needs a realm (RFC 7617): a
		// challenge stored with none names the hostname the visitor asked for.
		out := make([]string, len(s.challenges))
		for i, c := range s.challenges {
			if _, ok := c.Params["realm"]; !ok {
				c = c.withRealm(r.Host)
			}
			out[i] = c.Public()
		}
		Unauthorized(w, strings.Join(out, ", "))
	})
}

// maxAuthCookies is how many auth cookies a request gets tried: each is an
// HMAC, and a request may carry thousands.
const maxAuthCookies = 4

// cookied reports whether any of r's auth cookies is valid. Every one is
// tried: a sibling tunnel on the shared domain can set one by the same name,
// which the browser may send ahead of this tunnel's own.
func (a *AuthImpl) cookied(r *http.Request, key []byte, s *state) bool {
	cookies := r.CookiesNamed(CookieName)
	if len(cookies) > maxAuthCookies {
		cookies = cookies[:maxAuthCookies]
	}
	for _, c := range cookies {
		if readCookie(key, c.Value, s.value, s.schemes, a.now()) {
			return true
		}
	}
	return false
}

// pass hands r to next without auth's own cookie, and without Authorization
// when Basic is what let it through: the origin never sees the password, and
// Chrome sends cached Basic to sibling paths unprompted. The edge is told
// not to keep what it passes: its cache key ignores cookies, so an asset the
// owner loaded would otherwise be a HIT for a visitor who never logged in.
func (a *AuthImpl) pass(w http.ResponseWriter, r *http.Request, next http.Handler, consumed bool) {
	r = r.Clone(r.Context())
	var kept []string
	for _, c := range r.Cookies() {
		if c.Name != CookieName {
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

// refuse answers status with headers only: an app's fetch or an SDK never
// meets a body it was not written for.
// Unauthorized is every 401 tunneld sends, the gate's, the control path's and
// the cache's: no body, and WWW-Authenticate when there is a challenge to
// answer. A refusal under the control path passes none, since a password
// does not open it; only the secret does.
func Unauthorized(w http.ResponseWriter, challenge string) {
	if challenge != "" {
		w.Header().Set("WWW-Authenticate", challenge)
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusUnauthorized)
}

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

func (a *AuthImpl) setCookie(w http.ResponseWriter, s *state, key []byte) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: mintCookie(key, s.schemes[0], s.value, a.now()),
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

// Handlers is the login page and logout, under the router's control path.
func (a *AuthImpl) Handlers(path string) map[string]func(http.ResponseWriter, *http.Request) {
	return map[string]func(http.ResponseWriter, *http.Request){
		path + "login":  a.login,
		path + "logout": a.logout,
	}
}

type page struct{ Host, Next, Error string }

func (a *AuthImpl) render(w http.ResponseWriter, r *http.Request, status int, next, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is one inline stylesheet and a form posting to itself.
	// Another site's frame could dress the password field up as something
	// else; the multiview panel's tiles are this origin's own frames, and a
	// tile that lands here must still show the form.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'self'; base-uri 'none'")
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, page{Host: r.Host, Next: next, Error: msg})
}

func (a *AuthImpl) login(w http.ResponseWriter, r *http.Request) {
	s := a.state.Load()
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		next := safeNext(r.URL.Query().Get("next"))
		if len(s.challenges) == 0 {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		a.render(w, r, http.StatusOK, next, "")
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		_ = r.ParseForm()
		next := safeNext(r.PostForm.Get("next"))
		if len(s.challenges) == 0 {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		key := cookieKey(a.secret())
		if key == nil {
			w.Header().Set("Retry-After", "2")
			a.render(w, r, http.StatusServiceUnavailable, next, "This tunnel is still starting. Try again in a moment.")
			return
		}
		good, retry := a.guard.check(r.Context(), r.Header.Get("CF-Connecting-IP"), key, s.value, r.PostForm.Get("password"), s.verifyAny)
		a.log.Info("a login", "via", "form", "ok", good, "held", retry > 0)
		switch {
		case retry > 0:
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			a.render(w, r, http.StatusTooManyRequests, next, "Too many tries. Wait a minute, then try again.")
		case good:
			a.setCookie(w, s, key)
			http.Redirect(w, r, next, http.StatusSeeOther)
		default:
			a.render(w, r, http.StatusOK, next, "That password isn't right.")
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
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}
