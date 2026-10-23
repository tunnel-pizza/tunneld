package origins

import (
	"net/url"
	"os"
	"reflect"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// The contract this implements, asserted where the implementation is.
var _ v1.Origins = (*OriginsImpl)(nil)

// TestMain clears the sessions Key reads, so a suite run inside an agent pins
// the keys a run outside one gets.
func TestMain(m *testing.M) {
	for _, env := range sessionEnv {
		os.Unsetenv(env)
	}
	os.Exit(m.Run())
}

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

// TestAllCannotBeUsedToReorderTheRun pins the defensive copy. Key sorts, and
// a caller that sorted the slice it was handed would reorder the routing
// parameters as a side effect of asking what they are.
func TestAllCannotBeUsedToReorderTheRun(t *testing.T) {
	a, b := must(t, "http://localhost:3000"), must(t, "http://localhost:3001")
	o := New(WithURL(a, b))

	got := o.All()
	got[0], got[1] = got[1], got[0]

	first, second := o.At(0).URI, o.At(1).URI
	if first != *a || second != *b {
		t.Errorf("At(0), At(1) = %v, %v, want %v, %v — All handed out the list itself", &first, &second, a, b)
	}
	if o.Len() != 2 {
		t.Errorf("Len() = %d, want 2", o.Len())
	}
}

// TestKeyIncludesAProgramsArguments pins #182: claude --resume ABC and claude
// --resume DEF are two sessions, so they are two tunnels. The arguments ride
// the origin as a query, in order, and the whole of it is in the key — the
// same as an http origin's query, which was never set aside.
func TestKeyIncludesAProgramsArguments(t *testing.T) {
	const dir = "/work/project"
	key := func(raw string) string { return New(WithDir(dir), WithURL(must(t, raw))).Key() }

	bare := key("exec:///usr/bin/claude")
	resume := key("exec:///usr/bin/claude?arg=--resume")
	abc := key("exec:///usr/bin/claude?arg=--resume&arg=ABC")
	def := key("exec:///usr/bin/claude?arg=--resume&arg=DEF")
	if bare == resume {
		t.Errorf("Key() = %q with and without --resume, want two tunnels", bare)
	}
	if abc == def {
		t.Errorf("Key() = %q for --resume ABC and --resume DEF, want two tunnels", abc)
	}
	if again := key("exec:///usr/bin/claude?arg=--resume&arg=ABC"); again != abc {
		t.Errorf("Key() = %q then %q for the same invocation", abc, again)
	}

	plain := key("http://localhost:3000")
	queried := key("http://localhost:3000?arg=x")
	if plain == queried {
		t.Errorf("Key() = %q for an http origin with and without a query, want two", plain)
	}
}

// TestWebSocket pins the index that says which origin owns WebSockets: none
// until one is named, the one named, and none for an index the list does not
// reach — a list rebuilt shorter must not route sockets past its end. The
// marker is routing configuration and not part of an address, so naming it
// leaves the key alone.
func TestWebSocket(t *testing.T) {
	a, b := must(t, "http://localhost:3000"), must(t, "http://localhost:5173")
	for _, tc := range []struct {
		name   string
		opts   []Option
		want   int
		wantOK bool
	}{
		{"none named", []Option{WithURL(a, b)}, -1, false},
		{"the second", []Option{WithURL(a, b), WithWebSocket(1)}, 1, true},
		{"the first", []Option{WithURL(a, b), WithWebSocket(0)}, 0, true},
		{"a later call replaces", []Option{WithURL(a, b), WithWebSocket(0), WithWebSocket(1)}, 1, true},
		{"past the end", []Option{WithURL(a), WithWebSocket(1)}, -1, false},
		{"negative", []Option{WithURL(a, b), WithWebSocket(-3)}, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := New(tc.opts...).WebSocket()
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("WebSocket() = %d, %v, want %d, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}

	plain := New(WithDir("/work"), WithURL(a, b)).Key()
	if marked := New(WithDir("/work"), WithURL(a, b), WithWebSocket(1)).Key(); marked != plain {
		t.Errorf("Key() = %q marked and %q not, want one tunnel", marked, plain)
	}
}

// TestKeyIsScopedToTheSession pins that a run an agent host started is scoped
// to its session: two sessions in one directory are two tunnels, the same
// session again is the same one, and none is the key a run outside one gets.
func TestKeyIsScopedToTheSession(t *testing.T) {
	o := New(WithDir("/work"), WithURL(must(t, "http://localhost:3000")))
	outside := o.Key()

	for _, env := range sessionEnv {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "3f2c")
			first := o.Key()
			if first == outside {
				t.Errorf("Key() = %q in session 3f2c and outside any", first)
			}
			if again := o.Key(); again != first {
				t.Errorf("Key() = %q then %q for the same session", first, again)
			}
			t.Setenv(env, "9a71")
			if other := o.Key(); other == first {
				t.Errorf("Key() = %q in sessions 3f2c and 9a71", other)
			}
			t.Setenv(env, "")
			if none := o.Key(); none != outside {
				t.Errorf("Key() = %q with %s empty, want %q", none, env, outside)
			}
		})
	}
}

// TestKeyIsPinned pins Key to the values it had before origins became
// v1.Origin: the key names a run's cache file, so a change here silently
// starts every tunnel over on a new hostname.
func TestKeyIsPinned(t *testing.T) {
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	for _, tc := range []struct {
		urls []string
		want string
	}{
		{[]string{"http://localhost:3000"}, "bf28bd8b3e4167f2"},
		{[]string{"exec:///bin/zsh"}, "986d9abba66a86e9"},
		{[]string{"http://localhost:3000", "exec:///usr/bin/claude?arg=--resume&arg=b2e1"}, "4303f5452b48b7f8"},
		{[]string{"attach://dockerd/web", "https://example.test/app"}, "5a72d67e085224bb"},
		{nil, "5d289ab3ce202563"},
	} {
		var opts []Option
		for _, raw := range tc.urls {
			opts = append(opts, WithURL(parse(raw)))
		}
		if got := New(append(opts, WithDir("/srv/project"))...).Key(); got != tc.want {
			t.Errorf("Key(%v) = %q, want %q", tc.urls, got, tc.want)
		}
	}
}

// TestOriginsAreParsedOnce pins what an origin is, worked out once from the
// URL it was settled as: its index, the URL itself, the scheme it counts as
// (https is http), its provider, its reference and an exec origin's
// arguments. All is a copy: changing it changes nothing in the run.
func TestOriginsAreParsedOnce(t *testing.T) {
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	raws := []string{
		"http://localhost:3000",
		"exec:///usr/bin/claude?arg=--resume&arg=b2e1",
		"attach://dockerd/web",
		"https://example.test/app",
		"exec://dockerd/api",
	}
	var opts []Option
	for _, raw := range raws {
		opts = append(opts, WithURL(parse(raw)))
	}
	o := New(opts...)
	want := []v1.Origin{
		{Index: 0, URI: *parse(raws[0]), Scheme: v1.HttpScheme, Ref: "localhost:3000"},
		{Index: 1, URI: *parse(raws[1]), Scheme: v1.ExecScheme, Ref: "/usr/bin/claude", Args: []string{"--resume", "b2e1"}},
		{Index: 2, URI: *parse(raws[2]), Scheme: v1.AttachScheme, Provider: v1.DockerProvider, Ref: "web"},
		{Index: 3, URI: *parse(raws[3]), Scheme: v1.HttpScheme, Ref: "example.test/app"},
		{Index: 4, URI: *parse(raws[4]), Scheme: v1.ExecScheme, Provider: v1.DockerProvider, Ref: "api"},
	}
	got := o.All()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("All() =\n%+v\nwant\n%+v", got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(o.At(i), want[i]) {
			t.Errorf("At(%d) = %+v, want %+v", i, o.At(i), want[i])
		}
	}
	got[1].Args[0] = "changed"
	got[0].URI.Host = "changed"
	if again := o.All(); again[1].Args[0] != "--resume" || again[0].URI.Host != "localhost:3000" {
		t.Errorf("All() handed out the run's own origins: %+v", again)
	}
}

// TestFrom pins From: an address on the tunnel names an origin the way the
// router routes it, by the first bare numeric parameter, none meaning the
// first; anything that names no origin is an error.
func TestFrom(t *testing.T) {
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	two := New(WithURL(parse("http://localhost:3000"), parse("exec:///bin/zsh")))
	one := New(WithURL(parse("exec:///bin/zsh")))
	for _, tc := range []struct {
		name   string
		of     *OriginsImpl
		url    string
		index  int
		failed bool
	}{
		{"no parameter is the first", two, "https://0tabc.tunneld.domain/", 0, false},
		{"a bare index", two, "https://0tabc.tunneld.domain/?1", 1, false},
		{"the first bare index wins", two, "https://0tabc.tunneld.domain/app?x=1&1&0", 1, false},
		{"a valued parameter is not an index", two, "https://0tabc.tunneld.domain/?1=x", 0, false},
		{"a single origin needs none", one, "https://0tabc.tunneld.domain", 0, false},
		{"past the list", two, "https://0tabc.tunneld.domain/?2", 0, true},
		{"negative", two, "https://0tabc.tunneld.domain/?-1", 0, true},
		{"not a URL", two, "https://%zz", 0, true},
		{"not an address", two, "exec:///bin/zsh", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.of.From(tc.url)
			if (err != nil) != tc.failed {
				t.Fatalf("From(%q) = %+v, %v; want failed=%v", tc.url, got, err, tc.failed)
			}
			if !tc.failed && !reflect.DeepEqual(got, tc.of.At(tc.index)) {
				t.Errorf("From(%q) = %+v, want origin %d", tc.url, got, tc.index)
			}
		})
	}
}

// TestAdd pins that what Add appends lands after what is there, each renumbered
// to its place, and that the caller's copy is left as it was.
func TestAdd(t *testing.T) {
	o := New(WithURL(must(t, "http://localhost:3000")))
	shell := New(WithURL(must(t, "exec:///bin/sh"))).All()
	o.Add(shell...)
	all := o.All()
	if len(all) != 2 || all[1].URI.String() != "exec:///bin/sh" || all[1].Index != 1 || all[1].Scheme != v1.ExecScheme {
		t.Fatalf("All() = %+v, want :3000 then /bin/sh at index 1", all)
	}
	if shell[0].Index != 0 {
		t.Errorf("Add renumbered the caller's origin: index %d", shell[0].Index)
	}
	empty := New()
	empty.Add(shell...)
	if empty.Len() != 1 || empty.At(0).Index != 0 {
		t.Errorf("Add to an empty list = %+v, want /bin/sh at index 0", empty.All())
	}
}
