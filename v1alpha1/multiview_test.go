package v1alpha1

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestMatchMultiview pins which requests reach the panel. The parameter has to
// be bare: "?multiview=" is an ordinary empty-valued parameter that belongs to
// the origin, and treating it as ours would quietly shadow a real one.
func TestMatchMultiview(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"/?multiview", true},
		{"/anything?multiview", true},
		{"/?a=1&multiview", true},
		{"/?multiview&a=1", true},
		{"/", false},
		{"/?0", false},            // a routing index, not the panel
		{"/?multiview=", false},   // valued: the origin's parameter
		{"/?multiview=1", false},  // valued
		{"/?multiviews", false},   // a different name
		{"/?notmultiview", false}, // substring, not a segment
		{"/?a=multiview", false},  // a value that happens to match
	}

	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if got := matchMultiview(r); got != tc.want {
				t.Errorf("matchMultiview(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

// TestWantsMultiview pins that the panel needs both the flag and something to
// compare. One origin framed alone is a worse view of it than the origin
// itself, so a lone --url keeps opening the origin.
func TestWantsMultiview(t *testing.T) {
	one, err := parseOrigins([]string{"http://localhost:3000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}
	two, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	cases := []struct {
		name    string
		enabled bool
		origins []*url.URL
		want    bool
	}{
		{"on, two origins", true, two, true},
		{"on, one origin", true, one, false},
		{"off, two origins", false, two, false},
		{"on, no origins", true, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsMultiview(tc.enabled, tc.origins); got != tc.want {
				t.Errorf("wantsMultiview() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMultiviewURL pins the panel's address and that it leaves the tunnel URL
// alone, since every origin address is derived from the same value.
func TestMultiviewURL(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := multiviewURL(public), "https://foo.tunneled.pizza/?multiview"; got != want {
		t.Errorf("multiviewURL() = %q, want %q", got, want)
	}
	if public.RawQuery != "" {
		t.Errorf("multiviewURL mutated its argument: RawQuery = %q, want empty", public.RawQuery)
	}
}

// TestServeShell renders the panel and pins what the page has to carry: a tile
// per origin, each addressed by its routing index, and the host the visitor
// actually used rather than one baked in at mint time.
func TestServeShell(t *testing.T) {
	origins, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000", "http://localhost:5000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/?multiview", nil)
	r.Host = "foo.tunneled.pizza"
	serveShell(rec, r, origins, slog.New(slog.DiscardHandler))

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
		"--rows:2", // ceil(3/2): the flex layout cannot count its own rows
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page does not contain %q", want)
		}
	}
	if got, want := strings.Count(body, "<iframe"), len(origins); got != want {
		t.Errorf("page has %d frames, want one per origin (%d)", got, want)
	}
}

// TestServeShellRowsAreCeilHalf pins the row count the layout is given, since
// getting it wrong is invisible in the markup and wrong only on screen.
func TestServeShellRowsAreCeilHalf(t *testing.T) {
	cases := map[int]string{1: "--rows:1", 2: "--rows:1", 3: "--rows:2", 4: "--rows:2", 5: "--rows:3"}

	for n, want := range cases {
		raw := make([]string, n)
		for i := range raw {
			raw[i] = "http://localhost:3000"
		}
		origins, err := parseOrigins(raw)
		if err != nil {
			t.Fatalf("parseOrigins: %v", err)
		}

		rec := httptest.NewRecorder()
		serveShell(rec, httptest.NewRequest(http.MethodGet, "/?multiview", nil), origins, slog.New(slog.DiscardHandler))
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%d origins: page does not carry %q", n, want)
		}
	}
}

// TestServeShellEscapesTheHost pins that the one value taken from the request
// is escaped. Host is attacker-controlled -- anyone can send whatever Host
// header they like -- and it lands in an HTML document, which is the shape of
// bug that has already cost this repo one CodeQL alert.
func TestServeShellEscapesTheHost(t *testing.T) {
	origins, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/?multiview", nil)
	r.Host = `evil"><script>alert(1)</script>`
	serveShell(rec, r, origins, slog.New(slog.DiscardHandler))

	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Error("the Host header reached the page as markup, want it escaped")
	}
}

// TestMultiviewInterceptorServesTheShell pins the wiring: the interceptor
// matches the panel's parameter and replaces the handler that would otherwise
// proxy the request to an origin.
func TestMultiviewInterceptorServesTheShell(t *testing.T) {
	origins, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	interceptor := multiview(origins, slog.New(slog.DiscardHandler))
	if interceptor.Priority != 1 {
		t.Errorf("Priority = %d, want 1 so nothing later can shadow the panel", interceptor.Priority)
	}
	if !interceptor.Match(httptest.NewRequest(http.MethodGet, "/?multiview", nil)) {
		t.Error("interceptor does not match its own parameter")
	}
	if interceptor.Match(httptest.NewRequest(http.MethodGet, "/?1", nil)) {
		t.Error("interceptor matches a routing index, which belongs to an origin")
	}
}
