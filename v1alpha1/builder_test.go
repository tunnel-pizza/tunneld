package v1alpha1

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnuss/libtunnel"
	ltv1 "github.com/cnuss/libtunnel/v1"
	"github.com/creack/pty"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/tunnel-pizza/tunneld/v0exp1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/display"
	"github.com/tunnel-pizza/tunneld/v1alpha1/logs"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
	"github.com/tunnel-pizza/tunneld/v1alpha1/router"
	"rsc.io/qr"
)

// execute runs a built command with args, capturing both streams. Every case
// here is an offline one — the command fails during validation, before the
// tunnel is ever dialed — so the tests never touch the network.
func execute(t *testing.T, b v1.Builder, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := b.Command()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// TestOptionsLand pins that every builder option reaches the field the flag
// binds over. Name through Name, the writers through cobra, the flag-backed
// knobs through their defaults, and the origins through the field they seed —
// origins are arguments now, so there is no flag default to read them from.
func TestOptionsLand(t *testing.T) {
	var sink bytes.Buffer
	b := New(
		WithName("expose"),
		WithOrigin("http://localhost:3000"),
		WithOrigin("http://localhost:4000"),
		WithProvider("example.test"),
		WithLogLevel("warn"),
		WithQR(true),
		WithStdout(&sink),
		WithStderr(&sink),
	)

	if got, want := b.Name(), "expose"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	cmd := b.Command()
	if got, want := cmd.Name(), "expose"; got != want {
		t.Errorf("built command Name() = %q, want %q", got, want)
	}
	if got, want := strings.Join(b.origins, ","), "http://localhost:3000,http://localhost:4000"; got != want {
		t.Errorf("seeded origins = %q, want %q — repeated options must append", got, want)
	}
	for flag, want := range map[string]string{
		"provider":  "example.test",
		"log-level": "warn",
		"qr":        "true",
	} {
		if got := cmd.Flags().Lookup(flag).DefValue; got != want {
			t.Errorf("--%s default = %q, want %q", flag, got, want)
		}
	}
	if cmd.OutOrStdout() != io.Writer(&sink) || cmd.ErrOrStderr() != io.Writer(&sink) {
		t.Error("WithStdout/WithStderr did not reach the built command")
	}
}

// TestNameDefaults pins that an unnamed builder produces the canonical command
// name, and that WithName is what an embedding program uses to mount it under
// its own verb.
func TestNameDefaults(t *testing.T) {
	if got, want := New().Name(), v1.CommandName; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := New().Command().Name(), v1.CommandName; got != want {
		t.Errorf("built command Name() = %q, want %q", got, want)
	}
}

// TestCommandIsIdempotent pins that Command assembles once. A second
// assembly would bind a second set of flags over the same fields, so the
// cached command is correctness, not just an optimization.
func TestCommandIsIdempotent(t *testing.T) {
	b := New(WithOrigin("http://localhost:3000"))
	if first, second := b.Command(), b.Command(); first != second {
		t.Error("Command() returned a different command on the second call, want the cached one")
	}
}

// TestOriginRequiredWhenUnseeded pins that a bare command refuses to run
// rather than minting a tunnel with nothing behind it, and that its message
// names both ways of supplying one — that is the choice an operator makes to
// fix it.
func TestOriginRequiredWhenUnseeded(t *testing.T) {
	// $SHELL is the last origin tried, and a developer's shell has one — so
	// without turning the fallback off the case does not assert a refusal, it
	// mints a tunnel and blocks on it. Said with the option rather than by
	// unsetting the variable, so what the case means is on the line that
	// means it.
	_, _, err := execute(t, New(WithShellFallback(false)))
	if !errors.Is(err, v1.ErrNoOrigin) {
		t.Fatalf("running with no origin = %v, want ErrNoOrigin", err)
	}
	if !strings.Contains(err.Error(), v1.OriginsEnv) {
		t.Errorf("error %q does not name %s", err, v1.OriginsEnv)
	}
}

// TestOriginOptionalWhenSeeded pins the embedding case: an origin supplied
// through WithOrigin lets the command run with no arguments at all. It gets
// past the nothing-to-expose check and fails on the deliberately bad log
// level, which is the assertion — it never reaches the network.
func TestOriginOptionalWhenSeeded(t *testing.T) {
	b := New(WithOrigin("http://localhost:3000"), WithLogLevel("loud"))
	_, _, err := execute(t, b)
	if !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel (proving the seed stood in for an argument)", err)
	}
}

// TestArgumentReplacesSeededOrigins pins that a command line overrides the
// seed wholesale instead of merging into it: an append would leave the seeded
// origin in the list behind the argument's.
//
// The flags are parsed rather than executed, which is all Origins needs to see
// argv and is what keeps the case offline.
func TestArgumentReplacesSeededOrigins(t *testing.T) {
	b := New(WithOrigin("http://seeded:1"))
	if err := b.Command().ParseFlags([]string{"http://localhost:3000"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got, want := originStrings(b.Origins()), []string{"http://localhost:3000"}; !slices.Equal(got, want) {
		t.Errorf("Origins() = %q, want %q — the seed survived an argument", got, want)
	}
}

// TestRejectsUnusableFlags pins that the validation failures reach the caller
// as their v1 sentinels, so a program embedding tunneld can branch on the
// class rather than on message text.
//
// An unusable origin is dropped rather than refused, so a run whose only
// origin was unusable arrives at the same place as one given none at all:
// ErrNoOrigin, before the mint. What it never becomes is a tunnel.
func TestRejectsUnusableFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want error
	}{
		{"unproxyable scheme", []string{"ftp://localhost:21"}, v1.ErrNoOrigin},
		{"no host", []string{"http://"}, v1.ErrNoOrigin},
		{"empty origin", []string{"  "}, v1.ErrNoOrigin},
		{"unknown log level", []string{"--log-level", "loud", "http://localhost:3000"}, v1.ErrInvalidLogLevel},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := execute(t, New(), tc.args...)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestArgumentsAreOrigins pins that bare arguments are the origins, and that
// every one of them is parsed rather than the first taken and the rest
// ignored. The second argument is the one that is not an origin, so its being
// dropped — while the first survives — is what proves the whole list was read.
func TestArgumentsAreOrigins(t *testing.T) {
	b := New()
	if err := b.Command().ParseFlags([]string{"http://localhost:3000", "ftp://nope"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got, want := originStrings(b.Origins()), []string{"http://localhost:3000"}; !slices.Equal(got, want) {
		t.Errorf("Origins() = %q, want %q", got, want)
	}
}

// TestVersionSubcommand pins that `tunneld version` reports without touching
// the network, and that the banner names both builds — a bug report needs the
// tunneld version and the tunnel library's.
func TestVersionSubcommand(t *testing.T) {
	stdout, _, err := execute(t, New(), "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	for _, want := range []string{"tunneld ", "libtunnel "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("version output %q does not contain %q", stdout, want)
		}
	}
}

// TestHelpNamesTheCommand pins that help follows WithName, so an embedded
// tunneld documents itself under the verb it was mounted as rather than under
// the binary's own name.
func TestHelpNamesTheCommand(t *testing.T) {
	stdout, _, err := execute(t, New(WithName("expose")), "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	if !strings.Contains(stdout, "expose [flags] [origin ...]") {
		t.Errorf("help %q does not use the configured command name", stdout)
	}
}

// TestBrowserOpensWhenSomebodyIsWatching covers the run's half of the display
// decision. The decision itself is display.TestOpenDecides; what is pinned
// here is that the facts reaching it are the true ones, read off the only
// thing a case can see from out here — whether a tab was actually launched.
//
// The terminal is a real pty, because whether a stream is one is exactly what
// the run reports and a buffer can never answer yes. $CI is cleared and a
// display named, since this suite runs under both conditions and one of them
// is a signal.
func TestBrowserOpensWhenSomebodyIsWatching(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	for _, tc := range []struct {
		name     string
		open     *bool
		terminal bool
		qr       bool
		want     bool
	}{
		{name: "a pipe is nobody watching", want: false},
		{name: "a terminal is somebody", terminal: true, want: true},
		{name: "a caller who declined outranks the terminal", open: ptr(false), terminal: true, want: false},
		{name: "a caller who insisted outranks the pipe", open: ptr(true), want: true},
		{name: "--qr is for a phone, so no tab on a terminal", terminal: true, qr: true, want: false},
		{name: "a caller who insisted outranks --qr", open: ptr(true), qr: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CI", "")
			t.Setenv("SSH_CONNECTION", "")
			t.Setenv("SSH_TTY", "")
			t.Setenv("DISPLAY", ":0")

			h := newRunHarness(t, live(public), ":3000")
			if tc.open != nil {
				v1.Apply(h.b, WithOpen(*tc.open))
			}
			if tc.terminal {
				ptmx, tty, err := pty.Open()
				if err != nil {
					t.Skipf("no pty to be a terminal on: %v", err)
				}
				t.Cleanup(func() { tty.Close(); ptmx.Close() })
				h.console = tty
			}
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel

			var args []string
			if tc.qr {
				args = append(args, "--qr")
			}
			if err := h.run(t, ctx, args...); err != nil {
				t.Fatalf("run() = %v", err)
			}
			if got := len(h.display.opened) > 0; got != tc.want {
				t.Errorf("opened %q, want a browser: %v", h.display.opened, tc.want)
			}
		})
	}
}

// ptr is a *bool for a literal, which When.Forced needs and Go has no spelling
// for inline.
func ptr(b bool) *bool { return &b }

func TestMultiviewDefaultsOn(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{name: "default", want: true},
		{name: "flag turns it off", args: []string{"--multiview=false"}, want: false},
		{name: "variable turns it off", env: "false", want: false},
		{name: "flag beats the variable", env: "false", args: []string{"--multiview=true"}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.MultiviewEnv, tc.env)

			b := New(WithOrigin("http://localhost:3000", "http://localhost:4000"))
			_, _, err := execute(t, b, append(append([]string{}, tc.args...), "--log-level", "loud")...)
			if !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
			}

			got, err := b.Command().Flags().GetBool("multiview")
			if err != nil {
				t.Fatalf("GetBool: %v", err)
			}
			if got != tc.want {
				t.Errorf("--multiview = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWithMultiviewSeedsTheDefault pins that an embedder can flip the default
// without forbidding the flag.
func TestWithMultiviewSeedsTheDefault(t *testing.T) {
	b := New(WithOrigin("http://localhost:3000"), WithMultiview(false))

	if got := b.Command().Flags().Lookup("multiview").DefValue; got != "false" {
		t.Errorf("--multiview default = %q, want %q", got, "false")
	}
}

// fakeTunnel is the tunnel the run drives in these tests. It embeds the
// interface so only the eight methods touched are implemented; any other
// call panics through the nil embed, which is the right outcome here.
type fakeTunnel struct {
	libtunnel.TunnelV1
	url    *url.URL
	err    error
	done   chan libtunnel.TunnelV1
	locals []*url.URL
	listen func(libtunnel.Event)
	order  *[]string
	// token is what WithToken was handed; the run applies it on every path.
	token string
	// headers is what WithHeader was handed, as "key: value", in order.
	headers []string
	// messages is what Messages hands back: what the provider said with the
	// spec, as libtunnel would carry it.
	messages []string
	// serialized, when set, is what Serialize hands back: a real envelope,
	// for a case about envelopes.
	serialized string
	// ctx is what WithContext was handed: the context the tunnel lives on.
	// Like a real tunnel, it ends when that does — unless it lingers, still
	// draining, until the test ends it.
	ctx     context.Context
	lingers bool
	ending  sync.Once
}

// live is a tunnel that comes up on public and stays up until the test says
// otherwise; dead is one that fails before it has a URL.
func live(public string) *fakeTunnel {
	u, err := url.Parse(public)
	if err != nil {
		panic(err)
	}
	return &fakeTunnel{url: u, done: make(chan libtunnel.TunnelV1, 1)}
}

func dead(cause error) *fakeTunnel {
	f := &fakeTunnel{err: cause, done: make(chan libtunnel.TunnelV1, 1)}
	f.end()
	return f
}

// end ends the tunnel the way libtunnel's lifecycle does: Done delivers the
// tunnel once and then closes, so a waiter reads it as `v, ok := <-Done()`
// and every later waiter still sees the close.
//
// The real Done hands out a fresh channel per call; this one shares a single
// channel, which is all a run needs — it holds one.
func (f *fakeTunnel) end() {
	f.ending.Do(func() {
		f.done <- f
		close(f.done)
	})
}

func (f *fakeTunnel) URL() *url.URL {
	if f.order != nil {
		*f.order = append(*f.order, "url")
	}
	// A tunnel that has a URL has been accepted by the edge, so it announces
	// the connection, the way a real one does.
	if f.url != nil && f.listen != nil {
		f.listen(libtunnel.Event{Kind: libtunnel.EventConnected, Hostname: f.url.Host})
	}
	return f.url
}

// Ready delivers the tunnel when it has one to serve on, and otherwise closes
// without delivering — the lifecycle's way of saying it never came up. A fake
// with no URL is that second case, which is what the failure paths run on.
func (f *fakeTunnel) Ready() <-chan libtunnel.TunnelV1 {
	ready := make(chan libtunnel.TunnelV1, 1)
	if f.url != nil {
		ready <- f
	}
	close(ready)
	return ready
}

// Serialize is what the run caches: a spec the next run could hand back to
// From. A fake's is anything stable and recognisable.
// Secret is a stand-in secret for the fake tunnel's hostname: nothing reads it
// but the run, which hands it to the cache.
func (f *fakeTunnel) Secret() []byte {
	if f.url == nil {
		return nil
	}
	return []byte("secret-of-" + f.url.Host)
}

func (f *fakeTunnel) Serialize() string {
	if f.serialized != "" {
		return f.serialized
	}
	if f.url == nil {
		return ""
	}
	return "spec:" + f.url.Host
}

func (f *fakeTunnel) WithToken(token string) libtunnel.TunnelV1 {
	f.token = token
	return f
}

func (f *fakeTunnel) WithHeader(key, value string) libtunnel.TunnelV1 {
	f.headers = append(f.headers, key+": "+value)
	return f
}

// Cancel ends the tunnel the way the lifecycle's does: with a cause it is a
// failure and Err reports it.
func (f *fakeTunnel) Cancel(cause ...error) {
	if len(cause) > 0 {
		f.err = cause[0]
	}
	f.end()
}

func (f *fakeTunnel) Err() error                                 { return f.err }
func (f *fakeTunnel) Done() <-chan libtunnel.TunnelV1            { return f.done }
func (f *fakeTunnel) WithLogger(*slog.Logger) libtunnel.TunnelV1 { return f }
func (f *fakeTunnel) WithContext(ctx context.Context) libtunnel.TunnelV1 {
	f.ctx = ctx
	if !f.lingers {
		go func() {
			<-ctx.Done()
			f.end()
		}()
	}
	return f
}
func (f *fakeTunnel) WithEventListener(fn func(libtunnel.Event)) libtunnel.TunnelV1 {
	f.listen = fn
	return f
}
func (f *fakeTunnel) WithLocalURL(u *url.URL) libtunnel.TunnelV1 {
	f.locals = append(f.locals, u)
	return f
}
func (f *fakeTunnel) Messages() []string { return f.messages }

// fakeCache answers Load with a fixed spec and records the rest. onSave is
// how a case ends the run: cancelling the context, failing the tunnel, or
// delivering verdicts, all after the URL is live.
type fakeCache struct {
	cached string
	saved  bool
	// spec is what the run asked the tunnel to serialize, and tracking what
	// the run said it settled on — written beside it, and nothing reads back.
	spec     string
	tracking map[string]string
	secret   []byte
	key      string
	onSave   func()
	order    *[]string
	// specs is what Spec hands out, nil for a cache that never has a new
	// spec. Like the real one, each Save lands the saved spec on it too.
	specs chan string
	// mutable is what the fake's file says; recorded is what SetMutable kept.
	mutable  map[string]string
	recorded map[string]string
}

// Mutable is what the fake's file says about name.
func (f *fakeCache) Mutable(name string) (string, bool) { v, ok := f.mutable[name]; return v, ok }

// SetMutable records what the builder settled, for a case to read back.
func (f *fakeCache) SetMutable(name, value string) {
	if f.recorded == nil {
		f.recorded = map[string]string{}
	}
	f.recorded[name] = value
}

// fakeAuth is the real auth, recording each value Set and whether the router
// had been configured yet when it was: the password must be in place before
// anything can be routed to.
type fakeAuth struct {
	*auth.AuthImpl
	router         *fakeRouter
	sets           []string
	setBeforeRoute bool
}

func (f *fakeAuth) Set(v string) error {
	if err := f.AuthImpl.Set(v); err != nil {
		return err
	}
	f.sets = append(f.sets, v)
	if f.router != nil && f.router.configured == nil {
		f.setBeforeRoute = true
	}
	return nil
}

// Spec is every spec the fake takes, as the real cache hands them on.
func (f *fakeCache) Spec() <-chan string { return f.specs }

// Grant is no grant: the fake issues none.
func (f *fakeCache) Grant(string) bool { return false }

// fakePid records a run's registration in the order of effects, and says a
// launcher was waiting when waiting is set.
type fakePid struct {
	order      *[]string
	onRegister func()
	waiting    bool
	// running is what Register refuses with: the same run already going.
	running error
	// out is the file Detach was handed to point the run's streams at, and
	// onDetach runs when it is.
	out      *os.File
	onDetach func()
}

func (f *fakePid) Register(Origins, v1.Logger) (func(), error) {
	*f.order = append(*f.order, "register")
	if f.running != nil {
		return nil, f.running
	}
	if f.onRegister != nil {
		f.onRegister()
	}
	return func() { *f.order = append(*f.order, "release") }, nil
}

func (f *fakePid) Detach(out *os.File, _ v1.Logger) bool {
	*f.order = append(*f.order, "detach")
	f.out = out
	if f.onDetach != nil {
		f.onDetach()
	}
	return f.waiting
}

func (f *fakeCache) Load(...cache.Option) string { return f.cached }

// Secret is the secret the fake was saved with, and Key the key of the
// origins it was saved under.
func (f *fakeCache) Secret() []byte { return f.secret }
func (f *fakeCache) Key() string    { return f.key }

// String is the file the fake saved, rendered as the real cache renders it,
// and Handlers the real cache's endpoints serving that file.
func (f *fakeCache) String() string { return f.real().String() }
func (f *fakeCache) Handlers(path string) map[string]func(http.ResponseWriter, *http.Request) {
	return f.real().Handlers(path)
}

// real is a cache holding what the fake saved, touching no disk.
func (f *fakeCache) real() *cache.CacheImpl {
	return cache.New(cache.WithSpec(f.spec), cache.WithTracking(f.tracking))
}
func (f *fakeCache) Save(opts ...cache.Option) {
	// The run's options, read back off a cache they configure rather than
	// one that writes.
	c := cache.New(opts...)
	f.spec = ""
	echoed := false
	select {
	case f.spec = <-c.Spec():
		echoed = true
	default:
		// WithSavedSpec: in the file, not handed on.
		if m := regexp.MustCompile(`(?m)^LIBTUNNEL_SPEC='(.*)'$`).FindStringSubmatch(c.String()); m != nil {
			f.spec = m[1]
		}
	}
	f.saved, f.tracking, f.secret, f.key = true, c.Tracking(), c.Secret(), c.Key()
	if f.specs != nil && echoed {
		select {
		case <-f.specs:
		default:
		}
		f.specs <- f.spec
	}
	if f.order != nil {
		*f.order = append(*f.order, "save")
	}
	if f.onSave != nil {
		f.onSave()
	}
}

// fakeDisplay records what it was asked to open and opens nothing. Only the
// launch is faked: the panel half of the contract is the real one, embedded,
// because the address TestRun expects reported and opened is the one the
// panel computes.
// fakeDisplay is the real display with a launcher that records instead of
// launching. It is not a stub: deciding whether to open is DisplayImpl's, and
// a stub that skipped that decision would let a case assert "nothing opened"
// while asserting nothing at all.
type fakeDisplay struct {
	*display.DisplayImpl
	opened []string
	order  *[]string
}

func (f *fakeDisplay) Open(ctx context.Context, log v1.Logger, opts ...display.Option) {
	// The recorder goes on first so the run's own options still win, and the
	// effect is recorded where it actually happens: a launch that the
	// decision declined never reaches this.
	f.DisplayImpl.Open(ctx, log, append([]display.Option{
		display.WithLaunch(func(addr string) error {
			f.opened = append(f.opened, addr)
			if f.order != nil {
				*f.order = append(*f.order, "open")
			}
			return nil
		}),
	}, opts...)...)
}

// fakeBinder stands in for the attach package: it hands the origins back
// unchanged and closes nothing, so TestRun never stands up a real listener.
type fakeBinder struct {
	err    error
	closed bool
	// asked is what a viewer's exit closes, and what Done hands back: how the
	// run learns a terminal asked it to stop.
	asked chan struct{}
	// announced is what Announce was handed last, and announcedAll every
	// address it was handed, in order.
	announced    []string
	announcedAll []string
	// onAnnounce fires when it arrives, which is after the URL is live and
	// before the cache is written — the one signal a case can end a run on
	// whether or not this run caches anything.
	onAnnounce func()
	// mirrors makes what Bind returns carry Show, which is how the real
	// binder reports a single served origin — the only shape a console can
	// draw.
	mirrors bool
	// showed records that a viewer was drawn, so a case can assert on the
	// mirror having started rather than on what it displaced. Atomic because
	// console.Show draws on a goroutine of its own.
	showed atomic.Bool
	// shows counts the frames drawn: a respec keeps the one that is up.
	shows atomic.Int32
}

func (f *fakeBinder) Bind(_ context.Context, shown Origins, _ v1.Logger) (Origins, attach.Bound, error) {
	// Carrying Mirror is how the real binder says a run has exactly one
	// served origin, so it is a wrapper here too rather than a method on the
	// binder itself: a fake that always carried it would mirror every case
	// that happens to have a terminal, and a browser would never open.
	if f.mirrors {
		return shown, mirrorableBinder{f}, f.err
	}
	return shown, f, f.err
}

// mirrorableBinder is a bound closer with a terminal to draw, which is what
// the binder hands back for a single served origin.
type mirrorableBinder struct{ *fakeBinder }

// Mirror blocks until the run ends, like the real one, so a case can assert on
// what happened while it was drawing.
func (m mirrorableBinder) Show(ctx context.Context, _ io.Reader, _ io.Writer) error {
	m.showed.Store(true)
	m.shows.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeBinder) Close() error { f.closed = true; return nil }

func (f *fakeBinder) Done() <-chan struct{} { return f.asked }

// Announce is what a bound closer is told the public addresses through. Every
// one of them can be, which is why it is on the type Bind returns rather than
// an interface a caller has to go looking for.
func (f *fakeBinder) Announce(public []string) {
	f.announced = public
	f.announcedAll = append(f.announcedAll, public...)
	if f.onAnnounce != nil {
		f.onAnnounce()
	}
}

// routed is the address fakeRouter answers with: nothing listens there, and a
// tunnel handed it is a run that asked the router rather than dialing an
// origin itself.
var routed = &url.URL{Scheme: "http", Host: "127.0.0.1:1"}

// fakeRouter stands in for the router package: it records what it was asked
// to put behind the tunnel and answers with routed, so TestRun never stands
// up a listener. A lone origin is routed like any run, as the real one does.
type fakeRouter struct {
	err      error
	dialable Origins
	ws       int
	front    func(http.Handler) http.Handler
	// ctx is the router's lifetime: the run's context detached from its
	// cancellation, as the real one makes it, and ended only by Cancel — so a
	// case can see when the run cancels it.
	ctx  context.Context
	stop context.CancelFunc
	// configured is the real router the run's options build, never routed by
	// the run: a case can route it itself to see what those options serve.
	configured *router.RouterImpl
	// unanswered is what Unanswered says nothing answered on, dialing
	// nothing: the dial is the router's, and router_test.go dials it. probed
	// is the list the run asked about.
	unanswered []int
	probed     Origins
}

func (f *fakeRouter) Unanswered(_ context.Context, origins Origins) []int {
	f.probed = origins
	return f.unanswered
}

func (f *fakeRouter) Route(ctx context.Context, opts ...router.Option) (*url.URL, error) {
	// The run's options, read back off a router they configure rather than
	// one that serves.
	r := router.New(opts...)
	f.configured = r
	dialable, front := r.Origins(), r.Wrap()
	f.ctx, f.stop = context.WithCancel(context.WithoutCancel(ctx))
	f.dialable, f.ws, f.front = dialable, r.WebSockets(), front
	if f.err != nil {
		return nil, f.err
	}
	return routed, nil
}

func (f *fakeRouter) Cancel() {
	if f.stop != nil {
		f.stop()
	}
}

// fakeIdentity stands in for the identity package: it answers what it was
// built with and records what the builder asked it, which is how a case pins
// the list that settled.
type fakeIdentity struct {
	token string
	err   error
	known [][]string
	asked [][]string
}

func (f *fakeIdentity) Known(names []string) error {
	f.known = append(f.known, slices.Clone(names))
	return f.err
}

func (f *fakeIdentity) Token(_ context.Context, names []string, _ v1.Logger) string {
	f.asked = append(f.asked, slices.Clone(names))
	return f.token
}

// fakeMotd stands in for the motd package: it records what Learn was handed
// and prints one line per message, so a case can see the run reach it and
// where its output landed.
type fakeMotd struct {
	learned []string
	onLearn func() // what else lands while the run learns
}

func (f *fakeMotd) Learn(raw []string, _ v1.Logger) {
	f.learned = raw
	if f.onLearn != nil {
		f.onLearn()
	}
}

// runHarness is run with every collaborator faked except the one that is
// pure: the shown's panel half, because its URL and the page it serves are
// what the assertions check. order records the effects that matter in the
// sequence they landed.
type runHarness struct {
	// tunnels are handed out in order through the run's one seam — a remint
	// after a refused replay gets the second — and specs records what each
	// was asked to hint with.
	tunnels []*fakeTunnel
	specs   []string
	cache   *fakeCache
	display *fakeDisplay
	binder  *fakeBinder
	router  *fakeRouter
	motd    *fakeMotd
	auth    *fakeAuth
	order   []string
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	// console, when set, is what the command is given for stdin and stdout
	// instead of the buffers: a real terminal, which is what mirrorable asks
	// about and what a bytes.Buffer can never be.
	console *os.File
	b       *BuilderImpl
}

func newRunHarness(t *testing.T, tun *fakeTunnel, urls ...string) *runHarness {
	t.Helper()
	t.Setenv(v1.LogEnv, "")     // a developer's shell must not turn the log on
	t.Setenv(v1.NoCacheEnv, "") // nor turn the cache off under a case that is about it
	// The provider travels by environment and the run sets it; blanked so a
	// case can read back exactly what this run set.
	t.Setenv(ltv1.CloudflareProviderEnv, "")
	// Unset by default: a developer's shell must not protect a case's run.
	// t.Setenv records the original for cleanup; the Unsetenv then holds.
	t.Setenv(v1.WWWAuthenticateEnv, "")
	os.Unsetenv(v1.WWWAuthenticateEnv)
	h := &runHarness{tunnels: []*fakeTunnel{tun}}
	h.cache = &fakeCache{order: &h.order}
	h.display = &fakeDisplay{DisplayImpl: display.New(), order: &h.order}
	h.binder = &fakeBinder{}
	h.router = &fakeRouter{}
	h.motd = &fakeMotd{}
	h.auth = &fakeAuth{AuthImpl: auth.New(), router: h.router}
	tun.order = &h.order
	h.b = New(
		WithOrigin(urls...),
		WithProvider("example.test"),
		WithTunnelFactory(func(spec string) libtunnel.TunnelV1 {
			h.specs = append(h.specs, spec)
			tun := h.tunnels[0]
			if len(h.tunnels) > 1 {
				h.tunnels = h.tunnels[1:]
			}
			return tun
		}),
		WithCache(h.cache),
		// A log file in a directory only this case sees: a unit test never
		// writes into the machine's own cache directory.
		WithLog(logs.New(logs.WithDir(t.TempDir()))),
		WithDisplay(h.display),
		WithBinder(h.binder),
		WithRouter(h.router),
		WithMotd(h.motd),
		WithAuth(h.auth),
	)
	return h
}

// run executes the built command with args, the command's stderr captured
// in h.stderr and every TUNNELD_ mirror blanked so the developer's shell
// cannot reach in. It is what TestRun drives now that run's body lives in
// Command's RunE.
func (h *runHarness) run(t *testing.T, ctx context.Context, args ...string) error {
	t.Helper()
	for _, name := range []string{v1.OriginsEnv, v1.ProviderEnv, v1.LogEnv, v1.MultiviewEnv, v1.QREnv} {
		t.Setenv(name, "")
	}
	cmd := h.b.Command()
	// Stdin is set either way: unset, cobra falls back to the process's own,
	// which is a terminal when the suite is run from one — and whether a
	// stream is a terminal is a thing the run now reads.
	if h.console != nil {
		cmd.SetIn(h.console)
		cmd.SetOut(h.console)
	} else {
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&h.stdout)
	}
	cmd.SetErr(&h.stderr)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

// captureStderr swaps os.Stderr for a pipe until the returned function is
// called, which restores it and hands back what was written. Nothing in the
// run is supposed to write there — its logger goes to the command's own
// stderr — so TestLogger reads this to prove that nothing did.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	// Drained in a goroutine, started before anything writes, so a chatty
	// logger can never fill the pipe's buffer and deadlock against a writer
	// waiting on a reader that has not started yet.
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()

	restored := false
	restore := func() string {
		if !restored {
			restored = true
			os.Stderr = orig
			w.Close() // unblocks the copy goroutine's Read, which closes done
			<-done
			r.Close()
		}
		return buf.String()
	}
	t.Cleanup(func() { restore() })
	return restore
}

// TestRun is the composition under test: every effect run has, against fakes
// for the edge, the disk, the shown and the daemon. Each case asserts what
// the code did before it was split, so a case going red is a behaviour
// change, not a refactor.
func TestRun(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"

	t.Run("a mint reports, opens the panel, then saves", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", ":4000")
		v1.Apply(h.b, WithOpen(true)) // its streams are buffers; say so out loud
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel // the signal, arriving once the tunnel is live

		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v, want nil after a signal", err)
		}
		if want := []string{""}; !slices.Equal(h.specs, want) {
			t.Errorf("engine asked for specs %q, want a single mint", h.specs)
		}
		if got := os.Getenv(ltv1.CloudflareProviderEnv); got != "example.test" {
			t.Errorf("%s = %q, want %q — --provider did not reach the mint", ltv1.CloudflareProviderEnv, got, "example.test")
		}
		// And what the tunnel serialized once it was up is what was cached.
		if want := h.tunnels[0].Serialize(); h.cache.spec != want {
			t.Errorf("cached spec = %q, want the tunnel's own %q", h.cache.spec, want)
		}
		if want := []string{"url", "open", "save"}; !slices.Equal(h.order, want) {
			t.Errorf("effects in order %v, want %v — the cache must not be written before the URL is live", h.order, want)
		}
		if want := []string{public}; !slices.Equal(h.display.opened, want) {
			t.Errorf("opened %q, want the panel address %q", h.display.opened, want)
		}
		// TestReportNamesTheMultiviewPanel: the panel's own address is the
		// one stdout carries when there is one — it answers for every origin
		// at once, so the origins it reaches are the list stderr puts
		// beneath it.
		if want := public + "\n"; h.stdout.String() != want {
			t.Errorf("stdout = %q, want the panel address %q", h.stdout.String(), want)
		}
		for _, want := range []string{"  -> http://localhost:3000\n", "  -> http://localhost:4000\n"} {
			if !strings.Contains(h.stderr.String(), want) {
				t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
			}
		}
		// The panel goes in front of the routing, and the tunnel is handed
		// the router's one address rather than the origins.
		if h.router.front == nil {
			t.Fatal("the router was given nothing to put in front, want the panel")
		}
		rec := httptest.NewRecorder()
		h.router.front(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("the bare address through the router's front = %q, want the page", got)
		}
		if got := h.router.dialable.Len(); got != 2 {
			t.Errorf("router was given %d origins, want 2", got)
		}
		if tun := h.tunnels[0]; len(tun.locals) != 1 || tun.locals[0] != routed {
			t.Errorf("tunnel was given %v, want the router's address alone", tun.locals)
		}
		if !h.binder.closed {
			t.Error("the binder's closer was never called; RunE's defer did not run")
		}
	})

	t.Run("the mint says which tunneld is asking", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel
		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if got, want := h.tunnels[0].headers, []string{"User-Agent: " + UserAgent()}; !slices.Equal(got, want) {
			t.Errorf("headers = %q, want %q", got, want)
		}
	})

	t.Run("the tunnel's context lives while it is up, and ends with the run", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		ctx, cancel := context.WithCancel(t.Context())
		atSave := errors.New("never saved")
		h.cache.onSave = func() { atSave = h.tunnels[0].ctx.Err(); cancel() }
		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if atSave != nil {
			t.Errorf("the tunnel's context at the save = %v, want it live", atSave)
		}
		if h.tunnels[0].ctx.Err() == nil {
			t.Error("the tunnel's context outlived the run")
		}
	})

	// A new spec while the run waits — a PATCH to the control path, or any
	// other spec the cache takes — is a new tunnel: the one up is stopped
	// and drained, the next is minted from the spec, and everything a tunnel
	// that comes up does happens again.
	t.Run("a new spec replaces the tunnel, and it is all shown again", func(t *testing.T) {
		const second = "https://bar.tunneled.pizza/"
		first, next := live(public), live(second)
		h := newRunHarness(t, first, ":3000")
		v1.Apply(h.b, WithOpen(true)) // its streams are buffers; say so out loud
		h.tunnels = append(h.tunnels, next)
		next.order = &h.order
		h.cache.specs = make(chan string, 1)
		ctx, cancel := context.WithCancel(t.Context())
		saves := 0
		var firstAtSecondSave error
		h.cache.onSave = func() {
			saves++
			if saves == 1 {
				// Arrives once the run has passed over its own save, the way a
				// PATCH turned away with a 429 would once it tried again.
				go func() { h.cache.specs <- "patched" }()
				return
			}
			firstAtSecondSave = first.ctx.Err()
			cancel()
		}
		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v, want nil after a signal", err)
		}
		if want := []string{"", "patched"}; !slices.Equal(h.specs, want) {
			t.Errorf("minted from %q, want the cached spec then the new one", h.specs)
		}
		if firstAtSecondSave == nil {
			t.Error("the first tunnel was still live when the second came up, want it stopped first")
		}
		if want := []string{public, second}; !slices.Equal(h.display.opened, want) {
			t.Errorf("opened %q, want each tunnel's address in turn", h.display.opened)
		}
		if want := []string{"url", "open", "save", "url", "open", "save"}; !slices.Equal(h.order, want) {
			t.Errorf("effects in order %v, want each tunnel shown and saved", h.order)
		}
		if want := public + "\n" + second + "\n"; h.stdout.String() != want {
			t.Errorf("stdout = %q, want both tunnels' addresses in turn", h.stdout.String())
		}
		if want := next.Serialize(); h.cache.spec != want {
			t.Errorf("cached spec = %q, want the second tunnel's %q", h.cache.spec, want)
		}
	})

	t.Run("the run's own save is not a new spec", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.cache.specs = make(chan string, 1)
		ctx, cancel := context.WithCancel(t.Context())
		// The save lands its spec on the channel; the signal waits until the
		// run has taken it off, so a run that mistook it for a new one has
		// already minted again by then.
		h.cache.onSave = func() {
			go func() {
				for len(h.cache.specs) > 0 {
					time.Sleep(time.Millisecond)
				}
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
		}
		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if want := []string{""}; !slices.Equal(h.specs, want) {
			t.Errorf("minted from %q, want once: the run's own save is the tunnel it has", h.specs)
		}
	})

	t.Run("the spec sent back as saved keeps the tunnel, and says so", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.cache.specs = make(chan string, 1)
		ctx, cancel := context.WithCancel(t.Context())
		// Once the run has passed over its own save, the same spec twice more —
		// what a caller replaying the cache file sends — and then the signal.
		// Armed by the first save only: the run saves again on each replay, to
		// settle it.
		saves := 0
		h.cache.onSave = func() {
			if saves++; saves > 1 {
				return
			}
			spec := h.cache.spec // read here, beside the saves that write it
			go func() {
				for range 2 {
					for len(h.cache.specs) > 0 {
						time.Sleep(time.Millisecond)
					}
					h.cache.specs <- spec
				}
				for len(h.cache.specs) > 0 {
					time.Sleep(time.Millisecond)
				}
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
		}
		if err := h.run(t, ctx, "--log-level", "info"); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if want := []string{""}; !slices.Equal(h.specs, want) {
			t.Errorf("minted from %q, want once", h.specs)
		}
		const kept = "the spec sent is the one this tunnel already has; keeping the tunnel"
		if n := strings.Count(h.stderr.String(), kept); n != 2 {
			t.Errorf("%q logged %d times, want twice: once per replay, never for the run's own save", kept, n)
		}
	})

	t.Run("a new spec whose tunnel never comes up ends the run", func(t *testing.T) {
		cause := errors.New("the edge refused the spec")
		h := newRunHarness(t, live(public), ":3000")
		h.tunnels = append(h.tunnels, dead(cause))
		h.cache.specs = make(chan string, 1)
		h.cache.onSave = func() { go func() { h.cache.specs <- "bad" }() }
		if err := h.run(t, t.Context()); !errors.Is(err, cause) {
			t.Errorf("run() = %v, want the new tunnel's %v", err, cause)
		}
		if want := []string{"", "bad"}; !slices.Equal(h.specs, want) {
			t.Errorf("minted from %q, want the new spec tried", h.specs)
		}
	})

	t.Run("a signal while a new spec waits is a clean stop", func(t *testing.T) {
		first := live(public)
		first.lingers = true // still draining when the signal lands
		h := newRunHarness(t, first, ":3000")
		h.cache.specs = make(chan string, 1)
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = func() {
			go func() {
				h.cache.specs <- "patched"
				// The run is draining the first tunnel for the new spec; the
				// signal lands before it is done.
				for first.ctx.Err() == nil {
					time.Sleep(time.Millisecond)
				}
				cancel()
				first.end()
			}()
		}
		if err := h.run(t, ctx); err != nil {
			t.Errorf("run() = %v, want nil: a signal is a clean stop", err)
		}
		if want := []string{""}; !slices.Equal(h.specs, want) {
			t.Errorf("minted from %q, want nothing minted after the signal", h.specs)
		}
	})

	t.Run("a run is registered while it runs, and only when asked", func(t *testing.T) {
		for _, on := range []bool{true, false} {
			h := newRunHarness(t, live(public), ":3000")
			if on {
				v1.Apply(h.b, WithPid(&fakePid{order: &h.order}))
			}
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel
			if err := h.run(t, ctx); err != nil {
				t.Fatalf("run() = %v, want nil after a signal", err)
			}
			want := []string{"url", "save"}
			if on {
				want = []string{"register", "url", "save", "detach", "release"}
			}
			if !slices.Equal(h.order, want) {
				t.Errorf("WithPid set = %v: effects in order %v, want %v", on, h.order, want)
			}
		}
	})

	t.Run("--no-cache keeps the spec, not the registration", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		ctx, cancel := context.WithCancel(t.Context())
		v1.Apply(h.b, WithPid(&fakePid{order: &h.order, onRegister: cancel}))
		_ = h.run(t, ctx, "--no-cache", ":3000")
		if !slices.Contains(h.order, "register") || h.cache.saved {
			t.Errorf("effects %v, saved = %v, want a registration and no spec", h.order, h.cache.saved)
		}
	})

	t.Run("the same run already going is refused before the mint", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		refusal := fmt.Errorf("%w as pid 42", v1.ErrRunning)
		v1.Apply(h.b, WithPid(&fakePid{order: &h.order, running: refusal}))
		if err := h.run(t, t.Context()); !errors.Is(err, v1.ErrRunning) {
			t.Fatalf("run() = %v, want ErrRunning", err)
		}
		if len(h.specs) != 0 || !slices.Equal(h.order, []string{"register"}) {
			t.Errorf("specs %q, effects %v, want nothing after the refusal", h.specs, h.order)
		}
	})

	t.Run("a detached run's streams go to its log file, and its logger leaves stderr", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		ctx, cancel := context.WithCancel(t.Context())
		fake := &fakePid{order: &h.order, waiting: true, onDetach: func() {
			// Once Run has taken the logger off stderr, which it does as
			// Detach returns: a line logged then must not reach stderr.
			go func() {
				time.Sleep(50 * time.Millisecond)
				h.b.log.Logger().Info("after the detach")
				cancel()
			}()
		}}
		v1.Apply(h.b, WithPid(fake), WithLogLevel("info"))
		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if fake.out == nil || !strings.HasSuffix(fake.out.Name(), h.b.Origins().Key()+".log") {
			t.Errorf("Detach was handed %v, want the run's <key>.log", fake.out)
		}
		if strings.Contains(h.stderr.String(), "after the detach") {
			t.Errorf("stderr carried a line logged after the detach:\n%s", h.stderr.String())
		}
	})

	t.Run("a detached run says nothing about Ctrl+C", func(t *testing.T) {
		for _, waiting := range []bool{true, false} {
			h := newRunHarness(t, live(public), ":3000")
			v1.Apply(h.b, WithPid(&fakePid{order: &h.order, waiting: waiting}))
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel
			if err := h.run(t, ctx); err != nil {
				t.Fatalf("run() = %v", err)
			}
			if got := strings.Contains(h.stderr.String(), stopHint); got == waiting {
				t.Errorf("launcher waiting = %v: stop hint printed = %v", waiting, got)
			}
		}
	})

	// --qr is the address a browser would open, drawn as a code: the panel's
	// when there is one, since it reaches every origin, and otherwise the
	// default origin's, one code either way. It goes on stderr after the map,
	// stdout keeping the addresses alone, and it is out before a detached run
	// hands its streams to the log, which is how a launcher's caller gets it.
	t.Run("--qr draws the address a browser opens, on stderr, before a detach", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args []string
			addr string // "" is no code at all
		}{
			{"one code for the panel", []string{"--qr"}, public},
			{"the default origin's without one", []string{"--qr", "--multiview=false"}, public + "?0"},
			{"none unless asked for", nil, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newRunHarness(t, live(public), ":3000", ":4000")
				ctx, cancel := context.WithCancel(t.Context())
				var detached string
				v1.Apply(h.b, WithPid(&fakePid{order: &h.order, waiting: true, onDetach: func() {
					detached = h.stderr.String()
					cancel()
				}}))
				if err := h.run(t, ctx, tc.args...); err != nil {
					t.Fatalf("run() = %v", err)
				}
				if strings.ContainsAny(h.stdout.String(), "█▀▄") {
					t.Errorf("stdout = %q, want the addresses alone", h.stdout.String())
				}
				if tc.addr == "" {
					if strings.Contains(h.stderr.String(), "█") {
						t.Errorf("stderr carries a code nobody asked for:\n%s", h.stderr.String())
					}
					return
				}
				lines, err := attach.QRLines(tc.addr, qr.M)
				if err != nil {
					t.Fatalf("QRLines: %v", err)
				}
				code := strings.Join(lines, "\n") + "\n"
				if got := strings.Count(h.stderr.String(), code); got != 1 {
					t.Errorf("the code of %s appears %d times on stderr, want 1:\n%s", tc.addr, got, h.stderr.String())
				}
				if !strings.Contains(detached, code) {
					t.Errorf("stderr at the detach = %q, want the code already out", detached)
				}
				if at, origin := strings.Index(h.stderr.String(), code), strings.LastIndex(h.stderr.String(), "  -> "); at < origin {
					t.Errorf("code at %d, last origin at %d, want the code after the map:\n%s", at, origin, h.stderr.String())
				}
			})
		}
	})

	t.Run("what the provider said is learned, and stderr stays the map", func(t *testing.T) {
		tun := live(public)
		tun.messages = []string{"data:text/markdown;base64,PiBbIXdhcm5pbmdd"} // "> [!warning]"
		h := newRunHarness(t, tun, ":3000", ":4000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v, want nil after a signal", err)
		}
		if !slices.Equal(h.motd.learned, tun.messages) {
			t.Errorf("Learn was handed %q, want the tunnel's %q", h.motd.learned, tun.messages)
		}
		// Nothing of it reaches stderr: the map and the logs are what that
		// stream carries, and the message is read on the frame and the panel.
		if out := h.stderr.String(); strings.Contains(out, "warning") || strings.Contains(out, "data:") {
			t.Errorf("stderr = %q, want no trace of the message", out)
		}
	})

	t.Run("a binder failure is returned before the engine is asked for anything", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.binder.err = errors.New("no such container")

		err := h.run(t, t.Context())
		if !errors.Is(err, h.binder.err) {
			t.Errorf("run() = %v, want the binder's own error", err)
		}
		if len(h.specs) != 0 {
			t.Errorf("engine was asked for %d specs, want none", len(h.specs))
		}
	})

	t.Run("a router failure is returned before the engine is asked for anything", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", ":4000")
		h.router.err = errors.New("no loopback")

		err := h.run(t, t.Context())
		if !errors.Is(err, h.router.err) {
			t.Errorf("run() = %v, want the router's own error", err)
		}
		if len(h.specs) != 0 {
			t.Errorf("engine was asked for %d specs, want none", len(h.specs))
		}
	})

	t.Run("the +ws origin reaches the router as its index", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", "http+ws://:5173")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if h.router.ws != 1 {
			t.Errorf("router was given ws = %d, want 1", h.router.ws)
		}
		if got := urlStrings(h.router.dialable.URLs()); got[1] != "http://localhost:5173" {
			t.Errorf("router was given %q, want the marker off the scheme", got)
		}
	})

	// The tunnel drains what the edge already sent it for a grace period
	// after the run ends, and every one of those requests goes through the
	// router, so the router is down with the tunnel rather than the run.
	// The router is handed the run's cache and serves a remote copy of the
	// file the run saved on the control path. Routed here rather than by the
	// run, against a real origin, to see what the run's options serve.
	envOf := func(t *testing.T, h *runHarness) (int, string, string) {
		t.Helper()
		if h.router.configured == nil {
			t.Fatal("the run never asked the router")
		}
		origin := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(origin.Close)
		u, _ := url.Parse(origin.URL)
		r := h.router.configured
		local, err := r.Route(t.Context(), router.WithOrigins(origins.New(origins.WithURL(u))))
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		t.Cleanup(r.Cancel)
		req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(local.String(), "/")+router.ControlPath+".env", nil)
		if err != nil {
			t.Fatal(err)
		}
		// The token the fake tunnel's secret makes: what an operator
		// holding the run's spec would send.
		req.Header.Set("Authorization", "token "+base64.StdEncoding.EncodeToString([]byte("secret-of-foo.tunneled.pizza")))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), resp.Header.Get(router.CacheKeyHeader)
	}
	t.Run("the router serves what the run saved", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel
		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if got, want := string(h.cache.secret), "secret-of-foo.tunneled.pizza"; got != want {
			t.Errorf("the run saved secret %q, want the tunnel's (%q)", got, want)
		}
		want := h.cache.String()
		if !strings.HasPrefix(want, "LIBTUNNEL_SPEC=") {
			t.Fatalf("the run saved %q, want a cache file", want)
		}
		code, body, key := envOf(t, h)
		if code != 200 || body != want {
			t.Errorf("GET .env = %d %q, want the file the run saved:\n%s", code, body, want)
		}
		if key == "" || key != h.cache.key {
			t.Errorf("GET .env named the run %q, want the key it saved under (%q)", key, h.cache.key)
		}
	})
	t.Run("a run with caching off serves its .env from memory", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.cache.cached = "cached-spec"
		ctx, cancel := context.WithCancel(t.Context())
		v1.Apply(h.b, WithPid(&fakePid{order: &h.order, onRegister: cancel}))
		_ = h.run(t, ctx, "--no-cache", ":3000")
		// --no-cache is about the hostname, not the control path: the run
		// keeps its spec in memory, so .env answers (and a password can be
		// set), while the file the cache would write is never touched and
		// the cached spec is never replayed.
		code, body, _ := envOf(t, h)
		if code != 200 || !strings.HasPrefix(body, "LIBTUNNEL_SPEC=") || strings.Contains(body, "cached-spec") {
			t.Errorf("GET .env with --no-cache = %d %q, want this run's spec from memory", code, body)
		}
		if h.cache.saved {
			t.Error("--no-cache saved to the on-disk cache")
		}
	})

	t.Run("the router outlives the run until the tunnel ends", func(t *testing.T) {
		tun := live(public)
		tun.lingers = true
		h := newRunHarness(t, tun, ":3000", ":4000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if err := h.router.ctx.Err(); err != nil {
			t.Fatalf("router's context ended with the run (%v), want it serving until the tunnel ends", err)
		}
		tun.end()
		select {
		case <-h.router.ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("router's context outlived the tunnel")
		}
	})

	t.Run("a cached spec is what the engine replays", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.cache.cached = "cached-spec"
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if want := []string{"cached-spec"}; !slices.Equal(h.specs, want) {
			t.Errorf("engine asked for %q, want the cached spec", h.specs)
		}
	})

	// A failed replay is returned as it is: no second attempt and no discard.
	// From asks the edge about a hint before minting, so a dead spec is
	// already a fresh mint by the time it reaches here — the one failure left
	// is a provider that could not be reached, which a remint could not reach
	// either, and which a stale file cannot make worse next time.
	t.Run("a replay failure is returned and the cache kept", func(t *testing.T) {
		boom := errors.New("provider unreachable")
		h := newRunHarness(t, dead(boom), ":3000")
		h.cache.cached = "good"

		err := h.run(t, t.Context())
		if !errors.Is(err, boom) {
			t.Fatalf("run() = %v, want the tunnel's own error", err)
		}
		if h.cache.saved {
			t.Error("a tunnel that never came up was cached")
		}
		if want := []string{"good"}; !slices.Equal(h.specs, want) {
			t.Errorf("asked for %q, want one replay and no remint", h.specs)
		}
	})

	t.Run("no URL and no cause is ErrNotReady", func(t *testing.T) {
		h := newRunHarness(t, &fakeTunnel{done: make(chan libtunnel.TunnelV1, 1)}, ":3000")
		err := h.run(t, t.Context())
		if !errors.Is(err, v1.ErrNotReady) {
			t.Errorf("run() = %v, want ErrNotReady", err)
		}
	})

	t.Run("one origin keeps the bare address and no panel", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		v1.Apply(h.b, WithOpen(true)) // its streams are buffers; say so out loud
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
		if want := []string{public}; !slices.Equal(h.display.opened, want) {
			t.Errorf("opened %q, want the plain URL %q", h.display.opened, want)
		}
		if h.router.front != nil {
			t.Error("the router was given a panel to put in front of one origin, want none")
		}
		if want := public + "\n"; h.stdout.String() != want {
			t.Errorf("stdout = %q, want the bare address %q", h.stdout.String(), want)
		}
		if want := "  -> http://localhost:3000\n"; !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
		}
	})

	t.Run("a tunnel that fails after coming up returns its error", func(t *testing.T) {
		tun := live(public)
		h := newRunHarness(t, tun, ":3000")
		gone := errors.New("edge went away")
		h.cache.onSave = func() {
			tun.err = gone
			tun.end()
		}

		err := h.run(t, t.Context())
		if !errors.Is(err, gone) {
			t.Errorf("run() = %v, want the tunnel's error", err)
		}
	})

	t.Run("a bare struct is refused before it touches anything", func(t *testing.T) {
		// The wiring check runs at the top of Command, before a flag is
		// bound to any field — a bare BuilderImpl never reaches that
		// binding, so Command hands back a minimal command whose only job
		// is to report the error. The display is checked first, so a bare
		// struct's error names it.
		b := &BuilderImpl{origins: []string{":3000"}}
		cmd := b.Command()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(nil)
		err := cmd.ExecuteContext(t.Context())
		if err == nil || !strings.Contains(err.Error(), "browser") || !strings.Contains(err.Error(), "construct it with New") {
			t.Errorf("run() on a bare BuilderImpl = %v, want the wiring error naming browser", err)
		}
	})
}

// TestParseOriginsAccepts covers the shapes a caller is allowed to type,
// including the bare host:port that implies http — the affordance that lets
// `tunneld localhost:3000` work the way people expect. Driven through the
// whole run (the origin parsing has no seam of its own to call directly), so
// what is pinned is what is actually routed to: the router's list, in order.
func TestParseOriginsAccepts(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
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
		{"a container by name", []string{"attach://dockerd/api"}, []string{"attach://dockerd/api"}},
		// The shape the parser produces for itself when it resolves a bare
		// word, so it has to read it back: an absolute path cannot be a URL's
		// authority, only its path, and the empty authority is this machine.
		{"a program by path", []string{"exec:///usr/bin/top"}, []string{"exec:///usr/bin/top"}},
		{"a container by id", []string{"attach://dockerd/3f2a1b9c8d7e"}, []string{"attach://dockerd/3f2a1b9c8d7e"}},
		{"a container name keeps case and underscores", []string{"attach://dockerd/My_Container"}, []string{"attach://dockerd/My_Container"}},
		// A provider is free to read a reference with a separator in it however
		// it likes; the parser's business ends at "there is one".
		{"a reference may carry a separator", []string{"attach://dockerd/api/sh"}, []string{"attach://dockerd/api/sh"}},
		{
			"a container beside an http origin, in order",
			[]string{"http://localhost:3000", "attach://dockerd/api"},
			[]string{"http://localhost:3000", "attach://dockerd/api"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunHarness(t, live(public), tc.in...)
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel

			if err := h.run(t, ctx); err != nil {
				t.Fatalf("run(%q) = %v, want ok", tc.in, err)
			}
			got := h.router.dialable.URLs()
			if len(got) != len(tc.want) {
				t.Fatalf("run(%q) routed %d origins, want %d", tc.in, len(got), len(tc.want))
			}
			for i, u := range got {
				if u.String() != tc.want[i] {
					t.Errorf("origin %d = %q, want %q", i, u, tc.want[i])
				}
			}
		})
	}
}

// TestOriginsTakesTheMarkerOffTheScheme pins where the +ws marker goes: off
// the scheme and onto the list as an index, so every URL downstream is the
// bare address it dials. It used to ride the scheme through three packages to
// the tunnel engine, and the binder dropping it was #173; there is nothing
// left to drop now (#176).
func TestOriginsTakesTheMarkerOffTheScheme(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
		ws   int // -1 for none
	}{
		{"no marker", []string{"http://localhost:4000", "http://localhost:5173"}, []string{"http://localhost:4000", "http://localhost:5173"}, -1},
		{"the second owns websockets", []string{"http://localhost:4000", "http+ws://localhost:5173"}, []string{"http://localhost:4000", "http://localhost:5173"}, 1},
		{"the marker on https", []string{"https+wss://localhost:5173", "http://localhost:4000"}, []string{"https://localhost:5173", "http://localhost:4000"}, 0},
		{"ws and wss are interchangeable", []string{"http://localhost:4000", "http+wss://localhost:5173"}, []string{"http://localhost:4000", "http://localhost:5173"}, 1},
		{"a marked origin keeps the bare-port shorthand", []string{"http://localhost:4000", "http+ws://:5173"}, []string{"http://localhost:4000", "http://localhost:5173"}, 1},
		// The index is the origin's place in what survived, not in what was
		// typed: a dropped origin before it moves it up.
		{"counted after a drop", []string{"ftp://localhost:21", "http://localhost:4000", "http+ws://localhost:5173"}, []string{"http://localhost:4000", "http://localhost:5173"}, 1},
		{"a second claim keeps the first", []string{"http+ws://localhost:4000", "http+ws://localhost:5173"}, []string{"http://localhost:4000", "http://localhost:5173"}, 0},
		// A marked origin dropped for want of a host never held the claim, so
		// the next marked origin gets it rather than losing its marker.
		{"a dropped origin claims nothing", []string{"http+ws://", "http://localhost:4000", "http+ws://localhost:5173"}, []string{"http://localhost:4000", "http://localhost:5173"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := New(WithOrigin(tc.in...), WithStderr(io.Discard)).Origins()
			if urls := originStrings(got); !slices.Equal(urls, tc.want) {
				t.Errorf("Origins(%q) = %q, want %q", tc.in, urls, tc.want)
			}
			ws, ok := got.(interface{ WebSocket() (int, bool) }).WebSocket()
			if !ok {
				ws = -1
			}
			if ws != tc.ws {
				t.Errorf("WebSocket() = %d, want %d", ws, tc.ws)
			}
		})
	}
}

// originStrings renders parsed origins for comparison, so a table can be
// written the way somebody types origins rather than as *url.URL literals.
func originStrings(origins Origins) []string {
	return urlStrings(origins.URLs())
}

// urlStrings is the same for a plain slice, which is what a case that slices
// the list to compare its tail has in hand.
func urlStrings(urls []*url.URL) []string {
	got := make([]string, len(urls))
	for i, u := range urls {
		got[i] = u.String()
	}
	return got
}

// TestOriginsDropsTheUnusable pins that an origin tunneld cannot expose is
// dropped with a warning rather than failing the whole run. One typo used to
// take every other origin down with it, and the origins that work are what
// somebody is waiting on.
//
// Each case asserts both halves of that bargain: what survived, and that the
// warning names the value that did not — a drop nobody is told about is just a
// missing origin. Origins is called directly rather than through a run, so
// nothing here dials.
// TestRunIsTheOtherDoor covers the run reached without a command line: a
// program that configured the builder with options and wants a tunnel, not a
// CLI. Same work and the same streams as executing the command, with no argv
// parsed on the way and no command anybody will ever see.
//
// The environment is asserted on this path too, because binding it is
// PersistentPreRunE's job when a command is executed and nothing runs
// PersistentPreRunE here. Run calls applyEnv itself so env still beats code,
// and TUNNELD_PROVIDER reaching the engine is what proves it.
func TestRunIsTheOtherDoor(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000")
	for _, name := range []string{v1.OriginsEnv, v1.NoCacheEnv, v1.MultiviewEnv} {
		t.Setenv(name, "")
	}
	t.Setenv(v1.ProviderEnv, "from-the-environment.test")

	var stdout, stderr bytes.Buffer
	v1.Apply(h.b, WithStdout(&stdout), WithStderr(&stderr))
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.b.Run(ctx); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if want := public + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if want := "  -> http://localhost:3000\n"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not contain %q", stderr.String(), want)
	}
	if got := os.Getenv(ltv1.CloudflareProviderEnv); got != "from-the-environment.test" {
		t.Errorf("%s = %q, want the one the environment set", ltv1.CloudflareProviderEnv, got)
	}
}

// TestOriginsFallsBackToTheShell covers the answer to being given nothing:
// a shell, which every run can have. $SHELL first, then bash and sh on $PATH,
// then the one built into tunneld. Each is resolved before it is adopted,
// because the parse loop's fallback for an unresolvable word is to read it as
// an address — so an unrunnable $SHELL has to be passed over, not become a
// proxy to localhost.
//
// A seed or an argument outranks all of it, and declining the fallback
// declines every step, the built-in shell included.
func TestOriginsFallsBackToTheShell(t *testing.T) {
	// A $PATH of fake shells, so which one is chosen is this case's to
	// decide and not the machine's. Windows finds a program by its
	// extension, so the fakes carry one there.
	fake := func(t *testing.T, dir, name string) string {
		t.Helper()
		if runtime.GOOS == "windows" {
			name += ".bat"
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	exec := func(path string) []*url.URL { return []*url.URL{{Scheme: v1.ExecScheme, Path: path}} }
	// On Windows there is no built-in shell to fall back to, and with the
	// experiment turned off there is none anywhere, so the chain ends with
	// nothing, as it did before there was one.
	var builtinURL *url.URL
	if builtin := v0exp1.Experimental().Builtin(); builtin != nil {
		if origin, err := builtin.Origin(); err == nil {
			builtinURL, _ = url.Parse(origin)
		} else if runtime.GOOS != "windows" {
			t.Fatalf("v0exp1.Experimental().Builtin().Origin() = %v", err)
		}
	}

	for _, tc := range []struct {
		name     string
		onPath   []string // fake shells on $PATH
		shell    string   // $SHELL: a fake's name, a path that is not there, or ""
		fallback bool
		args     []string
		want     string // the fake chosen, "builtin", ":3000", or "" for none
		mention  string
	}{
		{"a runnable $SHELL is the origin", []string{"zsh", "bash", "sh"}, "zsh", true, nil, "zsh", ""},
		{"an unrunnable $SHELL is passed over for bash", []string{"bash", "sh"}, "missing", true, nil, "bash", "not exposing $SHELL"},
		{"no $SHELL is bash", []string{"bash", "sh"}, "", true, nil, "bash", ""},
		{"no bash is sh", []string{"sh"}, "", true, nil, "sh", ""},
		{"no shell at all is the built-in one", nil, "", true, nil, "builtin", ""},
		{"an unrunnable $SHELL and nothing else is the built-in one", nil, "missing", true, nil, "builtin", "not exposing $SHELL"},
		{"an argument outranks it", []string{"bash"}, "", true, []string{":3000"}, ":3000", ""},
		// The knob is asked before anything is looked for, so a shell that is
		// there, or the built-in one, is still not an origin when nobody
		// wanted one.
		{"the option declines a shell", []string{"bash"}, "", false, nil, "", ""},
		{"the option declines the built-in one", nil, "", false, nil, "", ""},
		{"declining does not touch an argument", nil, "", false, []string{":3000"}, ":3000", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := map[string]string{}
			for _, name := range tc.onPath {
				paths[name] = fake(t, dir, name)
			}
			t.Setenv("PATH", dir)
			t.Setenv(v1.OriginsEnv, "") // a developer's shell must not seed this
			switch tc.shell {
			case "":
				t.Setenv("SHELL", "")
			case "missing":
				t.Setenv("SHELL", filepath.Join(dir, "nope"))
			default:
				t.Setenv("SHELL", paths[tc.shell])
			}

			var stderr bytes.Buffer
			b := New(WithLogLevel("warn"), WithStderr(&stderr), WithShellFallback(tc.fallback))
			if err := b.Command().ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.args, err)
			}

			var want []*url.URL
			switch tc.want {
			case "":
			case ":3000":
				want = []*url.URL{{Scheme: "http", Host: "localhost:3000"}}
			case "builtin":
				if builtinURL != nil {
					want = []*url.URL{builtinURL}
				}
			default:
				resolved, ok := shell.Resolve(paths[tc.want])
				if !ok {
					t.Fatalf("fake %s does not resolve", tc.want)
				}
				want = exec(resolved)
			}
			// Compared as fields rather than as strings, because a Windows
			// path is C:\... and url.URL.String escapes every separator in it.
			got := b.Origins().URLs()
			if !slices.EqualFunc(got, want, func(a, b *url.URL) bool {
				return a.Scheme == b.Scheme && a.Host == b.Host && a.Path == b.Path && a.RawQuery == b.RawQuery
			}) {
				t.Errorf("Origins() = %v, want %v", got, want)
			}
			if tc.mention != "" && !strings.Contains(stderr.String(), tc.mention) {
				t.Errorf("stderr %q does not mention %q", stderr.String(), tc.mention)
			}
		})
	}
}

func TestOriginsDropsTheUnusable(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		mention string
	}{
		{"empty value", []string{""}, nil, "dropping an origin"},
		{"whitespace only", []string{"   "}, nil, "dropping an origin"},
		{"unproxyable scheme", []string{"ftp://localhost:21"}, nil, "ftp://localhost:21"},
		{"scheme with no host", []string{"http://"}, nil, "http://"},
		{
			"one bad origin among good ones",
			[]string{"http://localhost:3000", "ftp://localhost:21", "http://localhost:4000"},
			[]string{"http://localhost:3000", "http://localhost:4000"},
			"ftp://localhost:21",
		},
		{
			// The marker goes, the origin stays: it is a perfectly good origin
			// that asked for something already taken.
			"a second origin claiming the websockets",
			[]string{"http+ws://localhost:4000", "http+ws://localhost:5173"},
			[]string{"http://localhost:4000", "http://localhost:5173"},
			"already claimed",
		},
		{"the marker on a container", []string{"attach+ws://dockerd/api"}, nil, "attach+ws"},
		{"the marker on an unproxyable scheme", []string{"ftp+ws://localhost:21"}, nil, "ftp+ws"},
		{"a container with no provider or name", []string{"attach://"}, nil, "names no container"},
		{"a provider with no container", []string{"attach://dockerd"}, nil, "names no container"},
		{"a program with no name", []string{"exec://"}, nil, "names no program"},
		// The authority is looked up as a program before it is read as a
		// provider, so a word this machine cannot run leaves nothing either
		// reading can use.
		{"an authority that is neither program nor provider", []string{"exec://definitely-not-a-program"}, nil, "names no program"},
		{"a container with a query", []string{"attach://dockerd/api?tty=1"}, nil, "attach://dockerd/api"},
		{"a container with a fragment", []string{"attach://dockerd/api#sh"}, nil, "attach://dockerd/api"},
		{"a container with a userinfo", []string{"attach://root@dockerd/api"}, nil, "carries more than a container reference"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			b := New(WithOrigin(tc.in...), WithLogLevel("warn"), WithStderr(&stderr))

			if got := originStrings(b.Origins()); !slices.Equal(got, tc.want) {
				t.Errorf("Origins(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !strings.Contains(stderr.String(), tc.mention) {
				t.Errorf("warnings %q do not name %q", stderr.String(), tc.mention)
			}
		})
	}
}

// TestOriginsRunsAProgram pins the shorthand that makes `tunneld htop` mean
// what somebody typing it meant: a bare word this machine can run is a program
// origin, not the unresolvable hostname the http default would have made of
// it. Everything beside it is untouched, which is the other half — the rule
// only claims words that resolve.
func TestOriginsRunsAProgram(t *testing.T) {
	// Unix reads the mode bit and Windows reads the extension; .bat is on the
	// default PATHEXT exec.LookPath falls back to.
	name := "tunneld-origin-fixture"
	if runtime.GOOS == "windows" {
		name += ".bat"
	}
	const script = "#!/bin/sh\nexit 0\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	t.Setenv("PATH", dir)

	// The program last: the words after one are its arguments, so a URL
	// after it would be handed to it rather than served.
	b := New(WithOrigin("http://localhost:3000", "attach://dockerd/api", name))
	got := b.Origins()

	// The origin carries the resolved path, not the word typed: "top" names a
	// program only on the machine that looked it up, and the frame, the
	// reported map and a pasted-back copy all read this.
	want := filepath.Join(dir, name)
	if got.Len() != 3 || got.At(2).Scheme != v1.ExecScheme || got.At(2).Path != want {
		t.Fatalf("Origins() = %q, want the last to be %s://%s", originStrings(got), v1.ExecScheme, want)
	}
	// And it survives being written out and read back, which is the promise
	// the frame makes when it puts the origin in its corner.
	//
	// A Unix promise only: a Windows absolute path is C:\..., which has no
	// spelling inside an exec:// URL — url.URL escapes the separators either
	// way round. It costs nothing there, because the binder reads the path off
	// the URL rather than off its printed form.
	if runtime.GOOS != "windows" {
		again, err := url.Parse(got.At(2).String())
		if err != nil {
			t.Fatalf("%q did not parse back: %v", got.At(2), err)
		}
		if again.Path != want {
			t.Errorf("%q parsed back to path %q, want %q", got.At(2), again.Path, want)
		}
	}
	if rest := urlStrings(got.URLs()[:2]); !slices.Equal(rest, []string{"http://localhost:3000", "attach://dockerd/api"}) {
		t.Errorf("the other origins = %q, want them untouched", rest)
	}

	// The same word spelled with its scheme resolves the same way. An
	// authority with nothing after it cannot be a provider being asked for
	// something, so it is looked up as a program first — and lands on the
	// resolved path, exactly as the bare word does.
	spelled := New(WithOrigin(v1.ExecScheme + "://" + name)).Origins()
	if spelled.Len() != 1 || spelled.At(0).Scheme != v1.ExecScheme || spelled.At(0).Path != want {
		t.Fatalf("Origins(%q) = %q, want %s://%s", v1.ExecScheme+"://"+name, originStrings(spelled), v1.ExecScheme, want)
	}
	if got.At(2).String() != spelled.At(0).String() {
		t.Errorf("%q and %q are the same program spelled two ways, got %q and %q",
			name, v1.ExecScheme+"://"+name, got.At(2), spelled.At(0))
	}
}

// TestOriginsGiveAProgramTheWordsAfterIt pins docker's rule for the command
// line: origins are read left to right, and the first word that names a
// program takes every word after it as its arguments. `tunneld claude
// --resume` runs claude with --resume; `tunneld :3000 claude --resume` serves
// :3000 and then claude with --resume; and the program is the last origin on
// the line, the way docker's image is followed only by its own command.
func TestOriginsGiveAProgramTheWordsAfterIt(t *testing.T) {
	name := "tunneld-origin-fixture"
	if runtime.GOOS == "windows" {
		name += ".bat"
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, name)

	// Spelled through url.URL rather than by concatenation, on both sides of
	// every comparison: a Windows path has backslashes, which String()
	// percent-encodes, so a raw `exec://C:\...` neither parses nor matches.
	program := func(args ...string) string {
		u := &url.URL{Scheme: v1.ExecScheme, Path: path}
		if len(args) > 0 {
			u.RawQuery = url.Values{v1.ArgKey: args}.Encode()
		}
		return u.String()
	}

	for _, tc := range []struct {
		name    string
		origins []string
		// want is each origin's String(); the program's carries its query.
		want []string
	}{
		{
			name:    "the words after a program are its arguments",
			origins: []string{name, "--resume", "--model", "opus"},
			want:    []string{program("--resume", "--model", "opus")},
		},
		{
			name:    "origins before the program stay origins",
			origins: []string{"http://localhost:3000", "attach://dockerd/api", name, "--resume"},
			want:    []string{"http://localhost:3000", "attach://dockerd/api", program("--resume")},
		},
		{
			name:    "a port after the program is a second origin, not a script to run",
			origins: []string{name, ":8000"},
			want:    []string{program(), "http://localhost:8000"},
		},
		{
			name:    "a URL after the program is a second origin",
			origins: []string{name, "--resume", "http://localhost:3000"},
			want:    []string{program("--resume"), "http://localhost:3000"},
		},
		{
			name:    "the arguments stop at the first origin and the program after it takes its own",
			origins: []string{name, "-x", ":8000", name, "-y"},
			want:    []string{program("-x"), "http://localhost:8000", program("-y")},
		},
		{
			name:    "the program's own word again is a second one, not an argument",
			origins: []string{name, name, "-x"},
			want:    []string{program(), program("-x")},
		},
		{
			name:    "a program quoted with its arguments is complete, and a bare word after it is the next origin",
			origins: []string{name + " -m http.server 8000", name, ":9000"},
			want:    []string{program("-m", "http.server", "8000"), program(), "http://localhost:9000"},
		},
		{
			name:    "quotes inside the group keep an argument whole",
			origins: []string{name + ` -c 'echo hi there' "a b"`},
			want:    []string{program("-c", "echo hi there", "a b")},
		},
		{
			name:    "a bare word or a path after the program is still its argument",
			origins: []string{name, "8000", "./script.sh", "host:8000"},
			want:    []string{program("8000", "./script.sh", "host:8000")},
		},
		{
			name:    "a program spelled as a URL takes the rest too",
			origins: []string{v1.ExecScheme + "://" + name, "-d", "5"},
			want:    []string{program("-d", "5")},
		},
		{
			name:    "arguments an origin already carries come first",
			origins: []string{program("--resume"), "--model", "opus"},
			want:    []string{program("--resume", "--model", "opus")},
		},
		{
			name:    "a program with no words after it has none",
			origins: []string{"http://localhost:3000", name},
			want:    []string{"http://localhost:3000", program()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A seeded exec:// URL is the one input here that spells the path
			// inside the URL rather than resolving a word to it, and a
			// Windows path has no such spelling: with every backslash
			// percent-encoded there is no separator after the authority, so
			// the whole path parses as a host and the origin is dropped —
			// the limitation TestOriginsRunsAProgram already notes. The
			// program is still found by its bare word there, which the other
			// cases cover.
			if runtime.GOOS == "windows" && strings.HasPrefix(tc.origins[0], v1.ExecScheme+"://"+path[:1]) {
				t.Skip("an absolute Windows path cannot be spelled inside an exec:// URL")
			}
			got := originStrings(New(WithOrigin(tc.origins...)).Origins())
			if !slices.Equal(got, tc.want) {
				t.Errorf("Origins(%q) = %q, want %q", tc.origins, got, tc.want)
			}
		})
	}

	// A container is not a program, so the words after it are origins in
	// their own right — and one that names nothing is dropped as it always
	// was, rather than handed to the container.
	got := originStrings(New(WithOrigin("attach://dockerd/api", "http://localhost:3000")).Origins())
	if want := []string{"attach://dockerd/api", "http://localhost:3000"}; !slices.Equal(got, want) {
		t.Errorf("Origins() = %q, want %q — a container takes no arguments", got, want)
	}
}

// TestFlagsStopAtTheFirstOrigin pins the other half of docker's rule: tunneld's
// own flags parse up to the first origin and not past it, so a flag after a
// program is the program's even when tunneld has a flag of the same name.
func TestFlagsStopAtTheFirstOrigin(t *testing.T) {
	name := "tunneld-origin-fixture"
	if runtime.GOOS == "windows" {
		name += ".bat"
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	t.Setenv("PATH", dir)

	b := New()
	cmd := b.Command()
	// --log-level twice: the first is tunneld's, the second is the program's,
	// and "loud" is a level tunneld would refuse if it were reading it.
	if err := cmd.ParseFlags([]string{"--log-level", "debug", name, "--log-level", "loud"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got := b.logLevel; got != "debug" {
		t.Errorf("tunneld's --log-level = %q, want %q — the program's flag was read as tunneld's", got, "debug")
	}
	if got, want := cmd.Flags().Args(), []string{name, "--log-level", "loud"}; !slices.Equal(got, want) {
		t.Fatalf("positional args = %q, want %q", got, want)
	}
	got := b.Origins()
	if got.Len() != 1 || !slices.Equal(got.At(0).Query()[v1.ArgKey], []string{"--log-level", "loud"}) {
		t.Errorf("Origins() = %q, want one program with --log-level loud as its arguments", originStrings(got))
	}
}

// TestLauncherFlagsAreNotTunnelds pins that -d and -k stay the npm
// launcher's. Neither is defined here, so `npx tunneld -d` and `tunneld -d`
// cannot come to mean two things; one that reaches the command anyway, first
// or after another flag, is refused with where it belongs. Any other unknown
// flag is still cobra's error, or the error of the command this one is
// mounted under, which is what an embedding program's own handler expects.
func TestLauncherFlagsAreNotTunnelds(t *testing.T) {
	for _, s := range []string{"d", "k"} {
		if f := New().Command().Flags().ShorthandLookup(s); f != nil {
			t.Errorf("-%s is defined as --%s: it is the npm launcher's", s, f.Name)
		}
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"-d first", []string{"-d", ":3000"}, "-d is not a tunneld flag: the tunneld npm launcher detaches"},
		{"-d after a flag", []string{"--log-level", "debug", "-d", ":3000"}, "-d is not a tunneld flag"},
		{"-k", []string{"-k"}, "-k is not a tunneld flag: the tunneld npm launcher ends every run"},
		{"-kd", []string{"-kd", ":3000"}, "-kd is not a tunneld flag: the tunneld npm launcher ends every run and detaches"},
		{"-dk", []string{"-dk", ":3000"}, "-dk is not a tunneld flag"},
		{"another", []string{"-z"}, "unknown shorthand flag: 'z' in -z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := execute(t, New(), tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	t.Run("mounted", func(t *testing.T) {
		parent := &cobra.Command{Use: "host", SilenceErrors: true, SilenceUsage: true}
		parent.SetFlagErrorFunc(func(*cobra.Command, error) error { return errors.New("the host's") })
		parent.AddCommand(New().Command())
		parent.SetOut(io.Discard)
		parent.SetErr(io.Discard)
		for args, want := range map[string]string{
			"tunneld -z":         "the host's",
			"tunneld version -z": "the host's",
			"tunneld -d":         "-d is not a tunneld flag",
		} {
			parent.SetArgs(strings.Fields(args))
			if err := parent.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error = %v, want it to contain %q", args, err, want)
			}
		}
	})
}

// TestOriginsWarnsAtTheTunnelsLevel pins that a dropped origin is reported at
// the level the tunnel's own logs are, rather than uninvited on stderr: a
// library that writes without being asked pollutes its importer's output, and
// tunneld's default is silence.
func TestOriginsWarnsAtTheTunnelsLevel(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  bool
	}{
		{"", false},
		{"error", false},
		{"warn", true},
		{"debug", true},
	} {
		t.Run("level "+tc.level, func(t *testing.T) {
			var stderr bytes.Buffer
			b := New(WithOrigin("ftp://localhost:21"), WithLogLevel(tc.level), WithStderr(&stderr))
			b.Origins()

			if got := strings.Contains(stderr.String(), "ftp://localhost:21"); got != tc.want {
				t.Errorf("--log-level %q warned = %v, want %v (%q)", tc.level, got, tc.want, stderr.String())
			}
		})
	}
}

// TestOriginsNoneLeftIsAnError pins the one failure that survives the drop: a
// run with nothing left to expose refuses rather than minting a hostname that
// answers only errors, and its message names both ways of supplying an origin.
func TestOriginsNoneLeftIsAnError(t *testing.T) {
	_, _, err := execute(t, New(WithOrigin("ftp://localhost:21")))
	if !errors.Is(err, v1.ErrNoOrigin) {
		t.Fatalf("running with only an unusable origin = %v, want ErrNoOrigin", err)
	}
	if !strings.Contains(err.Error(), v1.OriginsEnv) {
		t.Errorf("error %q does not name %s", err, v1.OriginsEnv)
	}
}

// TestAViewerCanEndTheRun pins the last link of the frame's exit: a keystroke
// in a browser tab stops the process.
//
// The command is the only thing that can — ending the run takes its context
// and everything started under it — so what the terminal does is ask, on the
// closer Bind handed back, and this is the arm that listens. It is a clean
// exit, not a failure: nothing went wrong, somebody chose it.
func TestAViewerCanEndTheRun(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000")
	h.binder.asked = make(chan struct{})

	// Asked once the run is up, so what is pinned is the wait ending rather
	// than a start that never happened. The cache save is the last thing
	// before the wait, which makes it the moment a viewer could be looking.
	h.cache.onSave = func() { close(h.binder.asked) }

	if err := h.run(t, t.Context()); err != nil {
		t.Fatalf("run() = %v, want a clean exit", err)
	}
	if !h.binder.closed {
		t.Error("the origins were left up after the run ended")
	}
}

// TestReportNamesAProgramWithoutItsArguments pins what the origin map shows
// for a program started with arguments: the program. The arguments ride the
// origin as a query because that is how they travel; the map is for a person.
func TestReportNamesAProgramWithoutItsArguments(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), "exec:///usr/bin/htop?arg=-d&arg=5")
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx); err != nil {
		t.Fatalf("run() = %v, want nil after a signal", err)
	}
	if want := "  -> exec:///usr/bin/htop\n"; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("stderr %q does not map the program as %q", h.stderr.String(), want)
	}
	if strings.Contains(h.stderr.String(), "?arg=") {
		t.Errorf("stderr shows the arguments' carrier:\n%s", h.stderr.String())
	}
	// The router in front of the tunnel was still handed the origin whole;
	// the arguments are the binder's to read off it.
	if got := h.router.dialable.URLs(); len(got) != 1 || got[0].Query().Get(v1.ArgKey) != "-d" {
		t.Errorf("router was given %v, want the origin with its arguments", got)
	}
}

// TestReportSplitsTheAddressFromItsOrigin pins the output contract: a running
// tunnel writes each public address to stdout, one per line and nothing else,
// and writes the origin that address reaches to stderr beneath it.
//
// stdout did carry one bare URL per origin once, alongside a full map on
// stderr, and every address then printed twice wherever both streams landed
// together; the de-duplication meant to hide that could only recognise one
// file descriptor being literally the other — which a container's two pipes
// are not, so it never fired there. A split is not that duplication: an
// address reaches exactly one stream, which keeps `tunneld > addresses` a
// machine interface while a terminal holding both still reads as a map.
//
// Multiview is turned off so the report falls into its per-origin branch —
// TestRun's "mint" case already pins the panel branch, folding in
// TestReportNamesTheMultiviewPanel's doc.
func TestReportSplitsTheAddressFromItsOrigin(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000", ":4000")
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx, "--multiview=false"); err != nil {
		t.Fatalf("run() = %v", err)
	}

	// stdout is the machine interface: the addresses, in order, alone.
	if want := public + "?0\n" + public + "?1\n"; h.stdout.String() != want {
		t.Errorf("stdout = %q, want %q", h.stdout.String(), want)
	}
	for _, want := range []string{"  -> http://localhost:3000\n", "  -> http://localhost:4000\n"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
		}
	}

	// An address reaches one stream, so stderr names origins and no addresses.
	for _, addr := range []string{public + "?0", public + "?1"} {
		if got := strings.Count(h.stderr.String(), addr); got != 0 {
			t.Errorf("%s appears %d times on stderr, want 0:\n%s", addr, got, h.stderr.String())
		}
	}
}

// TestReportLogsWhatNothingListensOn pins what the run does with an origin
// nothing is listening on once the tunnel is up: it asks the router about the
// origins as typed, and logs a warning per origin named. It prints nothing to
// the console for it: that is left to the builder, in one place, later
// (#209), and stdout stays the address alone.
//
// The dial is the router's, and router_test.go dials a loopback listener and a
// closed port for it; the fake here answers from a list.
func TestReportLogsWhatNothingListensOn(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	for name, tc := range map[string]struct {
		unanswered []int
		args       []string
		logged     []string
	}{
		"everything answers":           {},
		"one does not, log off":        {unanswered: []int{1}},
		"one does not, logged at warn": {unanswered: []int{1}, args: []string{"--log-level", "warn"}, logged: []string{"http://localhost:4000"}},
		"none does, logged at warn":    {unanswered: []int{0, 1}, args: []string{"--log-level", "warn"}, logged: []string{"http://localhost:3000", "http://localhost:4000"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRunHarness(t, live(public), ":3000", ":4000")
			h.router.unanswered = tc.unanswered
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel
			if err := h.run(t, ctx, tc.args...); err != nil {
				t.Fatalf("run() = %v", err)
			}

			if h.router.probed == nil {
				t.Fatal("the run never asked the router what answers")
			}
			if got, want := urlStrings(h.router.probed.URLs()), []string{"http://localhost:3000", "http://localhost:4000"}; !slices.Equal(got, want) {
				t.Errorf("asked about %q, want the origins as typed %q", got, want)
			}
			out := h.stderr.String()
			if got := strings.Count(out, "nothing is listening"); got != len(tc.logged) {
				t.Errorf("stderr mentions nothing listening %d times, want %d:\n%s", got, len(tc.logged), out)
			}
			for _, origin := range tc.logged {
				if !strings.Contains(out, "origin="+origin) {
					t.Errorf("no warning names %s:\n%s", origin, out)
				}
			}
			if want := public + "\n"; h.stdout.String() != want {
				t.Errorf("stdout = %q, want the address alone", h.stdout.String())
			}
		})
	}
}

// TestMirroringTellsTheBrowser covers what a drawn console reports: the
// terminal is already on a screen the person is looking at, and a tab on top
// of it would be a second copy of the one thing they can already see, counted
// as another viewer and competing for the same keys.
//
// WithOpen(true) is asked for so the console is the only thing that can be
// suppressing the tab — otherwise a runner with $CI set would pass this for
// the wrong reason.
//
// A code asked for with --qr stays off that console too: Ctrl+K q is the code
// there, sized to the pane, and one printed first would only sit behind the
// frame.
//
// A real pty, because the console package asks whether the command's own
// streams are one and a buffer can never answer yes. Skipped where there is
// none, which is Windows.
func TestMirroringTellsTheBrowser(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty to draw on: %v", err)
	}
	t.Cleanup(func() { tty.Close(); ptmx.Close() })

	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), "attach://dockerd/my-container")
	h.binder.mirrors = true
	// Asked for, so the mirror is the only thing that can be suppressing it —
	// otherwise a runner with $CI set would pass this for the wrong reason.
	v1.Apply(h.b, WithOpen(true))
	h.console = tty
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx, "--qr"); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if len(h.display.opened) != 0 {
		t.Errorf("opened %q, want nothing — the console is already showing it", h.display.opened)
	}
	if strings.Contains(h.stderr.String(), "█") {
		t.Errorf("stderr carries a code under the frame:\n%s", h.stderr.String())
	}
}

// TestMirroringOutlivesARespec pins what a new spec does to a console the
// first tunnel drew a frame on: the frame stays — it shows the origins, which
// a new spec does not change — so it is not drawn again, and the spinner that
// greets a tunnel coming up stays off the console the frame owns, and so do
// the new address and the map: the frame was told them. The first tunnel, with
// nothing drawn yet, still gets its spinner and its report.
//
// A real pty for the command's stdin and stdout, as in
// TestMirroringTellsTheBrowser, read from its other end to see what reached
// the terminal; stderr stays a buffer, which is where the spinner and the map
// write, so what they drew can be read back.
func TestMirroringOutlivesARespec(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty to draw on: %v", err)
	}
	t.Cleanup(func() { tty.Close(); ptmx.Close() })
	var mu sync.Mutex
	var screen bytes.Buffer
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := ptmx.Read(b)
			mu.Lock()
			screen.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	const public, second = "https://foo.tunneled.pizza/", "https://bar.tunneled.pizza/"
	h := newRunHarness(t, live(public), "attach://dockerd/my-container")
	next := live(second)
	next.order = &h.order
	h.tunnels = append(h.tunnels, next)
	h.binder.mirrors = true
	h.console = tty
	h.cache.specs = make(chan string, 1)
	ctx, cancel := context.WithCancel(t.Context())
	saves, mark := 0, 0
	h.cache.onSave = func() {
		saves++
		if saves == 1 {
			mark = h.stderr.Len()
			go func() { h.cache.specs <- "patched" }()
			return
		}
		cancel()
	}

	if err := h.run(t, ctx); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if want := []string{"", "patched"}; !slices.Equal(h.specs, want) {
		t.Fatalf("minted from %q, want a respec", h.specs)
	}
	const spinner = "Creating tunnel..."
	if !strings.Contains(h.stderr.String()[:mark], spinner) {
		t.Errorf("no spinner before the first tunnel was up, want one: nothing was drawn yet")
	}
	if after := h.stderr.String()[mark:]; strings.Contains(after, spinner) || strings.Contains(after, "  -> ") {
		t.Errorf("stderr after the respec = %q, want no spinner and no map over the frame", after)
	}
	time.Sleep(50 * time.Millisecond) // what the run wrote, through the pty
	mu.Lock()
	drawn := screen.String()
	mu.Unlock()
	if !strings.Contains(drawn, "foo.tunneled.pizza") {
		t.Errorf("terminal = %q, want the first tunnel's address, printed before the frame", drawn)
	}
	if strings.Contains(drawn, "bar.tunneled.pizza") {
		t.Errorf("terminal = %q, want the second tunnel's address kept off the frame", drawn)
	}
	if want := []string{public, second}; !slices.Equal(h.binder.announcedAll, want) {
		t.Errorf("announced %q, want each tunnel's address told to the frame", h.binder.announcedAll)
	}
	if n := h.binder.shows.Load(); n != 1 {
		t.Errorf("frames drawn = %d, want 1: the respec keeps the one that is up", n)
	}
}

// TestOpenFalseShowsNothing covers the hammer. OPEN=false is a run told to
// show itself nowhere: the console keeps its scrollback and no tab is
// launched, which is what somebody watching the run's own log lines is asking
// for — the frame is drawn over exactly the output they are trying to read.
//
// Set up as the case that would otherwise do both. A pty on the command's
// streams is a console to draw on, a mirroring binder is a terminal to draw,
// and WithOpen(true) is a caller who insisted on a tab; each of the three is
// the thing the variable has to beat.
//
// The stop hint is the deterministic half of the verdict: it is printed on the
// run's own goroutine, and only for a run with no screen, so a mirror that
// started could not have printed it. showed is the direct check behind it, and
// it is read after the run has ended.
func TestOpenFalseShowsNothing(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty to draw on: %v", err)
	}
	t.Cleanup(func() { tty.Close(); ptmx.Close() })
	t.Setenv("OPEN", "false") // the hammer: see run.openEnv

	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), "attach://dockerd/my-container")
	h.binder.mirrors = true
	v1.Apply(h.b, WithOpen(true))
	h.console = tty
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if h.binder.showed.Load() {
		t.Error("the console drew the terminal, want a run that shows nothing")
	}
	if len(h.display.opened) != 0 {
		t.Errorf("opened %q, want a run that shows nothing", h.display.opened)
	}
	if got := strings.Count(h.stderr.String(), stopHint); got != 1 {
		t.Errorf("stop hint appears %d times on stderr, want 1:\n%s", got, h.stderr.String())
	}
}

// TestStopHintTellsAWaitingConsoleWhatToPress covers the line a run prints
// once there is nothing left for it to draw. It is chrome, so stderr — stdout
// is the machine interface and a script reading addresses off it should not
// have to skip prose — and it comes after the addresses, because reading the
// last one is what tells a person the run is up.
//
// The harness writes to buffers, not terminals, so mirrorable is false here
// and this is the branch under test. The mirrored branch says the same thing
// on its way out of the frame, which needs a pty and is covered by hand.
func TestStopHintTellsAWaitingConsoleWhatToPress(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000")
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if got := strings.Count(h.stderr.String(), stopHint); got != 1 {
		t.Errorf("stop hint appears %d times on stderr, want 1:\n%s", got, h.stderr.String())
	}
	if strings.Contains(h.stdout.String(), "Ctrl+C") {
		t.Errorf("stdout carries the hint, want addresses alone: %q", h.stdout.String())
	}
	origin, hint := strings.Index(h.stderr.String(), "  -> "), strings.Index(h.stderr.String(), stopHint)
	if origin < 0 || hint < origin {
		t.Errorf("hint at %d, first origin at %d, want the hint after it:\n%s", hint, origin, h.stderr.String())
	}
}

// TestLogger covers the level resolution: the --log-level value wins, the
// environment mirror is the fallback, and neither being set means silence.
// The flag is strict — an operator typo must not vanish — and the
// environment reaches RunE through that same flag, mirrored onto it by
// PersistentPreRunE, so it is strict too; TestEnvLogLevelIsStrict pins that
// end.
//
// Where the lines land is the other half: the command's own stderr writer,
// never the process's os.Stderr. An embedding program that called SetErr is
// watching the former, and a log line on the latter is one it cannot see.
// Driven through the whole run, because the logger is built inside Command's
// RunE; captureStderr guards the process stream so a leak there is a
// failure rather than invisible.
func TestLogger(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	cases := []struct {
		name    string
		level   string
		env     string
		wantErr error
		want    bool // "tunneld starting" present on the command's stderr
	}{
		{name: "unset is silent", want: false},
		{name: "flag sets the level", level: "debug", want: true},
		{name: "environment is the fallback", env: "debug", want: true},
		{name: "flag beats environment", level: "error", env: "debug", want: false},
		{name: "unparsable flag is an error", level: "loud", wantErr: v1.ErrInvalidLogLevel},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunHarness(t, live(public), ":3000")
			// newRunHarness already blanks TUNNELD_LOG; this is the case
			// under test, set after so it is not immediately erased.
			t.Setenv(v1.LogEnv, tc.env)

			var args []string
			if tc.level != "" {
				args = append(args, "--log-level", tc.level)
			}

			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel

			restore := captureStderr(t)
			cmd := h.b.Command()
			cmd.SetOut(io.Discard)
			cmd.SetErr(&h.stderr)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(ctx)
			leaked := restore()
			logged := h.stderr.String()

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("run() = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run() = %v, want nil", err)
			}
			if got := strings.Contains(logged, "tunneld starting"); got != tc.want {
				t.Errorf("command stderr contains %q = %v, want %v:\n%s", "tunneld starting", got, tc.want, logged)
			}
			if strings.Contains(leaked, "tunneld starting") {
				t.Errorf("the log reached os.Stderr instead of the command's writer:\n%s", leaked)
			}
		})
	}
}

// TestEnvErrorNamesTheLever pins the doc discipline in code: an environment
// override that is set but unparsable must fail loudly and name the variable
// and the offending value, since that is the whole lever an operator has to
// recover from the error.
func TestEnvErrorNamesTheLever(t *testing.T) {
	t.Setenv(v1.MultiviewEnv, "maybe")

	cmd := New(WithOrigin(":3000")).Command()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("ExecuteContext() = nil error for an unparsable MultiviewEnv value")
	}
	if !errors.Is(err, v1.ErrInvalidEnv) {
		t.Errorf("err = %v, want it to wrap v1.ErrInvalidEnv", err)
	}
	for _, want := range []string{v1.MultiviewEnv, "maybe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

// TestFlagEnvRegistryIsComplete pins that every flag the command binds has an
// environment mirror, and that each mirror names a v1 constant rather than a
// string invented here. A flag added without a row is the failure mode this
// catches: it would work on the command line and be silently unreachable from
// a container's environment.
func TestFlagEnvRegistryIsComplete(t *testing.T) {
	cmd := New().Command()

	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" { // cobra's own, no knob behind it
			return
		}
		if _, ok := flagEnv[f.Name]; !ok {
			t.Errorf("flag --%s has no entry in flagEnv, so it cannot be set from the environment", f.Name)
		}
	})

	want := map[string]string{
		"provider":  "TUNNELD_PROVIDER",
		"log-level": "TUNNELD_LOG",
		"qr":        "TUNNELD_QR",
	}
	for flag, env := range want {
		if got := flagEnv[flag]; got != env {
			t.Errorf("flagEnv[%q] = %q, want %q", flag, got, env)
		}
	}
}

// TestEnvListSplitting covers the parsing behind a list-valued variable:
// comma-separated, surrounding space trimmed, and empty entries dropped so a
// trailing comma does not become an origin nothing can proxy to.
//
// It exercises the function rather than a built command. Origins are arguments
// now, so there is no flag holding the parsed list to read back;
// TestApplyEnvPrecedence covers the variable reaching a run.
func TestEnvListSplitting(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"single value", "http://localhost:3000", []string{"http://localhost:3000"}},
		{"two values", "http://a:1,http://b:2", []string{"http://a:1", "http://b:2"}},
		{"space around separators", " http://a:1 , http://b:2 ", []string{"http://a:1", "http://b:2"}},
		{"trailing comma dropped", "http://a:1,", []string{"http://a:1"}},
		{"empty entries dropped", "http://a:1,,http://b:2", []string{"http://a:1", "http://b:2"}},
		{"empty string", "", []string{}},
		{"separators only", ",,", []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := splitList(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitList(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("item %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestApplyEnvPrecedence pins argv > environment > seed, one row per rung.
//
// Origins reports the settled list directly, so each row reads as what the
// three layers resolve to rather than as which parse error came back. The
// flags are parsed rather than executed — that is all Origins needs to see
// argv, and it keeps every row offline.
func TestApplyEnvPrecedence(t *testing.T) {
	cases := []struct {
		name string
		seed []string
		env  string
		args []string
		want []string
	}{
		{
			name: "the seed supplies the origin",
			seed: []string{"http://seed:1"},
			want: []string{"http://seed:1"},
		},
		{
			name: "the environment supplies the origin",
			env:  "http://env:1",
			want: []string{"http://env:1"},
		},
		{
			// Reaching the second entry proves the whole variable was split
			// and parsed rather than its first value taken.
			name: "the environment supplies several origins",
			env:  "http://env:1,http://env:2",
			want: []string{"http://env:1", "http://env:2"},
		},
		{
			name: "the environment replaces the seed",
			seed: []string{"http://seed:1"},
			env:  "http://env:1",
			want: []string{"http://env:1"},
		},
		{
			name: "an argument beats the environment",
			env:  "http://env:1", args: []string{"http://flag:1"},
			want: []string{"http://flag:1"},
		},
		{
			name: "an argument replaces the whole environment list",
			env:  "http://env:1,http://env:2", args: []string{"http://flag:1"},
			want: []string{"http://flag:1"},
		},
		{
			name: "an argument replaces the seed too",
			seed: []string{"http://seed:1", "http://seed:2"},
			args: []string{"http://flag:1"},
			want: []string{"http://flag:1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.OriginsEnv, tc.env)

			b := New(WithOrigin(tc.seed...))
			if err := b.Command().ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			if got := originStrings(b.Origins()); !slices.Equal(got, tc.want) {
				t.Errorf("Origins() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyEnvSeededDefault pins that the variable replaces a seeded origin
// rather than being ignored behind it — an embedder's seed is a default, and
// an operator's variable is an override.
func TestApplyEnvSeededDefault(t *testing.T) {
	t.Setenv(v1.OriginsEnv, "http://env:1")

	b := New(WithOrigin("http://seeded:1"))
	if got, want := originStrings(b.Origins()), []string{"http://env:1"}; !slices.Equal(got, want) {
		t.Errorf("Origins() = %q, want %q — the seed won", got, want)
	}
}

// TestApplyEnvIsPerBuilder pins that the binding is a viper instance per
// builder, not the package global: two commands in one process must not share
// a key space, or an embedded tunneld would inherit its host's configuration.
func TestApplyEnvIsPerBuilder(t *testing.T) {
	t.Setenv(v1.OriginsEnv, "http://env:1")

	first := New().Command()
	first.SetOut(io.Discard)
	first.SetErr(io.Discard)
	first.SetArgs([]string{"--log-level", "loud"})
	if err := first.ExecuteContext(t.Context()); !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
	}

	second := New().Command()
	second.SetOut(io.Discard)
	second.SetErr(io.Discard)
	second.SetArgs([]string{"--log-level", "loud"})
	if err := second.ExecuteContext(t.Context()); !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
	}

	// The global was never the binding: each builder's flags are satisfied
	// by its own viper instance, and the package-global one stays untouched.
	if viper.IsSet("url") {
		t.Error("the package-global viper was bound; want one instance per builder")
	}
}

// TestEnvLogLevelIsStrict pins that an unparsable level fails the same way
// whichever side it came from. Env beats code, so a typo'd variable that fell
// back silently would be indistinguishable from one that worked — the promise
// ErrInvalidEnv already makes for the other knobs.
func TestEnvLogLevelIsStrict(t *testing.T) {
	t.Setenv(v1.LogEnv, "loud")
	t.Setenv(v1.OriginsEnv, "http://localhost:3000")

	cmd := New().Command()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	if err := cmd.ExecuteContext(t.Context()); !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Errorf("error = %v, want ErrInvalidLogLevel", err)
	}
}

// TestRunCarriesTheIdentityToken pins the whole path: the list the flag
// settled reaches Identity, and what Identity found reaches the engine — and
// the credential reaches neither stream on the way.
func TestRunCarriesTheIdentityToken(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000")
	ident := &fakeIdentity{token: "a-credential"}
	v1.Apply(h.b, WithIdentity(ident), WithIdentityProviders("github"))
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx); err != nil {
		t.Fatalf("run() = %v", err)
	}

	want := [][]string{{"github"}}
	if !slices.EqualFunc(ident.asked, want, slices.Equal) {
		t.Errorf("Identity was asked for %v, want %v", ident.asked, want)
	}
	if !slices.EqualFunc(ident.known, want, slices.Equal) {
		t.Errorf("Known was asked %v, want %v", ident.known, want)
	}
	if got := h.tunnels[0].token; got != "a-credential" {
		t.Errorf("the engine got tokens %q, want one %q", got, "a-credential")
	}

	// The discipline the whole feature lives under, pinned where the
	// credential passes through the most code: it reaches the engine, and
	// neither stream.
	if strings.Contains(h.stderr.String(), "a-credential") {
		t.Errorf("the credential reached stderr: %q", h.stderr.String())
	}
	if strings.Contains(h.stdout.String(), "a-credential") {
		t.Errorf("the credential reached stdout: %q", h.stdout.String())
	}
}

// TestRunRefusesAnUnknownIdentityProvider pins that a typo stops the run
// before anything external happens — no tunnel asked for, and the message is
// the one Identity gave.
func TestRunRefusesAnUnknownIdentityProvider(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000")
	refused := fmt.Errorf("%w: %q, only %s", v1.ErrUnknownIdentity, "gitlab", "github")
	v1.Apply(h.b, WithIdentity(&fakeIdentity{err: refused}), WithIdentityProviders("gitlab"))

	err := h.run(t, t.Context())
	if !errors.Is(err, v1.ErrUnknownIdentity) {
		t.Fatalf("run() = %v, want it to wrap ErrUnknownIdentity", err)
	}
	if len(h.specs) != 0 {
		t.Errorf("the engine was asked for %d tunnels, want 0 — the run should stop first", len(h.specs))
	}
}

// TestIdentityProvidersSettle pins the precedence the other knobs have: the
// flag beats the variable, which beats the seed, and what reaches Identity is
// whichever won.
func TestIdentityProvidersSettle(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want []string
	}{
		{"the default", "", nil, []string{"github", "anthropic"}},
		{"the variable", "kubernetes,github", nil, []string{"kubernetes", "github"}},
		{"the flag beats it", "kubernetes", []string{"--identity-providers=github"}, []string{"github"}},
		{"empty turns it off", "", []string{"--identity-providers="}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.IdentityProvidersEnv, tc.env)
			const public = "https://foo.tunneled.pizza/"
			h := newRunHarness(t, live(public), ":3000")
			ident := &fakeIdentity{}
			v1.Apply(h.b, WithIdentity(ident))
			if err := h.b.Command().ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.args, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel

			if err := h.run(t, ctx); err != nil {
				t.Fatalf("run() = %v", err)
			}
			if len(ident.asked) != 1 || !slices.Equal(ident.asked[0], tc.want) {
				t.Errorf("Identity was asked for %v, want %v", ident.asked, tc.want)
			}
		})
	}
}

// TestTheDefaultProvidersAreRegistered pins the one literal this design
// duplicates: v1.DefaultIdentityProviders names providers that v1 cannot
// import, so nothing but a test keeps the two in step.
func TestTheDefaultProvidersAreRegistered(t *testing.T) {
	b := New()
	if err := b.identity.Known(splitList(v1.DefaultIdentityProviders)); err != nil {
		t.Errorf("the default list is not registered by New: %v", err)
	}
}

// TestCachingIsOnUnlessItIsTurnedOff pins the switch and everything that can
// throw it, and pins that throwing it leaves the builder alone: an embedder
// that runs a command twice gets its cache back on the second run.
func TestCachingIsOnUnlessItIsTurnedOff(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"

	for _, tc := range []struct {
		name  string
		args  []string
		env   string
		off   func(b *BuilderImpl)
		saved bool
	}{
		{name: "by default", saved: true},
		{name: "--no-cache", args: []string{"--no-cache"}},
		{name: "the variable", env: "true"},
		{name: "WithCache(nil)", off: func(b *BuilderImpl) { v1.Apply(b, WithCache(nil)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunHarness(t, live(public), ":3000")
			if tc.off != nil {
				tc.off(h.b)
			}
			if tc.env != "" {
				t.Setenv(v1.NoCacheEnv, tc.env)
			}

			// Announce is the signal, not the save: a run with no cache never
			// saves, and a case waiting on that would wait out its deadline.
			ctx, cancel := context.WithCancel(t.Context())
			h.binder.onAnnounce = cancel

			if err := h.run(t, ctx, tc.args...); err != nil {
				t.Fatalf("run() = %v, want nil after a signal", err)
			}
			if h.cache.saved != tc.saved {
				t.Errorf("cache saved = %v, want %v", h.cache.saved, tc.saved)
			}
			// And the banner says which it was: naming a key for a run that
			// caches nothing names a file that will never exist.
			if named := strings.Contains(h.stderr.String(), "cache "); named != tc.saved {
				t.Errorf("banner names a cache = %v, want %v:\n%s", named, tc.saved, h.stderr.String())
			}
		})
	}
}

// TestTheCacheIsToldWhatTheRunSettledOn pins the tracking half of a cache
// file: a name that is a hash says nothing about the run that wrote it, so
// every knob goes in beside the spec — settled, not read back out of the
// environment, since a run configured by flags sets none of these variables.
func TestTheCacheIsToldWhatTheRunSettledOn(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000", "http://localhost:4000")
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx, "--log-level", "debug", "--multiview=false"); err != nil {
		t.Fatalf("run() = %v, want nil after a signal", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for name, want := range map[string]string{
		"PWD":                   wd,
		"CMD":                   strings.Join(os.Args, " "),
		"TUNNELD_VERSION":       Version(),
		"LIBTUNNEL_VERSION":     libtunnel.Version(),
		"SHELL":                 os.Getenv("SHELL"),
		v1.OriginsEnv:           "http://localhost:3000,http://localhost:4000",
		v1.ProviderEnv:          "example.test",
		v1.LogEnv:               "debug",
		v1.MultiviewEnv:         "false",
		v1.ShellFallbackEnv:     "true",
		v1.NoCacheEnv:           "false",
		v1.IdentityProvidersEnv: strings.Join(splitList(v1.DefaultIdentityProviders), ","),
		v1.QREnv:                "false",
	} {
		if got := h.cache.tracking[name]; got != want {
			t.Errorf("tracking[%s] = %q, want %q", name, got, want)
		}
	}

	// The flags beat what the harness seeded, which is the whole reason this
	// is the settled value and not the seed.
	if got := h.cache.tracking[v1.LogEnv]; got != "debug" {
		t.Errorf("tracking[%s] = %q, want the flag's value", v1.LogEnv, got)
	}

	// Who and when, which is what a file whose name is a hash cannot say and
	// mtime only half answers: a rerun overwrites in place, so the file's own
	// clock is the only record of the run that wrote what is there now.
	if got, want := h.cache.tracking["PID"], strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("tracking[PID] = %q, want %q", got, want)
	}
	if got, want := h.cache.tracking["PPID"], strconv.Itoa(os.Getppid()); got != want {
		t.Errorf("tracking[PPID] = %q, want %q", got, want)
	}
	if host, err := os.Hostname(); err == nil {
		if got := h.cache.tracking["HOSTNAME"]; got != host {
			t.Errorf("tracking[HOSTNAME] = %q, want %q", got, host)
		}
	}
	if got := h.cache.tracking[ltv1.HostnameEnv]; got != "foo.tunneled.pizza" {
		t.Errorf("tracking[%s] = %q, want the tunnel's hostname", ltv1.HostnameEnv, got)
	}
	// The one value that must never be here: the mint token is the account's
	// where a spec is one tunnel's, and a map of "everything the run settled
	// on" is one careless entry away from carrying it.
	if _, ok := h.cache.tracking[ltv1.TokenEnv]; ok {
		t.Errorf("tracking carries %s", ltv1.TokenEnv)
	}

	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	for name, want := range map[string]string{"USERNAME": me.Username, "UID": me.Uid} {
		if got := h.cache.tracking[name]; got != want {
			t.Errorf("tracking[%s] = %q, want %q", name, got, want)
		}
	}

	ts, err := time.Parse(time.RFC3339, h.cache.tracking["TS"])
	if err != nil {
		t.Fatalf("tracking[TS] = %q, want an RFC 3339 time: %v", h.cache.tracking["TS"], err)
	}
	if since := time.Since(ts); since < 0 || since > time.Minute {
		t.Errorf("tracking[TS] = %s, %s ago — want the moment the run cached", ts, since)
	}
}

// TestFieldsSplitLikeAShell pins the small lexer a quoted program goes
// through: whitespace separates, either quote keeps a run whole, a backslash
// keeps the next character, an empty quoted string is a word, and an unclosed
// quote runs to the end rather than failing.
func TestFieldsSplitLikeAShell(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"python3 -m http.server 8000", []string{"python3", "-m", "http.server", "8000"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{`bash -c 'echo hi there'`, []string{"bash", "-c", "echo hi there"}},
		{`bash -c "echo \"quoted\" $x"`, []string{"bash", "-c", `echo "quoted" $x`}},
		{`printf a\ b`, []string{"printf", "a b"}},
		{`echo ''`, []string{"echo", ""}},
		{`echo 'unclosed`, []string{"echo", "unclosed"}},
		{"single", []string{"single"}},
		{"", nil},
	} {
		if got := fields(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("fields(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRunSettlesThePassword pins where a run's password comes from and when:
// the environment if the variable is set at all, else the cache file's line
// if it has one, set on auth before anything is routed, and recorded for the
// file. An invalid value stops the run, naming the variable.
func TestRunSettlesThePassword(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	const pw = `Basic pw="$pbkdf2-sha256$i=600000$dHVubmVsLnBpenphL3YwMQ$UFtjhDQ2L2Fb/DQXWXQx19Nx2YTuaTLDIhGHp3Vdn24"`

	runTo := func(t *testing.T, h *runHarness) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel
		if err := h.run(t, ctx); err != nil {
			t.Fatalf("run() = %v", err)
		}
	}

	t.Run("the environment beats the cache, and is set before routing", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		t.Setenv(v1.WWWAuthenticateEnv, pw)
		h.cache.mutable = map[string]string{v1.WWWAuthenticateEnv: ""}
		runTo(t, h)
		if h.auth.Value() != pw || !h.auth.setBeforeRoute {
			t.Errorf("auth = %q, set before route %v; want the environment's, before", h.auth.Value(), h.auth.setBeforeRoute)
		}
		if h.cache.recorded[v1.WWWAuthenticateEnv] != pw {
			t.Errorf("recorded %q", h.cache.recorded)
		}
	})
	t.Run("the environment's password is gone from the environment once read", func(t *testing.T) {
		// Every program the run starts (the shell a viewer types into among
		// them) inherits the environment; the hash is not theirs to read.
		h := newRunHarness(t, live(public), ":3000")
		t.Setenv(v1.WWWAuthenticateEnv, pw)
		runTo(t, h)
		if v, set := os.LookupEnv(v1.WWWAuthenticateEnv); set {
			t.Errorf("%s still set after the run read it: %q", v1.WWWAuthenticateEnv, v)
		}
		if h.auth.Value() != pw {
			t.Errorf("auth = %q, want the environment's", h.auth.Value())
		}
	})
	t.Run("set and empty in the environment is public, over a cached password", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		t.Setenv(v1.WWWAuthenticateEnv, "")
		h.cache.mutable = map[string]string{v1.WWWAuthenticateEnv: pw}
		runTo(t, h)
		if h.auth.Value() != "" || len(h.auth.sets) != 1 {
			t.Errorf("auth = %q after %q, want public, set deliberately", h.auth.Value(), h.auth.sets)
		}
	})
	t.Run("a cached password applies when the environment says nothing", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.cache.mutable = map[string]string{v1.WWWAuthenticateEnv: pw}
		runTo(t, h)
		if h.auth.Value() != pw {
			t.Errorf("auth = %q, want the cached password", h.auth.Value())
		}
	})
	t.Run("a cached empty line stays public", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.cache.mutable = map[string]string{v1.WWWAuthenticateEnv: ""}
		runTo(t, h)
		if h.auth.Value() != "" || h.cache.recorded[v1.WWWAuthenticateEnv] != "" {
			t.Errorf("auth = %q", h.auth.Value())
		}
	})
	t.Run("an invalid value in the environment stops the run, naming it", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		t.Setenv(v1.WWWAuthenticateEnv, "Basic nope")
		err := h.run(t, t.Context())
		if err == nil || !strings.Contains(err.Error(), v1.WWWAuthenticateEnv) {
			t.Errorf("err = %v, want one naming %s", err, v1.WWWAuthenticateEnv)
		}
	})
	t.Run("--no-cache runs on an in-memory cache, not noCache", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		ctx, cancel := context.WithCancel(t.Context())
		v1.Apply(h.b, WithPid(&fakePid{order: &h.order, onRegister: cancel}))
		_ = h.run(t, ctx, "--no-cache", ":3000")
		if _, ok := h.b.runCache.(*cache.CacheImpl); !ok {
			t.Errorf("runCache = %T, want *cache.CacheImpl (in memory)", h.b.runCache)
		}
	})
}

// TestWWWAuthenticateIsEnvironmentOnly pins that no command line can set a
// password: no flag, nothing in --help, nothing in flagEnv.
func TestWWWAuthenticateIsEnvironmentOnly(t *testing.T) {
	cmd := New().Command()
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if strings.Contains(strings.ToLower(f.Name), "auth") || strings.Contains(f.Usage, v1.WWWAuthenticateEnv) {
			t.Errorf("flag %q reaches the password", f.Name)
		}
	})
	if strings.Contains(cmd.UsageString(), v1.WWWAuthenticateEnv) {
		t.Error("--help mentions the variable")
	}
	for _, env := range flagEnv {
		if env == v1.WWWAuthenticateEnv {
			t.Error("flagEnv names the variable")
		}
	}
}

// TestRunLearnsAMessagesOnlySpec pins the provider rewording what it says
// about a running tunnel (the "publicly accessible" warning following a
// password): learned and saved in place, with no stop and no mint, and the
// save's own echo through the spec channel passed over quietly.
func TestRunLearnsAMessagesOnlySpec(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	const env = `{"backend":"cloudflare","spec":{"hostname":"foo.tunneled.pizza","secret":"c2VjcmV0"},"messages":["warn","tip"]}`
	const quieter = `{"backend":"cloudflare","spec":{"hostname":"foo.tunneled.pizza","secret":"c2VjcmV0"},"messages":["tip"]}`
	tun := live(public)
	tun.serialized = env
	h := newRunHarness(t, tun, ":3000")
	h.cache.specs = make(chan string, 1)
	ctx, cancel := context.WithCancel(t.Context())
	saves := 0
	h.cache.onSave = func() {
		saves++
		if saves == 1 {
			go func() {
				for len(h.cache.specs) > 0 { // the run takes its own save first
					time.Sleep(time.Millisecond)
				}
				h.cache.specs <- quieter
			}()
			return
		}
		// The messages-only save. Its echo lands on the channel too; give
		// the run a moment to pass it over, then end.
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	}
	if err := h.run(t, ctx, "--log-level", "info"); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if len(h.specs) != 1 {
		t.Errorf("minted %d times (%q), want once: no respec", len(h.specs), h.specs)
	}
	if !slices.Equal(h.motd.learned, []string{"tip"}) {
		t.Errorf("motd learned %q, want the new messages", h.motd.learned)
	}
	if h.cache.spec != quieter {
		t.Errorf("cached spec = %q, want the new envelope", h.cache.spec)
	}
	if strings.Contains(h.stderr.String(), "already has") {
		t.Errorf("the save's own echo was logged as a resend:\n%s", h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "the provider's messages changed") {
		t.Errorf("no log line for the change:\n%s", h.stderr.String())
	}
}

// TestAMessagesOnlySaveKeepsAWaitingSpec pins a respec PATCHed while the run
// is taking a messages-only spec: it was answered 200, so it is the run's to
// apply. The messages-only save must not take its place on the channel.
func TestAMessagesOnlySaveKeepsAWaitingSpec(t *testing.T) {
	const public, second = "https://foo.tunneled.pizza/", "https://bar.tunneled.pizza/"
	const env = `{"backend":"cloudflare","spec":{"hostname":"foo.tunneled.pizza","secret":"c2VjcmV0"},"messages":["warn","tip"]}`
	const quieter = `{"backend":"cloudflare","spec":{"hostname":"foo.tunneled.pizza","secret":"c2VjcmV0"},"messages":["tip"]}`
	const respec = `{"backend":"cloudflare","spec":{"hostname":"bar.tunneled.pizza","secret":"b3RoZXI="}}`
	tun := live(public)
	tun.serialized = env
	h := newRunHarness(t, tun, ":3000")
	next := live(second)
	next.order = &h.order
	h.tunnels = append(h.tunnels, next)
	h.cache.specs = make(chan string, 1)
	h.motd.onLearn = func() { h.cache.specs <- respec } // lands before the save
	ctx, cancel := context.WithCancel(t.Context())
	saves := 0
	h.cache.onSave = func() {
		saves++
		switch saves {
		case 1:
			go func() {
				for len(h.cache.specs) > 0 { // the run takes its own save first
					time.Sleep(time.Millisecond)
				}
				h.cache.specs <- quieter
			}()
		case 2: // the messages-only save
		default:
			cancel()
		}
	}
	go func() { time.Sleep(5 * time.Second); cancel() }()
	if err := h.run(t, ctx, "--log-level", "info"); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if len(h.specs) != 2 || h.specs[1] != respec {
		t.Errorf("minted from %q, want the waiting respec applied", h.specs)
	}
}

// TestAReSentSpecSettlesTheCache pins a PATCH of the spec the run already
// has: the run keeps its tunnel, and saves, so the cache stops refusing spec
// PATCHes as though one were still being applied.
func TestAReSentSpecSettlesTheCache(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	const env = `{"backend":"cloudflare","spec":{"hostname":"foo.tunneled.pizza","secret":"c2VjcmV0"}}`
	tun := live(public)
	tun.serialized = env
	h := newRunHarness(t, tun, ":3000")
	h.cache.specs = make(chan string, 1)
	ctx, cancel := context.WithCancel(t.Context())
	saves := 0
	h.cache.onSave = func() {
		saves++
		if saves == 1 {
			go func() {
				for len(h.cache.specs) > 0 { // the run takes its own save first
					time.Sleep(time.Millisecond)
				}
				h.cache.specs <- env
			}()
			return
		}
		cancel()
	}
	go func() { time.Sleep(5 * time.Second); cancel() }()
	if err := h.run(t, ctx, "--log-level", "info"); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if saves != 2 || h.cache.spec != env {
		t.Errorf("saves = %d, cached %q; want the re-sent spec settled by a second save", saves, h.cache.spec)
	}
	if len(h.specs) != 1 {
		t.Errorf("minted %d times, want once: the same spec is no respec", len(h.specs))
	}
}

// TestEveryMintSaysTheVisibility pins X-Tunneld-Authenticate on the mint
// request: the gate's public challenges, realm left out, when the tunnel is
// protected, and no header at all when it is public, so a provider treats a
// public run exactly as before.
func TestEveryMintSaysTheVisibility(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	const pw = `Basic pw="$pbkdf2-sha256$i=600000$dHVubmVsLnBpenphL3YwMQ$UFtjhDQ2L2Fb/DQXWXQx19Nx2YTuaTLDIhGHp3Vdn24"`
	for name, tc := range map[string]struct {
		env  string // "" leaves the variable unset
		want []string
	}{
		"protected": {pw, []string{v1.AuthenticateHeader + `: Basic charset="UTF-8"`}},
		"public":    {"", nil},
	} {
		t.Run(name, func(t *testing.T) {
			tun := live(public)
			h := newRunHarness(t, tun, ":3000")
			if tc.env != "" {
				t.Setenv(v1.WWWAuthenticateEnv, tc.env)
			}
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel
			if err := h.run(t, ctx); err != nil {
				t.Fatalf("run() = %v", err)
			}
			var got []string
			for _, hd := range tun.headers {
				if strings.HasPrefix(hd, v1.AuthenticateHeader+":") {
					got = append(got, hd)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("mint headers %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDotenvServesNoHash pins the builder's wiring of the password's
// redaction: a run's .env, as served, says the challenge in public form and
// never carries the hash, so nobody reading it (tunnel.pizza included)
// receives the password's hash.
func TestDotenvServesNoHash(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	const pw = `Basic pw="$pbkdf2-sha256$i=600000$dHVubmVsLnBpenphL3YwMQ$UFtjhDQ2L2Fb/DQXWXQx19Nx2YTuaTLDIhGHp3Vdn24"`
	h := newRunHarness(t, live(public), ":3000")
	t.Setenv(v1.WWWAuthenticateEnv, pw)
	ctx, cancel := context.WithCancel(t.Context())
	v1.Apply(h.b, WithPid(&fakePid{order: &h.order, onRegister: cancel}))
	_ = h.run(t, ctx, "--no-cache", ":3000")
	serve := h.b.runCache.Handlers(router.ControlPath)[router.ControlPath+".env"]
	rec := httptest.NewRecorder()
	serve(rec, httptest.NewRequest("GET", router.ControlPath+".env", nil))
	body := rec.Body.String()
	if rec.Code != 200 || strings.Contains(body, "pbkdf2") || !strings.Contains(body, v1.WWWAuthenticateEnv+`='Basic charset="UTF-8"'`) {
		t.Errorf("GET .env = %d %q, want the public form and no hash", rec.Code, body)
	}
}
