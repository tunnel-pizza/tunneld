package auth

import (
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
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

// Public is each challenge as a visitor may see it, realm set to host (left
// out when host is ""); nil when public.
func (a *AuthImpl) Public(host string) []string {
	s := a.state.Load()
	if len(s.challenges) == 0 {
		return nil
	}
	out := make([]string, len(s.challenges))
	for i, c := range s.challenges {
		out[i] = c.Public(host)
	}
	return out
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
		if c, err := r.Cookie(CookieName); err == nil && readCookie(key, c.Value, s.value, s.schemes, a.now()) {
			a.pass(w, r, next, false)
			return
		}
		if _, pw, ok := r.BasicAuth(); ok {
			good, retry := a.guard.check(r.Context(), r.Header.Get("CF-Connecting-IP"), key, s.value, pw, s.verifyAny)
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
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/_tunneld/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		for _, v := range a.Public(r.Host) {
			w.Header().Add("WWW-Authenticate", v)
		}
		refuse(w, http.StatusUnauthorized)
	})
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
	w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
	next.ServeHTTP(w, r)
}

// refuse answers status with headers only: an app's fetch or an SDK never
// meets a body it was not written for.
func refuse(w http.ResponseWriter, status int, kv ...string) {
	for i := 0; i+1 < len(kv); i += 2 {
		w.Header().Set(kv[i], kv[i+1])
	}
	w.Header().Set("Cache-Control", "no-store")
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
// "/" otherwise: a next of /_tunneld/logout would clear the cookie just set.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) ||
		strings.HasPrefix(next, "/_tunneld/") {
		return "/"
	}
	return next
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
	w.Header().Set("Cache-Control", "no-store")
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
		a.log.Info("a login", "ok", good, "held", retry > 0)
		switch {
		case retry > 0:
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			a.render(w, r, http.StatusTooManyRequests, next, "Too many tries. Wait a minute, then try again.")
		case good:
			a.setCookie(w, s, key)
			w.Header().Set("Cache-Control", "no-store")
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
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}
