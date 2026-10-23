package origin

import (
	"net/url"
	"testing"
)

// of is raw as an origin of scheme and ref, the way a list settles one.
func of(t *testing.T, raw string, scheme Scheme, ref string) Origin {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return Origin{URI: *u, Scheme: scheme, Ref: ref}
}

// TestURLIsACopy pins that what URL hands back is the caller's: changing it,
// credentials included, changes nothing in the origin.
func TestURLIsACopy(t *testing.T) {
	o := of(t, "http://u:p@localhost:3000/a", HttpScheme, "localhost:3000/a")
	u := o.URL()
	u.Host = "elsewhere"
	u.User = url.UserPassword("x", "y")
	if got := o.URL().String(); got != "http://u:p@localhost:3000/a" {
		t.Errorf("URL() = %q after changing a copy, want it unchanged", got)
	}
	if o.URI.User == o.URL().User {
		t.Error("URL() shares its credentials with the origin")
	}
}

// TestLabelBareName pins the three ways an origin is shown: Label as the map
// prints it, a program without its arguments; Bare as a provider is told it,
// with nothing that can hold a secret; Name as compose output prefixes it.
func TestLabelBareName(t *testing.T) {
	for _, tc := range []struct {
		origin            Origin
		label, bare, name string
	}{
		{of(t, "exec:///usr/bin/claude?arg=--resume", ExecScheme, "/usr/bin/claude"), "exec:///usr/bin/claude", "exec:///usr/bin/claude", "claude"},
		{of(t, "attach://dockerd/api", AttachScheme, "api"), "attach://dockerd/api", "attach://dockerd/api", "api"},
		{of(t, "http://localhost:3000/app?x=1", HttpScheme, "localhost:3000/app"), "http://localhost:3000/app?x=1", "http://localhost:3000/app", ":3000"},
		{of(t, "http://127.0.0.1:8080", HttpScheme, "127.0.0.1:8080"), "http://127.0.0.1:8080", "http://127.0.0.1:8080", ":8080"},
		{of(t, "https://u:p@example.test/x#f", HttpScheme, "example.test/x"), "https://u:p@example.test/x#f", "https://example.test/x", "example.test"},
	} {
		o := tc.origin
		if got := o.Label(); got != tc.label {
			t.Errorf("%s: Label() = %q, want %q", o.URI.Redacted(), got, tc.label)
		}
		if got := o.Bare(); got != tc.bare {
			t.Errorf("%s: Bare() = %q, want %q", o.URI.Redacted(), got, tc.bare)
		}
		if got := o.Name(); got != tc.name {
			t.Errorf("%s: Name() = %q, want %q", o.URI.Redacted(), got, tc.name)
		}
	}
}
