package v1alpha1

import (
	"net/url"
	"testing"
)

// TestPublicURL pins the routing contract: with more than one origin every
// address carries a bare ?i, the parameter the tunnel's proxy consumes — the
// default origin included, since a plain URL routes by referer and cookie and
// so stops reaching origin 0 once a browser has visited ?1. A valued parameter
// ("?1=x") would be application data and route nowhere, so the bareness is
// half the assertion and the explicit ?0 is the other half.
//
// A lone origin has nothing to route between and gets the plain URL. The
// tunnel URL itself must survive unmodified either way, since every later call
// derives from it.
func TestPublicURL(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cases := []struct {
		name string
		i, n int
		want string
	}{
		{"lone origin is plain", 0, 1, "https://foo.tunneled.pizza/"},
		{"default origin is explicit when it can be confused", 0, 2, "https://foo.tunneled.pizza/?0"},
		{"second origin", 1, 2, "https://foo.tunneled.pizza/?1"},
		{"double digits", 12, 13, "https://foo.tunneled.pizza/?12"},
	}
	for _, tc := range cases {
		if got := PublicURL(public, tc.i, tc.n); got != tc.want {
			t.Errorf("%s: PublicURL(_, %d, %d) = %q, want %q", tc.name, tc.i, tc.n, got, tc.want)
		}
	}
	if public.RawQuery != "" {
		t.Errorf("PublicURL mutated its argument: RawQuery = %q, want empty", public.RawQuery)
	}
}
