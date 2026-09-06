package v1alpha1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cnuss/libtunnel"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
	"github.com/tunnel-pizza/tunneld/v1alpha1/panel"
)

// TestParseOriginsAccepts covers the shapes a caller is allowed to type,
// including the bare host:port that implies http — the affordance that lets
// `--url localhost:3000` work the way people expect.
func TestParseOriginsAccepts(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"explicit http", []string{"http://localhost:3000"}, []string{"http://localhost:3000"}},
		{"https origin", []string{"https://127.0.0.1:8443"}, []string{"https://127.0.0.1:8443"}},
		{"bare host:port implies http", []string{"localhost:3000"}, []string{"http://localhost:3000"}},
		{"bare host implies http", []string{"localhost"}, []string{"http://localhost"}},
		{"bare port implies localhost", []string{":8000"}, []string{"http://localhost:8000"}},
		{"bare port keeps an explicit scheme", []string{"https://:8443"}, []string{"https://localhost:8443"}},
		{"scheme and bare port", []string{"http://:8000"}, []string{"http://localhost:8000"}},
		{"bare port with a path", []string{":8000/api"}, []string{"http://localhost:8000/api"}},
		{"surrounding space trimmed", []string{"  http://localhost:3000  "}, []string{"http://localhost:3000"}},
		{"path preserved", []string{"http://localhost:3000/api"}, []string{"http://localhost:3000/api"}},
		{
			"order preserved across origins",
			[]string{"http://localhost:3000", "http://localhost:4000"},
			[]string{"http://localhost:3000", "http://localhost:4000"},
		},
		{"a container by name", []string{"dockerd://api"}, []string{"dockerd://api"}},
		{"a container by id", []string{"dockerd://3f2a1b9c8d7e"}, []string{"dockerd://3f2a1b9c8d7e"}},
		{"a container name keeps case and underscores", []string{"dockerd://My_Container"}, []string{"dockerd://My_Container"}},
		{"a websocket-owning origin keeps its marker", []string{"http://localhost:4000", "http+ws://localhost:5173"}, []string{"http://localhost:4000", "http+ws://localhost:5173"}},
		{"the marker on https", []string{"http://localhost:4000", "https+wss://localhost:5173"}, []string{"http://localhost:4000", "https+wss://localhost:5173"}},
		{"ws and wss are interchangeable", []string{"http://localhost:4000", "http+wss://localhost:5173"}, []string{"http://localhost:4000", "http+wss://localhost:5173"}},
		{"a marked origin keeps the bare-port shorthand", []string{"http://localhost:4000", "http+ws://:5173"}, []string{"http://localhost:4000", "http+ws://localhost:5173"}},
		{
			"a container beside an http origin, in order",
			[]string{"http://localhost:3000", "dockerd://api"},
			[]string{"http://localhost:3000", "dockerd://api"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOrigins(tc.in)
			if err != nil {
				t.Fatalf("parseOrigins(%q) = error %v, want ok", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseOrigins(%q) returned %d origins, want %d", tc.in, len(got), len(tc.want))
			}
			for i, u := range got {
				if u.String() != tc.want[i] {
					t.Errorf("origin %d = %q, want %q", i, u, tc.want[i])
				}
			}
		})
	}
}

// TestParseOriginsRejects pins the failure modes as errors rather than as a
// public hostname that answers only errors. Each case asserts both the sentinel
// (so callers can branch on the class) and that the message names the offending
// input (so an operator can act on it).
func TestParseOriginsRejects(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    error
		mention string
	}{
		{"no origins at all", nil, v1.ErrNoOrigin, ""},
		{"empty value", []string{""}, v1.ErrNoOrigin, ""},
		{"whitespace only", []string{"   "}, v1.ErrNoOrigin, ""},
		{"unproxyable scheme", []string{"ftp://localhost:21"}, v1.ErrInvalidOrigin, "ftp"},
		{"scheme with no host", []string{"http://"}, v1.ErrInvalidOrigin, "http://"},
		{"one bad origin among good ones", []string{"http://localhost:3000", "ftp://localhost:21"}, v1.ErrInvalidOrigin, "ftp"},
		{"two origins claiming the websockets", []string{"http+ws://localhost:4000", "http+ws://localhost:5173"}, v1.ErrInvalidOrigin, "http+ws://localhost:5173"},
		{"the marker on a container", []string{"dockerd+ws://api"}, v1.ErrInvalidOrigin, "dockerd+ws"},
		{"the marker on an unproxyable scheme", []string{"ftp+ws://localhost:21"}, v1.ErrInvalidOrigin, "ftp"},
		{"a container with no name", []string{"dockerd://"}, v1.ErrInvalidOrigin, "dockerd://"},
		{"a container with a path", []string{"dockerd://api/sh"}, v1.ErrInvalidOrigin, "dockerd://api"},
		{"a container with a query", []string{"dockerd://api?tty=1"}, v1.ErrInvalidOrigin, "dockerd://api"},
		{"a container with a fragment", []string{"dockerd://api#sh"}, v1.ErrInvalidOrigin, "dockerd://api"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOrigins(tc.in)
			if err == nil {
				t.Fatalf("parseOrigins(%q) = %v, want an error", tc.in, got)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if tc.mention != "" && !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not name %q", err, tc.mention)
			}
		})
	}
}

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

// TestReportWritesOnlyToStderr pins the output contract: a running tunnel
// writes its addresses to stderr and nothing at all to stdout.
//
// stdout used to carry one bare URL per origin as a machine interface. It
// meant every address printed twice wherever both streams landed together,
// and the de-duplication meant to hide that could only recognise one file
// descriptor being literally the other — which a container's two pipes are
// not, so it never fired there. The map says which origin each address
// reaches, which the bare lines never did.
func TestReportWritesOnlyToStderr(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	origins, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	var stderr bytes.Buffer
	report(&stderr, public, origins, "")

	for _, want := range []string{
		"  https://foo.tunneled.pizza/?0\n    -> http://localhost:3000\n",
		"  https://foo.tunneled.pizza/?1\n    -> http://localhost:4000\n",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", stderr.String(), want)
		}
	}

	// Every address appears once: in the map, and nowhere else.
	for _, addr := range []string{"https://foo.tunneled.pizza/?0", "https://foo.tunneled.pizza/?1"} {
		if got := strings.Count(stderr.String(), addr); got != 1 {
			t.Errorf("%s appears %d times, want 1:\n%s", addr, got, stderr.String())
		}
	}
}

// TestLogger covers the level resolution: the --log-level value wins, the
// environment mirror is the fallback, and neither being set means silence. The
// flag is strict (an operator typo must not vanish) while the environment is
// lenient, matching what the underlying library does with its own knob.
func TestLogger(t *testing.T) {
	cases := []struct {
		name     string
		level    string
		env      string
		wantErr  error
		enabled  slog.Level
		disabled bool // the level above must NOT be enabled
	}{
		{name: "unset is silent", enabled: slog.LevelError, disabled: true},
		{name: "flag sets the level", level: "debug", enabled: slog.LevelDebug},
		{name: "environment is the fallback", env: "debug", enabled: slog.LevelDebug},
		{name: "flag beats environment", level: "error", env: "debug", enabled: slog.LevelInfo, disabled: true},
		{name: "unparsable environment reads as info", env: "loud", enabled: slog.LevelInfo},
		{name: "unparsable flag is an error", level: "loud", wantErr: v1.ErrInvalidLogLevel},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.LogEnv, tc.env)
			b := New()
			b.logLevel = tc.level

			log, err := b.logger()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("logger() = error %v, want ok", err)
			}
			if got := log.Enabled(t.Context(), tc.enabled); got == tc.disabled {
				t.Errorf("Enabled(%v) = %v, want %v", tc.enabled, got, !tc.disabled)
			}
		})
	}
}

// TestReportNamesTheMultiviewPanel pins that the panel's own address is what
// stderr leads with when there is one: it answers for every origin at once, so
// the per-origin addresses become the indented list beneath it.
func TestReportNamesTheMultiviewPanel(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	origins, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	var stderr bytes.Buffer
	report(&stderr, public, origins, panel.New().URL(public))

	if !strings.Contains(stderr.String(), "https://foo.tunneled.pizza/\n") {
		t.Errorf("stderr %q does not name the panel", stderr.String())
	}
	for _, origin := range []string{"-> http://localhost:3000", "-> http://localhost:4000"} {
		if !strings.Contains(stderr.String(), origin) {
			t.Errorf("stderr %q does not list %q under the panel", stderr.String(), origin)
		}
	}
}

// fakeTunnel is the tunnel run drives in these tests. It embeds the
// interface so only the eight methods run touches are implemented; any other
// call panics through the nil embed, which is the right outcome here.
type fakeTunnel struct {
	libtunnel.TunnelV1
	url    *url.URL
	err    error
	done   chan struct{}
	locals []*url.URL
	ics    []libtunnel.Interceptor
	listen func(libtunnel.Event)
	order  *[]string
}

// live is a tunnel that comes up on public and stays up until the test says
// otherwise; dead is one that fails before it has a URL.
func live(public string) *fakeTunnel {
	u, err := url.Parse(public)
	if err != nil {
		panic(err)
	}
	return &fakeTunnel{url: u, done: make(chan struct{})}
}

func dead(cause error) *fakeTunnel {
	done := make(chan struct{})
	close(done)
	return &fakeTunnel{err: cause, done: done}
}

func (f *fakeTunnel) URL() *url.URL {
	if f.order != nil {
		*f.order = append(*f.order, "url")
	}
	return f.url
}
func (f *fakeTunnel) Err() error                                     { return f.err }
func (f *fakeTunnel) Done() <-chan struct{}                          { return f.done }
func (f *fakeTunnel) WithLogger(*slog.Logger) libtunnel.TunnelV1     { return f }
func (f *fakeTunnel) WithContext(context.Context) libtunnel.TunnelV1 { return f }
func (f *fakeTunnel) WithEventListener(fn func(libtunnel.Event)) libtunnel.TunnelV1 {
	f.listen = fn
	return f
}
func (f *fakeTunnel) WithLocalURL(u ...*url.URL) libtunnel.TunnelV1 {
	f.locals = append(f.locals, u...)
	return f
}
func (f *fakeTunnel) WithInterceptor(ic libtunnel.Interceptor) libtunnel.TunnelV1 {
	f.ics = append(f.ics, ic)
	return f
}

// fakeEngine hands out its tunnels in order — a remint after a refused
// replay gets the second — and records what it was asked for.
type fakeEngine struct {
	tunnels   []*fakeTunnel
	specs     []string
	providers []string
}

func (f *fakeEngine) Tunnel(spec, provider string) libtunnel.TunnelV1 {
	f.specs = append(f.specs, spec)
	f.providers = append(f.providers, provider)
	tun := f.tunnels[0]
	if len(f.tunnels) > 1 {
		f.tunnels = f.tunnels[1:]
	}
	return tun
}

// fakeCache answers Cached with a fixed spec and records the rest. onSave is
// how a case ends the run: cancelling the context, failing the tunnel, or
// delivering verdicts, all after the URL is live.
type fakeCache struct {
	cached    string
	saved     bool
	discarded bool
	onSave    func()
	order     *[]string
}

func (f *fakeCache) Cached([]string, v1.Logger) string { return f.cached }
func (f *fakeCache) Discard([]string, v1.Logger)       { f.discarded = true }
func (f *fakeCache) Save([]string, v1.Logger) {
	f.saved = true
	if f.order != nil {
		*f.order = append(*f.order, "save")
	}
	if f.onSave != nil {
		f.onSave()
	}
}

// fakeOpener records what it was asked to open and opens nothing.
type fakeOpener struct {
	opened []string
	order  *[]string
}

func (f *fakeOpener) Open(_ context.Context, addr string, _ io.Writer, _ v1.Logger) {
	f.opened = append(f.opened, addr)
	if f.order != nil {
		*f.order = append(*f.order, "open")
	}
}

// runHarness is run with every collaborator faked except the two that are
// pure: the real panel, because its URL and interceptor order are what the
// assertions check, and a real counter armed at one, because the verdict
// logic is what the gone case is about. order records the effects that
// matter in the sequence they landed.
type runHarness struct {
	engine *fakeEngine
	cache  *fakeCache
	opener *fakeOpener
	order  []string
	stderr bytes.Buffer
	b      *BuilderImpl
}

func newRunHarness(t *testing.T, tun *fakeTunnel, urls ...string) *runHarness {
	t.Helper()
	t.Setenv(v1.LogEnv, "") // a developer's shell must not turn the log on
	h := &runHarness{engine: &fakeEngine{tunnels: []*fakeTunnel{tun}}}
	h.cache = &fakeCache{order: &h.order}
	h.opener = &fakeOpener{order: &h.order}
	tun.order = &h.order
	h.b = New(
		WithURL(urls...),
		WithProvider("example.test"),
		WithCacheDir(t.TempDir()), // run consults the cache only with a directory
		WithEngine(h.engine),
		WithCache(h.cache),
		WithOpener(h.opener),
		WithTargets(&stubTargets{}),
		WithCounter(counter.New(counter.WithMaxGone(1))),
	)
	return h
}

// run drives the builder under the test's own context, for the cases that
// end on their own — a tunnel that fails, a verdict, a refused builder. A
// case that has to be signalled makes its own cancelable context and hands
// cancel to onSave.
func (h *runHarness) run(t *testing.T) error {
	t.Helper()
	return h.b.run(t.Context(), &h.stderr)
}

// TestRun is the composition under test: every effect run has, against fakes
// for the edge, the disk, the browser and the daemon. Each case asserts what
// the code did before it was split, so a case going red is a behaviour
// change, not a refactor.
func TestRun(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"

	t.Run("a mint reports, opens the panel, then saves", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", ":4000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel // the signal, arriving once the tunnel is live

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v, want nil after a signal", err)
		}
		if want := []string{""}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for specs %q, want a single mint", h.engine.specs)
		}
		if want := []string{"example.test"}; !slices.Equal(h.engine.providers, want) {
			t.Errorf("engine was handed providers %q, want %q — --provider did not reach the mint", h.engine.providers, want)
		}
		if want := []string{"url", "open", "save"}; !slices.Equal(h.order, want) {
			t.Errorf("effects in order %v, want %v — the cache must not be written before the URL is live", h.order, want)
		}
		if want := []string{public}; !slices.Equal(h.opener.opened, want) {
			t.Errorf("opened %q, want the panel address %q", h.opener.opened, want)
		}
		for _, want := range []string{"  " + public + "\n", "    -> http://localhost:3000\n", "    -> http://localhost:4000\n"} {
			if !strings.Contains(h.stderr.String(), want) {
				t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
			}
		}
		tun := h.engine.tunnels[0]
		if len(tun.ics) != 2 || tun.ics[0].Priority >= tun.ics[1].Priority {
			t.Errorf("registered %d interceptors, want the shell then the unframer", len(tun.ics))
		}
		if len(tun.locals) != 2 {
			t.Errorf("tunnel was given %d origins, want 2", len(tun.locals))
		}
	})

	t.Run("a cached spec is what the engine replays", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.cache.cached = "cached-spec"
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if want := []string{"cached-spec"}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for %q, want the cached spec", h.engine.specs)
		}
	})

	t.Run("a replay the edge refuses is discarded and minted again", func(t *testing.T) {
		refused := dead(fmt.Errorf("edge: %w", libtunnel.ErrCredentialRejected))
		h := newRunHarness(t, refused, ":3000")
		h.engine.tunnels = append(h.engine.tunnels, live(public))
		h.cache.cached = "stale"
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v, want the remint to succeed", err)
		}
		if want := []string{"stale", ""}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for %q, want the replay then a mint", h.engine.specs)
		}
		if !h.cache.discarded {
			t.Error("the refused spec was not discarded; every later run would replay it")
		}
		if !h.cache.saved {
			t.Error("the remint was not cached")
		}
	})

	t.Run("any other replay failure is returned and the cache kept", func(t *testing.T) {
		boom := errors.New("provider unreachable")
		h := newRunHarness(t, dead(boom), ":3000")
		h.cache.cached = "good"

		err := h.run(t)
		if !errors.Is(err, boom) {
			t.Fatalf("run() = %v, want the tunnel's own error", err)
		}
		if h.cache.discarded {
			t.Error("a good spec was discarded over a failure that was not its fault")
		}
		if h.cache.saved {
			t.Error("a tunnel that never came up was cached")
		}
		if want := []string{"good"}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for %q, want one replay and no remint", h.engine.specs)
		}
	})

	t.Run("no URL and no cause is ErrNotReady", func(t *testing.T) {
		h := newRunHarness(t, &fakeTunnel{done: make(chan struct{})}, ":3000")
		err := h.run(t)
		if !errors.Is(err, v1.ErrNotReady) {
			t.Errorf("run() = %v, want ErrNotReady", err)
		}
	})

	t.Run("--no-open opens nothing", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", ":4000")
		h.b.noOpen = true // what the flag binds over
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if len(h.opener.opened) != 0 {
			t.Errorf("opened %q, want nothing", h.opener.opened)
		}
		if want := []string{"url", "save"}; !slices.Equal(h.order, want) {
			t.Errorf("effects %v, want %v", h.order, want)
		}
	})

	t.Run("one origin keeps the bare address and no panel", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.b.run(ctx, &h.stderr); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if want := []string{public}; !slices.Equal(h.opener.opened, want) {
			t.Errorf("opened %q, want the plain URL %q", h.opener.opened, want)
		}
		if n := len(h.engine.tunnels[0].ics); n != 0 {
			t.Errorf("registered %d interceptors for one origin, want none", n)
		}
		if want := "  " + public + "\n    -> http://localhost:3000\n"; !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
		}
	})

	t.Run("a tunnel that fails after coming up returns its error", func(t *testing.T) {
		tun := live(public)
		h := newRunHarness(t, tun, ":3000")
		gone := errors.New("edge went away")
		h.cache.onSave = func() {
			tun.err = gone
			close(tun.done)
		}

		err := h.run(t)
		if !errors.Is(err, gone) {
			t.Errorf("run() = %v, want the tunnel's error", err)
		}
	})

	t.Run("enough gone verdicts end the run with ErrTunnelGone", func(t *testing.T) {
		tun := live(public)
		h := newRunHarness(t, tun, ":3000")
		h.cache.onSave = func() {
			tun.listen(libtunnel.Event{Kind: libtunnel.EventGone, Hostname: "foo.tunneled.pizza"})
		}

		err := h.run(t)
		if !errors.Is(err, v1.ErrTunnelGone) {
			t.Fatalf("run() = %v, want ErrTunnelGone", err)
		}
		if !strings.Contains(err.Error(), "foo.tunneled.pizza") {
			t.Errorf("error %q does not name the hostname", err)
		}
	})

	t.Run("a bare struct is refused before it touches anything", func(t *testing.T) {
		b := &BuilderImpl{urls: []string{":3000"}}
		err := b.run(t.Context(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "construct it with New") {
			t.Errorf("run() on a bare BuilderImpl = %v, want the wiring error", err)
		}
	})
}

// TestEvents pins the latch in the lifecycle listener. Verdicts keep arriving
// while the tunnel comes down, and without once.Do every one of them would
// repeat the error and cancel again. Driven directly, with a buffer-backed
// logger, because run's own logger writes to os.Stderr and the harness
// cannot see it — which is why TestRun's gone case cannot assert "once".
func TestEvents(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError}))
	var causes []error
	gone := func(cause error) { causes = append(causes, cause) }

	b := New(WithCounter(counter.New(counter.WithMaxGone(1))))
	listen := b.events(log, gone)
	for range 3 {
		listen(libtunnel.Event{Kind: libtunnel.EventGone, Hostname: "foo.tunneled.pizza"})
	}

	if len(causes) != 1 {
		t.Fatalf("cancelled %d times, want once", len(causes))
	}
	if !errors.Is(causes[0], v1.ErrTunnelGone) {
		t.Errorf("cause = %v, want ErrTunnelGone", causes[0])
	}
	if got := strings.Count(logged.String(), "disowned"); got != 1 {
		t.Errorf("logged the verdict %d times, want once:\n%s", got, logged.String())
	}
}
