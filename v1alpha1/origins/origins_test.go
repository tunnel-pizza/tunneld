package origins

import (
	"net/url"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// The contract this implements, asserted where the implementation is.
var _ v1.Origins = (*OriginsImpl)(nil)

// must parses a URL a test wrote, where a failure is the test's own bug.
func must(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u
}

// TestKeyIsTheSetAndWhereItRan pins what makes two runs the same tunnel: the
// same origins in any order, from the same directory. It is the whole of why
// this type exists — the cache files a spec under this name.
func TestKeyIsTheSetAndWhereItRan(t *testing.T) {
	const dir = "/work/project"
	a, b := must(t, "http://localhost:3000"), must(t, "exec:///usr/bin/htop")

	same := []struct {
		name string
		opts []Option
	}{
		{"as given", []Option{WithDir(dir), WithURL(a, b)}},
		{"reversed", []Option{WithDir(dir), WithURL(b, a)}},
		{"repeated", []Option{WithDir(dir), WithURL(a, b, a)}},
		{"appended across calls", []Option{WithDir(dir), WithURL(a), WithURL(b)}},
	}
	want := New(same[0].opts...).Key()
	for _, tc := range same[1:] {
		if got := New(tc.opts...).Key(); got != want {
			t.Errorf("%s: Key() = %q, want %q — the same tunnel", tc.name, got, want)
		}
	}

	differ := []struct {
		name string
		opts []Option
	}{
		{"another directory", []Option{WithDir("/work/other"), WithURL(a, b)}},
		{"one origin fewer", []Option{WithDir(dir), WithURL(a)}},
		{"another origin", []Option{WithDir(dir), WithURL(a, must(t, "http://localhost:3001"))}},
		{"no origins at all", []Option{WithDir(dir)}},
	}
	for _, tc := range differ {
		if got := New(tc.opts...).Key(); got == want {
			t.Errorf("%s: Key() = %q, want a different tunnel", tc.name, got)
		}
	}

	// Safe as a filename and stable in width, because it is one.
	if got := New(same[0].opts...).Key(); len(got) != keyWidth {
		t.Errorf("Key() = %q, %d characters, want %d", got, len(got), keyWidth)
	}
}

// TestNewSeedsTheWorkingDirectory pins that the default identity is where the
// process is, so a run that configures nothing still files its spec per
// project.
func TestNewSeedsTheWorkingDirectory(t *testing.T) {
	u := must(t, "http://localhost:3000")

	seeded := New(WithURL(u)).Key()
	if elsewhere := New(WithDir("/somewhere/else"), WithURL(u)).Key(); seeded == elsewhere {
		t.Errorf("Key() = %q from both the working directory and /somewhere/else", seeded)
	}
	if again := New(WithURL(u)).Key(); again != seeded {
		t.Errorf("Key() = %q then %q for the same run", seeded, again)
	}
}

// TestURLsCannotBeUsedToReorderTheRun pins the defensive copy. Key sorts, and
// a caller that sorted the slice it was handed would reorder the routing
// parameters as a side effect of asking what they are.
func TestURLsCannotBeUsedToReorderTheRun(t *testing.T) {
	a, b := must(t, "http://localhost:3000"), must(t, "http://localhost:3001")
	o := New(WithURL(a, b))

	got := o.URLs()
	got[0], got[1] = got[1], got[0]

	if o.At(0) != a || o.At(1) != b {
		t.Errorf("At(0), At(1) = %v, %v, want %v, %v — URLs handed out the list itself", o.At(0), o.At(1), a, b)
	}
	if o.Len() != 2 {
		t.Errorf("Len() = %d, want 2", o.Len())
	}
}
