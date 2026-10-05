// The tests for run.go. Most of what a run does is driven through the builder,
// against its fakes, in TestRun; what is pinned here is this package's own.
package run

import (
	"net/url"
	"slices"
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
		if got := publicURL(public, tc.i, tc.n); got != tc.want {
			t.Errorf("%s: publicURL(_, %d, %d) = %q, want %q", tc.name, tc.i, tc.n, got, tc.want)
		}
	}
	if public.RawQuery != "" {
		t.Errorf("publicURL mutated its argument: RawQuery = %q, want empty", public.RawQuery)
	}
}

// TestSameSpec pins what counts as the spec a tunnel already has: the same
// JSON, however it is spaced or its keys ordered, with numbers compared as
// written — and anything that is not JSON compared as it is.
func TestSameSpec(t *testing.T) {
	const saved = `{"backend":"cloudflare","spec":{"hostname":"0tk.tunneled.pizza","secret":"c2VjcmV0","port":7844}}`
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"byte for byte", saved, saved, true},
		{"keys reordered", saved, `{"spec":{"port":7844,"secret":"c2VjcmV0","hostname":"0tk.tunneled.pizza"},"backend":"cloudflare"}`, true},
		{"spaced out", saved, "{ \"backend\" : \"cloudflare\",\n \"spec\": {\"hostname\":\"0tk.tunneled.pizza\", \"secret\":\"c2VjcmV0\", \"port\": 7844} }\n", true},
		{"another secret", saved, `{"backend":"cloudflare","spec":{"hostname":"0tk.tunneled.pizza","secret":"b3RoZXI=","port":7844}}`, false},
		{"a key missing", saved, `{"backend":"cloudflare","spec":{"hostname":"0tk.tunneled.pizza","secret":"c2VjcmV0"}}`, false},
		{"a number as written", `{"n":1}`, `{"n":1.0}`, false},
		{"numbers past a float's precision", `{"n":9007199254740993}`, `{"n":9007199254740992}`, false},
		{"empty against a spec", "", saved, false},
		{"not JSON, equal", "patched", "patched", true},
		{"not JSON, different", "patched", "other", false},
		{"two values are not one spec", saved + saved, saved, false},
	} {
		if got := sameSpec(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameSpec = %v, want %v", tc.name, got, tc.want)
		}
		if got := sameSpec(tc.b, tc.a); got != tc.want {
			t.Errorf("%s, swapped: sameSpec = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMessagesOnly pins what is a change to the messages alone, which the
// run applies live, and what is a new tunnel: every other field is compared
// as JSON, so a reformatted envelope with the same messages is neither.
func TestMessagesOnly(t *testing.T) {
	const saved = `{"backend":"cloudflare","hostname":"0tk.tunneled.pizza","spec":{"hostname":"0tk.tunneled.pizza","secret":"c2VjcmV0"},"metadata":{"record_id":"r1"},"messages":["warn","tip"]}`
	for _, tc := range []struct {
		name, next string
		want       bool
		messages   []string
	}{
		{"the warning dropped", `{"backend":"cloudflare","hostname":"0tk.tunneled.pizza","spec":{"hostname":"0tk.tunneled.pizza","secret":"c2VjcmV0"},"metadata":{"record_id":"r1"},"messages":["tip"]}`, true, []string{"tip"}},
		{"all messages gone", `{"backend":"cloudflare","hostname":"0tk.tunneled.pizza","spec":{"secret":"c2VjcmV0","hostname":"0tk.tunneled.pizza"},"metadata":{"record_id":"r1"}}`, true, nil},
		{"reordered, same messages", `{"messages":["warn","tip"],"metadata":{"record_id":"r1"},"spec":{"secret":"c2VjcmV0","hostname":"0tk.tunneled.pizza"},"hostname":"0tk.tunneled.pizza","backend":"cloudflare"}`, false, nil},
		{"another secret", `{"backend":"cloudflare","hostname":"0tk.tunneled.pizza","spec":{"hostname":"0tk.tunneled.pizza","secret":"b3RoZXI="},"metadata":{"record_id":"r1"},"messages":["tip"]}`, false, nil},
		{"another record", `{"backend":"cloudflare","hostname":"0tk.tunneled.pizza","spec":{"hostname":"0tk.tunneled.pizza","secret":"c2VjcmV0"},"metadata":{"record_id":"r2"},"messages":["tip"]}`, false, nil},
		{"another backend", `{"backend":"other","hostname":"0tk.tunneled.pizza","spec":{"hostname":"0tk.tunneled.pizza","secret":"c2VjcmV0"},"metadata":{"record_id":"r1"},"messages":["tip"]}`, false, nil},
		{"not JSON", "patched", false, nil},
	} {
		got, ok := messagesOnly(tc.next, saved)
		if ok != tc.want || ok && !slices.Equal(got, tc.messages) {
			t.Errorf("%s: messagesOnly = %q, %v; want %q, %v", tc.name, got, ok, tc.messages, tc.want)
		}
	}
}
