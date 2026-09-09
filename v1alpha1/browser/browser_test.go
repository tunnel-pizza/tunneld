package browser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cnuss/libtunnel"
	"github.com/creack/pty"
	pkgbrowser "github.com/pkg/browser"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// discard is the logger every test hands to Interceptors: nothing under test
// asserts on log output, so it stays quiet.
var discard = slog.New(slog.DiscardHandler)

// fakeIC is the interceptor context the tests hand to a Handler: it records
// the handler installed with WithHandler and answers Handler with whatever
// the test says the proxy would have done. The other four methods are never
// reached and panic through the nil embed if they are.
type fakeIC struct {
	libtunnel.InterceptCtx
	next      http.HandlerFunc
	installed http.HandlerFunc
}

func (f *fakeIC) Handler() http.HandlerFunc { return f.next }
func (f *fakeIC) WithHandler(h http.HandlerFunc) libtunnel.InterceptCtx {
	f.installed = h
	return f
}

// framed is the origin list the helpers below hand to Interceptors when the
// test does not care which origins they are: two, because a lone origin is
// not framed at all and Interceptors would answer with nothing to return.
var framed = []*url.URL{
	{Scheme: "http", Host: "localhost:3000"},
	{Scheme: "http", Host: "localhost:4000"},
}

// pageOf returns the panel's own interceptor: the one that answers the bare
// tunnel address with the page of frames.
func pageOf(t *testing.T, origins []*url.URL) libtunnel.Interceptor {
	t.Helper()
	return New().Interceptors(true, origins, discard)[0]
}

// unframeOf returns the interceptor that strips framing headers from the
// panel's own frames. Which origins are framed is nothing to it, so it takes
// the fixture.
func unframeOf(t *testing.T) libtunnel.Interceptor {
	t.Helper()
	return New().Interceptors(true, framed, discard)[1]
}

// TestIsPanelRequest pins which requests reach the panel. The narrowing is
// the whole design: the panel answers the tunnel's own address and nothing
// else, because everything else belongs to an origin.
// TestOpenDecides covers the browser decision, which has no flag and no
// variable behind it any more. Each row is somewhere somebody actually runs,
// and the answer is the one they would give without being asked.
//
// Driven through Open with a launcher that records rather than launches, since
// what is under test is whether the attempt is made at all. Every variable it
// reads is set explicitly, the ones a runner sets for itself included: this
// suite runs under $CI, which is one of the signals.
func TestOpenDecides(t *testing.T) {
	const ssh = "10.0.0.1 51234 10.0.0.2 22"
	// watched and drawing are the two shapes a row starts from. The terminal
	// they name is a real pty, because that is what WithInteractive asks the
	// streams about and a buffer can never answer yes.
	watched := func(t *testing.T) []Option { return []Option{WithInteractive(onATerminal(t))} }
	drawing := func(t *testing.T) []Option { return append(watched(t), WithScreen(stillScreen{})) }
	pipe := func(*testing.T) []Option { return nil }
	for _, tc := range []struct {
		name string
		when func(*testing.T) []Option
		env  map[string]string
		want bool
	}{
		{"a desktop session", watched, map[string]string{"DISPLAY": ":0"}, true},
		{"a wayland session", watched, map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, true},
		{"nothing is watching a pipe", pipe, map[string]string{"DISPLAY": ":0"}, false},
		{"a runner", watched, map[string]string{"DISPLAY": ":0", "CI": "true"}, false},
		{"CI set to a falsehood is not a runner", watched, map[string]string{"DISPLAY": ":0", "CI": "false"}, true},
		{"ssh with nothing forwarded", watched, map[string]string{"SSH_CONNECTION": ssh}, false},
		{"ssh by its tty alone", watched, map[string]string{"SSH_TTY": "/dev/pts/0"}, false},
		{"ssh -X", watched, map[string]string{"SSH_CONNECTION": ssh, "DISPLAY": "localhost:10.0"}, true},
		// $CI is asked before ssh, because a runner reached over ssh is still
		// a runner and the display it forwarded is still nobody's.
		{"a runner reached over ssh", watched, map[string]string{"SSH_CONNECTION": ssh, "DISPLAY": "localhost:10.0", "CI": "true"}, false},
		// The console is already showing it, which outranks every signal
		// below and the caller's own instruction above.
		{"a console already showing it", drawing, map[string]string{"DISPLAY": ":0"}, false},
		{"a caller who insists cannot beat that", forced(drawing, true), map[string]string{"DISPLAY": ":0"}, false},
		// The caller beats everything the machine has to say.
		{"a caller who declines", forced(watched, false), map[string]string{"DISPLAY": ":0"}, false},
		{"a caller who insists over a pipe", forced(pipe, true), map[string]string{"CI": "true"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"CI", "SSH_CONNECTION", "SSH_TTY", "DISPLAY", "WAYLAND_DISPLAY"} {
				t.Setenv(name, tc.env[name])
			}
			var launched []string
			b := New(WithLaunch(func(addr string) error {
				launched = append(launched, addr)
				return nil
			}))
			b.Open(t.Context(), discard, append([]Option{WithAddr("https://foo.tunneled.pizza/")}, tc.when(t)...)...)
			if got := len(launched) > 0; got != tc.want {
				t.Errorf("launched %q, want a browser: %v", launched, tc.want)
			}
		})
	}
}

// forced is a row's options with a caller's own answer appended, which reads
// better in a table than an append inside a composite literal.
func forced(base func(*testing.T) []Option, open bool) func(*testing.T) []Option {
	return func(t *testing.T) []Option { return append(base(t), WithForced(&open)) }
}

// onATerminal is a run whose output goes somewhere a person can see, which is
// what WithInteractive asks the streams about — a real pty, since nothing else
// answers yes.
func onATerminal(t *testing.T) Streams {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty to be a terminal on: %v", err)
	}
	t.Cleanup(func() { tty.Close(); ptmx.Close() })
	return streams{tty}
}

type streams struct{ tty *os.File }

func (s streams) InOrStdin() io.Reader   { return s.tty }
func (s streams) OutOrStdout() io.Writer { return s.tty }
func (s streams) ErrOrStderr() io.Writer { return s.tty }

// stillScreen is a console that shows nothing. The rows using it are about
// what a console being there means, not about what it shows.
type stillScreen struct{}

func (stillScreen) Show(context.Context, v1.Logger) {}

// ptr is a *bool for a literal, which When.Forced needs and Go has no spelling
// for inline.
func ptr(b bool) *bool { return &b }

func TestIsPanelRequest(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		dest    string
		referer string
		upgrade string
		want    bool
	}{
		{name: "the bare hostname", target: "/", want: true},
		{name: "a typed top-level visit", target: "/", dest: "document", want: true},
		{name: "an explicit index at the root", target: "/?0", want: false},
		{name: "a later origin's index", target: "/?2", want: false},
		{name: "an origin subresource", target: "/app.js", want: false},
		{name: "an origin page below the root", target: "/dashboard", want: false},
		{name: "an app's own root parameters", target: "/?page=1", want: false},
		{name: "an OAuth callback", target: "/?code=abc&state=xyz", want: false},
		{name: "an empty query string", target: "/?", want: true},
		{name: "a frame navigating to the root", target: "/", dest: "iframe", want: false},
		{name: "a script or fetch for the root", target: "/", dest: "empty", want: false},
		{name: "a link from a page on this host", target: "/", dest: "document", referer: "https://foo.tunneled.pizza/?1", want: false},
		{name: "a link from somewhere else", target: "/", dest: "document", referer: "https://example.test/", want: true},
		{name: "a websocket handshake to the root", target: "/", upgrade: "websocket", want: false},
		{name: "a websocket handshake with no query", target: "/?", upgrade: "websocket", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			r.Host = "foo.tunneled.pizza"
			if tc.dest != "" {
				r.Header.Set("Sec-Fetch-Dest", tc.dest)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			if tc.upgrade != "" {
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", tc.upgrade)
			}
			if got := pageOf(t, framed).Match(r); got != tc.want {
				t.Errorf("Match(%q dest=%q referer=%q) = %v, want %v",
					tc.target, tc.dest, tc.referer, got, tc.want)
			}
		})
	}
}

// TestNoPanel pins that the panel needs both the flag and something to
// compare, and that URL and Interceptors agree about it. One origin framed
// alone is a worse view of it than the origin itself, so a lone origin keeps
// being opened directly.
func TestNoPanel(t *testing.T) {
	one, err := mustOrigins([]string{"http://localhost:3000"})
	if err != nil {
		t.Fatalf("origins: %v", err)
	}
	two, err := mustOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("origins: %v", err)
	}

	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cases := []struct {
		name    string
		enabled bool
		origins []*url.URL
		wanted  bool
	}{
		{"on, two origins", true, two, true},
		{"on, one origin", true, one, false},
		{"off, two origins", false, two, false},
		{"on, no origins", true, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := New().URL(tc.enabled, public, tc.origins)
			if wanted := got != ""; wanted != tc.wanted {
				t.Errorf("URL() = %q, want a panel address: %v", got, tc.wanted)
			}
			ics := New().Interceptors(tc.enabled, tc.origins, discard)
			if wanted := len(ics) > 0; wanted != tc.wanted {
				t.Errorf("Interceptors() returned %d, want any: %v", len(ics), tc.wanted)
			}
		})
	}
}

// TestURL pins that the panel's address is the tunnel's own, and that
// building it leaves the tunnel URL alone — every origin address derives from
// the same value.
func TestURL(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := New().URL(true, public, framed), "https://foo.tunneled.pizza/"; got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
	if public.RawQuery != "" {
		t.Errorf("URL mutated its argument: RawQuery = %q, want empty", public.RawQuery)
	}
}

// TestServeShell renders the panel and pins what the page has to carry: a tile
// per origin, each addressed by its routing index, and the host the visitor
// actually used rather than one baked in at mint time.
func TestServeShell(t *testing.T) {
	origins, err := mustOrigins([]string{"http://localhost:3000", "http://localhost:4000", "http://localhost:5000"})
	if err != nil {
		t.Fatalf("origins: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "foo.tunneled.pizza"

	ic := &fakeIC{}
	pageOf(t, origins).Handler(ic)
	ic.installed(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"foo.tunneled.pizza",
		`src="/?0"`,
		`src="/?1"`,
		`src="/?2"`,
		"localhost:3000",
		"localhost:5000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page does not contain %q", want)
		}
	}
	if got, want := strings.Count(body, "<iframe"), len(origins); got != want {
		t.Errorf("page has %d frames, want one per origin (%d)", got, want)
	}

	// The page focuses a tile itself when it is clicked, because an origin
	// that calls preventDefault on mousedown cancels the focus the browser
	// would otherwise have moved. Without this a tile can never take the
	// keyboard and every keystroke keeps going to whichever tile had it last.
	// Asserted here because the reason is invisible from the markup: nothing
	// looks broken if it is deleted until two origins are open at once.
	for _, want := range []string{"pointerdown", "contentWindow.focus()"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page does not contain %q, so a clicked tile will not take focus", want)
		}
	}
}

// TestServeShellEscapesTheHost pins that the one value taken from the request
// is escaped. Host is attacker-controlled -- anyone can send whatever Host
// header they like -- and it lands in an HTML document, which is the shape of
// bug that has already cost this repo one CodeQL alert.
func TestServeShellEscapesTheHost(t *testing.T) {
	origins, err := mustOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("origins: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = `evil"><script>alert(1)</script>`

	ic := &fakeIC{}
	pageOf(t, origins).Handler(ic)
	ic.installed(rec, r)

	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Error("the Host header reached the page as markup, want it escaped")
	}
}

// TestLabel pins how a tile names the origin behind it. An http origin is
// named by its host, which is what the operator typed; anything else keeps its
// scheme, so a container tile cannot be misread as a hostname.
func TestLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://localhost:3000", "localhost:3000"},
		{"https://127.0.0.1:8443", "127.0.0.1:8443"},
		{"dockerd://api", "dockerd://api"},
		{"http+ws://localhost:5173", "localhost:5173"},
		{"https+wss://localhost:5173", "localhost:5173"},
	}

	origins := make([]*url.URL, len(cases))
	for i, tc := range cases {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.in, err)
		}
		origins[i] = u
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "foo.tunneled.pizza"

	ic := &fakeIC{}
	pageOf(t, origins).Handler(ic)
	ic.installed(rec, r)

	body := rec.Body.String()
	for _, want := range []string{"localhost:3000", "127.0.0.1:8443", "dockerd://api"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page does not contain label %q", want)
		}
	}
	// http+ws and https+wss both name the tile by host alone, so each gets
	// its own iframe with that exact title: "localhost:5173" twice over.
	if got := strings.Count(body, `title="localhost:5173"`); got != 2 {
		t.Errorf("rendered page has %d iframes titled %q, want 2 (one per +ws/+wss origin)", got, "localhost:5173")
	}
}

// TestPanelInterceptorServesTheShell pins the wiring: the interceptor
// matches the panel's parameter and replaces the handler that would otherwise
// proxy the request to an origin.
func TestPanelInterceptorServesTheShell(t *testing.T) {
	origins, err := mustOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("origins: %v", err)
	}

	interceptor := pageOf(t, origins)
	if interceptor.Priority != 1 {
		t.Errorf("Priority = %d, want 1 so nothing later can shadow the panel", interceptor.Priority)
	}
	if !interceptor.Match(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("interceptor does not match the tunnel's own address")
	}
	if interceptor.Match(httptest.NewRequest(http.MethodGet, "/?1", nil)) {
		t.Error("interceptor matches a routing index, which belongs to an origin")
	}
}

// TestIsPanelFrame pins how narrow the framing-header removal is. Only a frame
// navigation from this same tunnel qualifies: a top-level visit keeps whatever
// the origin sent, and another site's attempt to frame the tunnel arrives
// cross-site and is left alone.
func TestIsPanelFrame(t *testing.T) {
	cases := []struct {
		name string
		dest string
		site string
		want bool
	}{
		{"the panel's own frame", "iframe", "same-origin", true},
		{"a legacy frame element", "frame", "same-origin", true},
		{"a top-level visit", "document", "same-origin", false},
		{"a subresource", "script", "same-origin", false},
		{"another site framing the tunnel", "iframe", "cross-site", false},
		{"a same-site but different origin frame", "iframe", "same-site", false},
		{"a browser that sends no Sec-Fetch headers", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?1", nil)
			if tc.dest != "" {
				r.Header.Set("Sec-Fetch-Dest", tc.dest)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if got := unframeOf(t).Match(r); got != tc.want {
				t.Errorf("Match(dest=%q site=%q) = %v, want %v", tc.dest, tc.site, got, tc.want)
			}
		})
	}
}

// TestWithoutFrameAncestors pins that only the framing directive is removed.
// The rest of a policy is what the origin relies on to be safe at all, so
// dropping more than asked would trade one exposure for a worse one.
func TestWithoutFrameAncestors(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		want   string
	}{
		{"only frame-ancestors", "frame-ancestors 'none'", ""},
		{"leading directive", "frame-ancestors 'none'; script-src 'self'", "script-src 'self'"},
		{"trailing directive", "default-src 'self'; frame-ancestors 'none'", "default-src 'self'"},
		{"middle directive", "default-src 'self'; frame-ancestors 'none'; img-src *", "default-src 'self'; img-src *"},
		{"case insensitive", "FRAME-ANCESTORS 'none'; img-src *", "img-src *"},
		{"nothing to remove", "default-src 'self'", "default-src 'self'"},
		{"a similarly named directive survives", "frame-src 'self'", "frame-src 'self'"},
		{"stray semicolons", "; default-src 'self' ;; frame-ancestors 'none';", "default-src 'self'"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Security-Policy", tc.policy)
				w.WriteHeader(http.StatusOK)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/?1", nil)
			req.Header.Set("Sec-Fetch-Dest", "iframe")
			req.Header.Set("Sec-Fetch-Site", "same-origin")

			ic := &fakeIC{next: next}
			unframeOf(t).Handler(ic)
			ic.installed(rec, req)

			if got := rec.Header().Get("Content-Security-Policy"); got != tc.want {
				t.Errorf("asTile(%q) = %q, want %q", tc.policy, got, tc.want)
			}
		})
	}
}

// TestStripFraming pins the header surgery across both spellings of the same
// refusal, including the report-only variant and an origin that sends several
// policies.
func TestStripFraming(t *testing.T) {
	next := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Add("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		w.Header().Add("Content-Security-Policy", "frame-ancestors https://example.test")
		w.Header().Set("Content-Security-Policy-Report-Only", "frame-ancestors 'none'; img-src *")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?1", nil)
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	ic := &fakeIC{next: next}
	unframeOf(t).Handler(ic)
	ic.installed(rec, req)

	if got := rec.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q, want it removed", got)
	}
	if got := rec.Header().Values("Content-Security-Policy"); len(got) != 1 || got[0] != "default-src 'self'" {
		t.Errorf("Content-Security-Policy = %q, want only the non-framing directives", got)
	}
	if got := rec.Header().Get("Content-Security-Policy-Report-Only"); got != "img-src *" {
		t.Errorf("report-only policy = %q, want only the non-framing directives", got)
	}
	// Everything else the origin sent is none of our business.
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want it untouched", got)
	}
}

// TestTileRestoresTheReferer pins that a framed origin's Referrer-Policy is
// dropped, which is what keeps its own assets routable.
//
// The tunnel routes a subresource to the origin that asked for it by reading
// the Referer its document set. An origin sending no-referrer — ordinary
// hardening, and what xpra's HTML5 client does — leaves every asset it
// requests with nothing to route by, so they fall back to a cookie that is
// last-write-wins across tiles and are served by whichever origin was framed
// most recently. The tile renders as unstyled markup, and the 404s come from
// an origin that genuinely does not have those files, so nothing says the
// routing went wrong.
//
// Dropping the header rather than rewriting it is deliberate: the browser
// default already sends the full URL for the same-origin requests this is
// about. The narrowing to the panel's own frames is the interceptor's Match,
// covered by TestIsPanelFrame — a top-level visit keeps whatever the origin
// sent.
func TestTileRestoresTheReferer(t *testing.T) {
	for _, policy := range []string{"no-referrer", "origin", "same-origin"} {
		t.Run(policy, func(t *testing.T) {
			rec := httptest.NewRecorder()
			u := &asTile{ResponseWriter: rec}
			u.Header().Set("Referrer-Policy", policy)
			u.Header().Set("X-Content-Type-Options", "nosniff")
			u.WriteHeader(http.StatusOK)

			if got := rec.Header().Get("Referrer-Policy"); got != "" {
				t.Errorf("Referrer-Policy = %q, want it removed so assets keep a Referer", got)
			}
			// Still none of our business.
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want it untouched", got)
			}
		})
	}
}

// TestUnframerScrubsBeforeTheWrite pins that the scrub happens while headers
// are still mutable, including for a handler that never calls WriteHeader and
// so would otherwise have its implicit 200 written past the wrapper.
func TestUnframerScrubsBeforeTheWrite(t *testing.T) {
	t.Run("explicit WriteHeader", func(t *testing.T) {
		rec := httptest.NewRecorder()
		u := &asTile{ResponseWriter: rec}
		u.Header().Set("X-Frame-Options", "DENY")
		u.WriteHeader(http.StatusOK)

		if got := rec.Header().Get("X-Frame-Options"); got != "" {
			t.Errorf("X-Frame-Options = %q, want it removed", got)
		}
	})

	t.Run("implicit 200 via Write", func(t *testing.T) {
		rec := httptest.NewRecorder()
		u := &asTile{ResponseWriter: rec}
		u.Header().Set("X-Frame-Options", "SAMEORIGIN")
		if _, err := u.Write([]byte("hello")); err != nil {
			t.Fatalf("Write: %v", err)
		}

		if got := rec.Header().Get("X-Frame-Options"); got != "" {
			t.Errorf("X-Frame-Options = %q, want it removed", got)
		}
		if rec.Body.String() != "hello" {
			t.Errorf("body = %q, want it passed through", rec.Body.String())
		}
	})

	t.Run("Unwrap reaches the real writer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		u := &asTile{ResponseWriter: rec}
		if u.Unwrap() != http.ResponseWriter(rec) {
			t.Error("Unwrap did not return the wrapped writer, so flush and hijack would break")
		}
	})
}

// TestUnframeIsBehindThePanel pins the ordering: the panel is served before
// anything considers framing, and the unframer never matches the panel's own
// request.
func TestUnframeIsBehindThePanel(t *testing.T) {
	pagePriority := pageOf(t, framed).Priority
	if got := unframeOf(t).Priority; got <= pagePriority {
		t.Errorf("unframe Priority = %d, want it behind the page's %d", got, pagePriority)
	}

	panelReq := httptest.NewRequest(http.MethodGet, "/", nil)
	if unframeOf(t).Match(panelReq) {
		t.Error("the unframer matched the panel request, which it does not serve")
	}
}

// TestInterceptorsOrder pins what run relies on: the page comes first and
// outranks the unframer, so the one request that must never reach an origin
// is answered before anything looks at framing. Swapping the two fails this.
func TestInterceptorsOrder(t *testing.T) {
	got := New().Interceptors(true, framed, discard)
	if len(got) != 2 {
		t.Fatalf("Interceptors() returned %d, want 2", len(got))
	}
	if !got[0].Match(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("Interceptors()[0] does not match the panel request, want the page first")
	}
	if got[0].Priority >= got[1].Priority {
		t.Errorf("page Priority = %d, unframe = %d; want the page ahead", got[0].Priority, got[1].Priority)
	}
}

// TestOpen pins the launch and the two promises around it: it never writes to
// stdout, and it never raises its voice above a debug line when it fails.
//
// The wait that used to stand in front of it lives in the counter now — the
// edge answers 530 for a moment after a tunnel reports ready, and its own
// event stream says when that is over, which an HTTP probe from here could
// only guess at.
func TestOpen(t *testing.T) {
	// opens the address and leaves stdout alone: the spawned process
	// inherits writers from pkg/browser's package globals, which default to
	// os.Stdout, and that stream carries a running tunnel's addresses.
	t.Run("opens the address and leaves stdout alone", func(t *testing.T) {
		var opened string
		o := New(WithLaunch(func(addr string) error {
			opened = addr
			return nil
		}))

		var stderr bytes.Buffer
		o.Open(t.Context(), slog.New(slog.DiscardHandler), WithAddr("https://striped-worm.tunneled.pizza/"), WithForced(ptr(true)), WithStderr(&stderr))

		if want := "https://striped-worm.tunneled.pizza/"; opened != want {
			t.Errorf("opened %q, want %q", opened, want)
		}
		if pkgbrowser.Stdout != io.Writer(&stderr) {
			t.Error("browser.Stdout was left pointing elsewhere, want the stderr writer")
		}
		if pkgbrowser.Stderr != io.Writer(&stderr) {
			t.Error("browser.Stderr was left pointing elsewhere, want the stderr writer")
		}
	})

	// asks the address nothing: the probe moved to the counter, and an Open
	// that still reached for the network would wait twice for one answer.
	t.Run("reaches for the network not at all", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusBadGateway) // the edge, not the origin
		}))
		defer srv.Close()

		o := New(WithLaunch(func(string) error { return nil }))
		o.Open(t.Context(), slog.New(slog.DiscardHandler), WithAddr(srv.URL), WithForced(ptr(true)), WithStderr(io.Discard))

		if got := requests.Load(); got != 0 {
			t.Errorf("made %d requests to the address, want none", got)
		}
	})

	// a dead context still launches: Open does not wait, so there is nothing
	// for a cancellation to cut short, and refusing to launch would only
	// withhold a page from somebody whose tunnel is up.
	t.Run("a cancelled context still launches", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		var launched bool
		o := New(WithLaunch(func(string) error {
			launched = true
			return nil
		}))
		o.Open(ctx, slog.New(slog.DiscardHandler), WithAddr("https://striped-worm.tunneled.pizza/"), WithForced(ptr(true)), WithStderr(io.Discard))

		if !launched {
			t.Error("a cancelled context stopped the launch, want it to open anyway")
		}
	})

	// a failure to open is a debug line, not an error: the tunnel is up and
	// serving either way, and a headless host is a normal place to run this,
	// not a broken one. It must still be there for somebody debugging a
	// browser that did not appear.
	t.Run("a failure is quiet outside the debug log", func(t *testing.T) {
		o := New(WithLaunch(func(string) error { return errors.New("no browser here") }))
		// Forced, so the decision above cannot be what keeps it quiet: the
		// launch has to be attempted for its failure to be the thing tested.
		anyway := []Option{WithAddr("https://striped-worm.tunneled.pizza/"), WithForced(ptr(true))}

		var quiet bytes.Buffer
		o.Open(t.Context(), slog.New(slog.NewTextHandler(&quiet, &slog.HandlerOptions{Level: slog.LevelWarn})), anyway...)
		if quiet.Len() != 0 {
			t.Errorf("log = %q, want nothing at warn level", quiet.String())
		}

		var logged bytes.Buffer
		o.Open(t.Context(), slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})), anyway...)
		if !strings.Contains(logged.String(), "could not open a browser") {
			t.Errorf("log = %q, want the failure in the debug log", logged.String())
		}
	})
}

// mustOrigins builds origin URLs for the tables above. The real parsing lives
// in v1alpha1, which this package cannot import — it is the other direction —
// and these fixtures are already-valid URLs, so a plain parse is enough.
func mustOrigins(raw []string) ([]*url.URL, error) {
	origins := make([]*url.URL, 0, len(raw))
	for _, s := range raw {
		u, err := url.Parse(s)
		if err != nil {
			return nil, err
		}
		origins = append(origins, u)
	}
	return origins, nil
}
