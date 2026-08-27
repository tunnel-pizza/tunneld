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
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page does not contain %q", want)
		}
	}
	if got, want := strings.Count(body, "<iframe"), len(origins); got != want {
		t.Errorf("page has %d frames, want one per origin (%d)", got, want)
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
			if got := isPanelFrame(r); got != tc.want {
				t.Errorf("isPanelFrame(dest=%q site=%q) = %v, want %v", tc.dest, tc.site, got, tc.want)
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
			if got := withoutFrameAncestors(tc.policy); got != tc.want {
				t.Errorf("withoutFrameAncestors(%q) = %q, want %q", tc.policy, got, tc.want)
			}
		})
	}
}

// TestStripFraming pins the header surgery across both spellings of the same
// refusal, including the report-only variant and an origin that sends several
// policies.
func TestStripFraming(t *testing.T) {
	h := http.Header{}
	h.Set("X-Frame-Options", "DENY")
	h.Add("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	h.Add("Content-Security-Policy", "frame-ancestors https://example.test")
	h.Set("Content-Security-Policy-Report-Only", "frame-ancestors 'none'; img-src *")
	h.Set("X-Content-Type-Options", "nosniff")

	stripFraming(h)

	if got := h.Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q, want it removed", got)
	}
	if got := h.Values("Content-Security-Policy"); len(got) != 1 || got[0] != "default-src 'self'" {
		t.Errorf("Content-Security-Policy = %q, want only the non-framing directives", got)
	}
	if got := h.Get("Content-Security-Policy-Report-Only"); got != "img-src *" {
		t.Errorf("report-only policy = %q, want only the non-framing directives", got)
	}
	// Everything else the origin sent is none of our business.
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want it untouched", got)
	}
}

// TestUnframerScrubsBeforeTheWrite pins that the scrub happens while headers
// are still mutable, including for a handler that never calls WriteHeader and
// so would otherwise have its implicit 200 written past the wrapper.
func TestUnframerScrubsBeforeTheWrite(t *testing.T) {
	t.Run("explicit WriteHeader", func(t *testing.T) {
		rec := httptest.NewRecorder()
		u := &unframer{ResponseWriter: rec}
		u.Header().Set("X-Frame-Options", "DENY")
		u.WriteHeader(http.StatusOK)

		if got := rec.Header().Get("X-Frame-Options"); got != "" {
			t.Errorf("X-Frame-Options = %q, want it removed", got)
		}
	})

	t.Run("implicit 200 via Write", func(t *testing.T) {
		rec := httptest.NewRecorder()
		u := &unframer{ResponseWriter: rec}
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
		u := &unframer{ResponseWriter: rec}
		if u.Unwrap() != http.ResponseWriter(rec) {
			t.Error("Unwrap did not return the wrapped writer, so flush and hijack would break")
		}
	})
}

// TestUnframeInterceptorIsBehindTheShell pins the ordering: the panel is
// served before anything considers framing, and the unframer never matches the
// panel's own request.
func TestUnframeInterceptorIsBehindTheShell(t *testing.T) {
	shellPriority := multiview(nil, slog.New(slog.DiscardHandler)).Priority
	if got := unframe().Priority; got <= shellPriority {
		t.Errorf("unframe Priority = %d, want it behind the shell's %d", got, shellPriority)
	}

	panel := httptest.NewRequest(http.MethodGet, "/?multiview", nil)
	if unframe().Match(panel) {
		t.Error("the unframer matched the panel request, which it does not serve")
	}
}
