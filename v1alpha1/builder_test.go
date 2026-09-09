package v1alpha1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cnuss/libtunnel"
	"github.com/creack/pty"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell"
	"github.com/tunnel-pizza/tunneld/v1alpha1/browser"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
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
		{"unknown log level", []string{"http://localhost:3000", "--log-level", "loud"}, v1.ErrInvalidLogLevel},
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
	if !strings.Contains(stdout, "expose [origin ...]") {
		t.Errorf("help %q does not use the configured command name", stdout)
	}
}

// TestBrowserOpensWhenSomebodyIsWatching covers the run's half of the browser
// decision. The decision itself is browser.TestOpenDecides; what is pinned
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
		want     bool
	}{
		{name: "a pipe is nobody watching", want: false},
		{name: "a terminal is somebody", terminal: true, want: true},
		{name: "a caller who declined outranks the terminal", open: ptr(false), terminal: true, want: false},
		{name: "a caller who insisted outranks the pipe", open: ptr(true), want: true},
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

			if err := h.run(t, ctx); err != nil {
				t.Fatalf("run() = %v", err)
			}
			if got := len(h.browser.opened) > 0; got != tc.want {
				t.Errorf("opened %q, want a browser: %v", h.browser.opened, tc.want)
			}
		})
	}
}

// ptr is a *bool for a literal, which When.Forced needs and Go has no spelling
// for inline.
func ptr(b bool) *bool { return &b }

// TestCacheDir pins how a cache directory list is read: which spellings mean
// the working directory, that entries become absolute before repeats collapse,
// and the flag > variable > seed precedence every other knob follows.
//
// A temporary working directory stands in for the image's /var/run/tunneld —
// a test cannot rely on that path existing, and every rule here is about the
// working directory rather than that particular one.
//
// Paths are written as "$WD" (that working directory), "$OTHER" (a second
// one) and "$DEFAULT" (wherever an unconfigured run caches), in the inputs as
// well as the wants. $DEFAULT is read off a builder that was asked for
// nothing rather than recomputed here, so the table says "the same place the
// default goes" without restating how that place is chosen —
// TestDefaultCacheDir pins the choosing. Spelling them "/tmp" would be a
// POSIX assumption: filepath.Abs turns a rooted path into C:\tmp on Windows,
// so a literal want would be right on one platform and wrong on the other.
func TestCacheDir(t *testing.T) {
	cases := []struct {
		name string
		seed []string
		env  string
		args []string
		want []string
	}{
		{name: "the default location when nothing asks", want: []string{"$DEFAULT"}},
		{
			// "." is the working directory and "true" is the default, which
			// are different places now that the default moved out of the
			// checkout — so this collapses nothing and names three.
			name: "a dot and a true are two different directories",
			env:  ".,true,$OTHER",
			want: []string{"$WD", "$DEFAULT", "$OTHER"},
		},
		{
			name: "every spelling of true is the default location",
			env:  "1,t,T,TRUE,True,true",
			want: []string{"$DEFAULT"},
		},
		{name: "an empty entry is the default too", args: []string{"--cache-dir", ""}, want: []string{"$DEFAULT"}},
		{name: "a relative path becomes absolute", env: "sub", want: []string{"$WD/sub"}},
		{
			// Both halves are instructions. Without the false half, turning
			// the knob off would silently cache into a directory named
			// "false" in whatever directory the process started from.
			name: "false turns it off rather than naming a directory",
			env:  "false",
		},
		{
			name: "every spelling of false turns it off",
			env:  "0,f,F,FALSE,False,false",
		},
		{
			// One false entry is a master switch: it drops what came before
			// it and stops what would come after, so where in the list it
			// appears cannot change what off means.
			name: "a false entry disables the whole list",
			env:  ".,false,$OTHER",
		},
		{name: "false first disables the rest", env: "false,$OTHER"},
		{name: "false last disables what came before", args: []string{"--cache-dir", "$OTHER", "--cache-dir", "false"}},
		{
			// Sticky within one source: a directory named after the switch
			// cannot quietly turn it back on.
			name: "a later directory cannot re-enable it",
			args: []string{"--cache-dir", "false", "--cache-dir", "$OTHER"},
		},
		{
			// Command fills an unset list with the working directory, and has
			// to tell "unset" from "emptied on purpose" to leave this one
			// alone. Nothing else in this table separates the two.
			name: "a seed that disabled it is not re-filled by the default",
			seed: []string{"false"},
		},
		{
			// A later source still overrides, the same as any other value.
			name: "the flag overrides a variable that disabled it",
			env:  "false", args: []string{"--cache-dir", "$OTHER"},
			want: []string{"$OTHER"},
		},
		{
			name: "the variable overrides a seed that disabled it",
			seed: []string{"false"}, env: "$OTHER",
			want: []string{"$OTHER"},
		},
		{
			// ParseBool has never accepted these, and this knob does not
			// invent them: they are paths.
			name: "yes and no are paths",
			env:  "yes,no",
			want: []string{"$WD/yes", "$WD/no"},
		},
		{
			// Splitting a comma-separated environment value drops empty
			// entries, so a stray or trailing comma is not a cache directory.
			name: "stray commas are not entries",
			env:  ",,$OTHER,",
			want: []string{"$OTHER"},
		},
		{
			name: "repeated flags append",
			args: []string{"--cache-dir", "$OTHER/a", "--cache-dir", "$OTHER/b", "--cache-dir", "$OTHER/a"},
			want: []string{"$OTHER/a", "$OTHER/b"},
		},
		{
			name: "the flag beats the variable",
			env:  "$OTHER/env", args: []string{"--cache-dir", "$OTHER/flag"},
			want: []string{"$OTHER/flag"},
		},
		{name: "a seed stands when nothing overrides it", seed: []string{"$OTHER/seeded"}, want: []string{"$OTHER/seeded"}},
		{
			// Both overrides replace the seed rather than extending it, which
			// is the rule origins follow: a command line never merges into a
			// seeded set, and neither does the environment.
			name: "the variable replaces a seed",
			seed: []string{"$OTHER/seeded"}, env: "$OTHER",
			want: []string{"$OTHER"},
		},
		{
			name: "the flag replaces a seed",
			seed: []string{"$OTHER/seeded"}, args: []string{"--cache-dir", "$OTHER/a", "--cache-dir", "$OTHER/b"},
			want: []string{"$OTHER/a", "$OTHER/b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			workdir, err := os.Getwd()
			if err != nil {
				t.Fatalf("Getwd: %v", err)
			}
			other := t.TempDir()

			// What an unconfigured run resolves to, from this working
			// directory. Command seeds the default; nothing is executed, so
			// the environment is not applied to it.
			probe := New(WithOrigin("http://localhost:3000"))
			dflt := probe.Command().Flags().Lookup("cache-dir").Value.(pflag.SliceValue).GetSlice()[0]

			// The same substitution on inputs and wants, so a path is written
			// once and means the same thing on either platform.
			resolve := func(s string) string {
				r := strings.NewReplacer("$WD", workdir, "$OTHER", other, "$DEFAULT", dflt)
				return filepath.FromSlash(r.Replace(s))
			}
			resolveAll := func(in []string) []string {
				out := make([]string, 0, len(in))
				for _, s := range in {
					out = append(out, resolve(s))
				}
				return out
			}

			t.Setenv(v1.CacheDirEnv, resolve(tc.env))

			opts := []Option{WithOrigin("http://localhost:3000")}
			if len(tc.seed) > 0 {
				opts = append(opts, WithCacheDir(resolveAll(tc.seed)...))
			}
			b := New(opts...)
			// A deliberately bad level stops the run once the flags have
			// settled, before anything dials.
			args := append(resolveAll(tc.args), "--log-level", "loud")
			if _, _, err := execute(t, b, args...); !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
			}

			// GetSlice rather than GetStringArray: the latter round-trips
			// through a comma-separated String(), which splits any path that
			// contains a comma — and t.TempDir builds one out of the subtest
			// name.
			got := b.Command().Flags().Lookup("cache-dir").Value.(pflag.SliceValue).GetSlice()
			if want := resolveAll(tc.want); !slices.Equal(got, want) {
				t.Errorf("--cache-dir = %v, want %v", got, want)
			}
		})
	}
}

// TestMultiviewDefaultsOn pins the panel's default and both levers that turn
// it off. The default is the point of the flag: someone exposing three
// services wants to see three services.
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

// TestDefaultCacheDir pins where an unconfigured run caches, which is the one
// thing the table above takes as given.
//
// The rule that matters is the first: a spec is credentials, and the working
// directory is usually a repository. No filename is reliably ignored there —
// against GitHub's 239 gitignore templates the best candidate managed 13%, and
// against 752 real ones, 26% — so the only safe answer is not to write into a
// checkout at all.
func TestDefaultCacheDir(t *testing.T) {
	dflt := func(t *testing.T) string {
		t.Helper()
		b := New(WithOrigin("http://localhost:3000"))
		return b.Command().Flags().Lookup("cache-dir").Value.(pflag.SliceValue).GetSlice()[0]
	}

	t.Run("is not inside the working directory", func(t *testing.T) {
		t.Chdir(t.TempDir())
		workdir, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		got := dflt(t)
		if rel, err := filepath.Rel(workdir, got); err == nil && !strings.HasPrefix(rel, "..") {
			t.Errorf("default %q is inside the working directory %q", got, workdir)
		}
	})

	t.Run("is under the user cache directory", func(t *testing.T) {
		t.Chdir(t.TempDir())
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Skipf("no user cache directory: %v", err)
		}
		if got := dflt(t); !strings.HasPrefix(got, cache) {
			t.Errorf("default %q is not under %q", got, cache)
		}
	})

	// Two projects on one machine are two tunnels, so the working directory
	// has to reach the name — which is the whole reason it is fingerprinted
	// rather than fixed.
	t.Run("a different working directory is a different cache", func(t *testing.T) {
		first := func() string { t.Chdir(t.TempDir()); return dflt(t) }
		a, b := first(), first()
		if a == b {
			t.Errorf("two working directories share a cache: %q", a)
		}
	})
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
	ics    []libtunnel.Interceptor
	listen func(libtunnel.Event)
	order  *[]string

	// silent is a tunnel that comes up without ever announcing a connection,
	// which is what the wait before the browser has to survive.
	silent bool
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
	f.done <- f
	close(f.done)
}

func (f *fakeTunnel) URL() *url.URL {
	if f.order != nil {
		*f.order = append(*f.order, "url")
	}
	// A tunnel that has a URL has been accepted by the edge, so it announces
	// the connection the run waits for before it opens a browser. A silent
	// tunnel is the case where that announcement never comes.
	if f.url != nil && f.listen != nil && !f.silent {
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

func (f *fakeTunnel) Err() error                                     { return f.err }
func (f *fakeTunnel) Done() <-chan libtunnel.TunnelV1                { return f.done }
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

// fakeCache answers Load with a fixed spec and records the rest. onSave is
// how a case ends the run: cancelling the context, failing the tunnel, or
// delivering verdicts, all after the URL is live.
type fakeCache struct {
	cached    string
	saved     bool
	discarded bool
	onSave    func()
	order     *[]string
}

func (f *fakeCache) Load([]string, v1.Logger) string { return f.cached }
func (f *fakeCache) Discard([]string, v1.Logger)     { f.discarded = true }
func (f *fakeCache) Save([]string, v1.Logger) {
	f.saved = true
	if f.order != nil {
		*f.order = append(*f.order, "save")
	}
	if f.onSave != nil {
		f.onSave()
	}
}

// fakeBrowser records what it was asked to open and opens nothing. Only the
// launch is faked: the panel half of the contract is the real one, embedded,
// because the address TestRun expects reported and opened is the one the
// panel computes.
// fakeBrowser is the real browser with a launcher that records instead of
// launching. It is not a stub: deciding whether to open is BrowserImpl's, and
// a stub that skipped that decision would let a case assert "nothing opened"
// while asserting nothing at all.
type fakeBrowser struct {
	*browser.BrowserImpl
	opened []string
	order  *[]string
}

func (f *fakeBrowser) Open(ctx context.Context, log v1.Logger, opts ...browser.Option) {
	// The recorder goes on first so the run's own options still win, and the
	// effect is recorded where it actually happens: a launch that the
	// decision declined never reaches this.
	f.BrowserImpl.Open(ctx, log, append([]browser.Option{
		browser.WithLaunch(func(addr string) error {
			f.opened = append(f.opened, addr)
			if f.order != nil {
				*f.order = append(*f.order, "open")
			}
			return nil
		}),
	}, opts...)...)
}

// fakeBinder stands in for the attach package: it hands display back
// unchanged and closes nothing, so TestRun never stands up a real listener.
type fakeBinder struct {
	err    error
	closed bool
	// asked is what a viewer's exit closes, and what Done hands back: how the
	// run learns a terminal asked it to stop.
	asked chan struct{}
	// announced is what Announce was handed.
	announced []string
	// mirrors makes what Bind returns carry Show, which is how the real
	// binder reports a single served origin — the only shape a console can
	// draw.
	mirrors bool
}

func (f *fakeBinder) Bind(_ context.Context, display []*url.URL, _ v1.Logger) ([]*url.URL, attach.Bound, error) {
	// Carrying Mirror is how the real binder says a run has exactly one
	// served origin, so it is a wrapper here too rather than a method on the
	// binder itself: a fake that always carried it would mirror every case
	// that happens to have a terminal, and a browser would never open.
	if f.mirrors {
		return display, mirrorableBinder{f}, f.err
	}
	return display, f, f.err
}

// mirrorableBinder is a bound closer with a terminal to draw, which is what
// the binder hands back for a single served origin.
type mirrorableBinder struct{ *fakeBinder }

// Mirror blocks until the run ends, like the real one, so a case can assert on
// what happened while it was drawing.
func (mirrorableBinder) Show(ctx context.Context, _ io.Reader, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeBinder) Close() error { f.closed = true; return nil }

func (f *fakeBinder) Done() <-chan struct{} { return f.asked }

// Announce is what a bound closer is told the public addresses through. Every
// one of them can be, which is why it is on the type Bind returns rather than
// an interface a caller has to go looking for.
func (f *fakeBinder) Announce(public []string) { f.announced = public }

// runHarness is run with every collaborator faked except the two that are
// pure: the browser's panel half, because its URL and interceptor order are
// what the assertions check, and a real counter armed at one, because the
// verdict logic is what the gone case is about. order records the effects
// that matter in the sequence they landed.
type runHarness struct {
	engine  *fakeEngine
	cache   *fakeCache
	browser *fakeBrowser
	binder  *fakeBinder
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
	t.Setenv(v1.LogEnv, "") // a developer's shell must not turn the log on
	h := &runHarness{engine: &fakeEngine{tunnels: []*fakeTunnel{tun}}}
	h.cache = &fakeCache{order: &h.order}
	h.browser = &fakeBrowser{BrowserImpl: browser.New(), order: &h.order}
	h.binder = &fakeBinder{}
	tun.order = &h.order
	h.b = New(
		WithEstablishDeadline(50*time.Millisecond),
		WithOrigin(urls...),
		WithProvider("example.test"),
		WithCacheDir(t.TempDir()), // run consults the cache only with a directory
		WithEngine(h.engine),
		WithCache(h.cache),
		WithBrowser(h.browser),
		WithBinder(h.binder),
		// A short connect bound so no case can sit on the production default
		// waiting for an announcement its tunnel may never make.
		WithCounter(counter.New(counter.WithMaxGone(1))),
	)
	return h
}

// run executes the built command with args, the command's stderr captured
// in h.stderr and every TUNNELD_ mirror blanked so the developer's shell
// cannot reach in. It is what TestRun drives now that run's body lives in
// Command's RunE.
func (h *runHarness) run(t *testing.T, ctx context.Context, args ...string) error {
	t.Helper()
	for _, name := range []string{v1.OriginsEnv, v1.ProviderEnv, v1.CacheDirEnv, v1.LogEnv, v1.MultiviewEnv} {
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
// for the edge, the disk, the browser and the daemon. Each case asserts what
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
		if want := []string{""}; !slices.Equal(h.engine.specs, want) {
			t.Errorf("engine asked for specs %q, want a single mint", h.engine.specs)
		}
		if want := []string{"example.test"}; !slices.Equal(h.engine.providers, want) {
			t.Errorf("engine was handed providers %q, want %q — --provider did not reach the mint", h.engine.providers, want)
		}
		if want := []string{"url", "open", "save"}; !slices.Equal(h.order, want) {
			t.Errorf("effects in order %v, want %v — the cache must not be written before the URL is live", h.order, want)
		}
		if want := []string{public}; !slices.Equal(h.browser.opened, want) {
			t.Errorf("opened %q, want the panel address %q", h.browser.opened, want)
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
		tun := h.engine.tunnels[0]
		if len(tun.ics) != 2 || tun.ics[0].Priority >= tun.ics[1].Priority {
			t.Errorf("registered %d interceptors, want the page then the unframer", len(tun.ics))
		}
		if len(tun.locals) != 2 {
			t.Errorf("tunnel was given %d origins, want 2", len(tun.locals))
		}
		if !h.binder.closed {
			t.Error("the binder's closer was never called; RunE's defer did not run")
		}
	})

	t.Run("a binder failure is returned before the engine is asked for anything", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000")
		h.binder.err = errors.New("no such container")

		err := h.run(t, t.Context())
		if !errors.Is(err, h.binder.err) {
			t.Errorf("run() = %v, want the binder's own error", err)
		}
		if len(h.engine.specs) != 0 {
			t.Errorf("engine was asked for %d specs, want none", len(h.engine.specs))
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

		if err := h.run(t, ctx); err != nil {
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

		err := h.run(t, t.Context())
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
		if want := []string{public}; !slices.Equal(h.browser.opened, want) {
			t.Errorf("opened %q, want the plain URL %q", h.browser.opened, want)
		}
		if n := len(h.engine.tunnels[0].ics); n != 0 {
			t.Errorf("registered %d interceptors for one origin, want none", n)
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

	// TestEvents folded in: the gone case delivers three verdicts, pinning
	// the once.Do latch in the inlined event listener — without it every
	// verdict would repeat the "disowned" log line and cancel again. Driven
	// through the whole run rather than the closure directly, because
	// events no longer exists as a callable method once it is inlined into
	// Command's RunE; the listener's logger writes to the command's stderr,
	// which the harness captures.
	t.Run("enough gone verdicts end the run with ErrTunnelGone", func(t *testing.T) {
		tun := live(public)
		h := newRunHarness(t, tun, ":3000")
		h.cache.onSave = func() {
			for range 3 {
				tun.listen(libtunnel.Event{Kind: libtunnel.EventGone, Hostname: "foo.tunneled.pizza"})
			}
		}
		err := h.run(t, t.Context(), "--log-level", "error")
		logged := h.stderr.String()

		if !errors.Is(err, v1.ErrTunnelGone) {
			t.Fatalf("run() = %v, want ErrTunnelGone", err)
		}
		if !strings.Contains(err.Error(), "foo.tunneled.pizza") {
			t.Errorf("error %q does not name the hostname", err)
		}
		if got := strings.Count(logged, "disowned"); got != 1 {
			t.Errorf("logged the verdict %d times, want once:\n%s", got, logged)
		}
	})

	t.Run("a bare struct is refused before it touches anything", func(t *testing.T) {
		// The wiring check runs at the top of Command, ahead of the
		// cacheDirs read a few lines down that needs the field — a bare
		// BuilderImpl never reaches that read, so Command hands back a
		// minimal command whose only job is to report the error, rather
		// than binding flags against a nil collaborator. cacheDirs is
		// checked first, so a bare struct's error names it.
		b := &BuilderImpl{origins: []string{":3000"}}
		cmd := b.Command()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(nil)
		err := cmd.ExecuteContext(t.Context())
		if err == nil || !strings.Contains(err.Error(), "cacheDirs") || !strings.Contains(err.Error(), "construct it with New") {
			t.Errorf("run() on a bare BuilderImpl = %v, want the wiring error naming cacheDirs", err)
		}
	})
}

// TestParseOriginsAccepts covers the shapes a caller is allowed to type,
// including the bare host:port that implies http — the affordance that lets
// `tunneld localhost:3000` work the way people expect. Driven through the
// whole run (the origin parsing has no seam of its own to call directly), so
// what is pinned is what the tunnel is actually given: tun.locals, in order.
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
		{"a container by name", []string{"dockerd://api"}, []string{"dockerd://api"}},
		// Explicit, so it is taken as typed rather than looked up: an origin
		// that names a program this machine does not have is the binder's to
		// refuse, before the mint and with the reason.
		{"a program by name", []string{"file://htop"}, []string{"file://htop"}},
		// The shape the parser produces for itself when it resolves a bare
		// word, so it has to read it back: an absolute path cannot be a URL's
		// authority, only its path.
		{"a program by path", []string{"file:///usr/bin/top"}, []string{"file:///usr/bin/top"}},
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
			h := newRunHarness(t, live(public), tc.in...)
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel

			if err := h.run(t, ctx); err != nil {
				t.Fatalf("run(%q) = %v, want ok", tc.in, err)
			}
			got := h.engine.tunnels[0].locals
			if len(got) != len(tc.want) {
				t.Fatalf("run(%q) gave the tunnel %d origins, want %d", tc.in, len(got), len(tc.want))
			}
			for i, u := range got {
				if u.String() != tc.want[i] {
					t.Errorf("origin %d = %q, want %q", i, u, tc.want[i])
				}
			}
		})
	}
}

// originStrings renders parsed origins for comparison, so a table can be
// written the way somebody types origins rather than as *url.URL literals.
func originStrings(origins []*url.URL) []string {
	got := make([]string, len(origins))
	for i, u := range origins {
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
	for _, name := range []string{v1.OriginsEnv, v1.CacheDirEnv, v1.MultiviewEnv} {
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
	if !slices.Contains(h.engine.providers, "from-the-environment.test") {
		t.Errorf("engine saw providers %v, want the one the environment set", h.engine.providers)
	}
}

// TestOriginsFallsBackToTheShell covers the answer to being given nothing:
// the one origin every machine has. It is resolved before it is adopted,
// because the parse loop's fallback for an unresolvable word is to read it as
// an address — so an unrunnable $SHELL has to leave the count at zero and get
// the message that names the lever, not become a proxy to localhost.
//
// A seed or an argument outranks it: the fallback is for having nothing, and
// anything settled above is something.
func TestOriginsFallsBackToTheShell(t *testing.T) {
	// Resolved the way Origins resolves it, so the case pins where $SHELL
	// ends up rather than re-deriving how a path is spelled — LookPath
	// answers a PATH hit absolutely and Resolve makes it absolute again, and
	// on Windows the answer is C:\Program Files\Git\usr\bin\sh.exe.
	real, ok := shell.Resolve("sh")
	if !ok {
		t.Skip("no sh on PATH to fall back to")
	}
	// Compared as fields rather than as strings, because a Windows shell is
	// C:\Program Files\Git\usr\bin\sh.exe and url.URL.String escapes every
	// separator in it — file://C:%5CProgram%20Files%5C... is correct and
	// nothing anybody would write down. Path is the claim worth pinning
	// anyway: the resolved program, not the word that named it.
	for _, tc := range []struct {
		name     string
		shell    string
		fallback bool
		args     []string
		want     []*url.URL
		mention  string
	}{
		{"a runnable shell is the origin", real, true, nil, []*url.URL{{Scheme: v1.FileScheme, Path: real}}, ""},
		{"an unrunnable one is dropped", filepath.Join(t.TempDir(), "nope"), true, nil, nil, "not exposing a shell"},
		{"unset is nothing to fall back to", "", true, nil, nil, ""},
		{"an argument outranks it", real, true, []string{":3000"}, []*url.URL{{Scheme: "http", Host: "localhost:3000"}}, ""},
		// The knob is asked before the variable is read, so a shell that is
		// there and runnable is still not an origin when nobody wanted one.
		{"the option declines it", real, false, nil, nil, ""},
		{"declining does not touch an argument", real, false, []string{":3000"}, []*url.URL{{Scheme: "http", Host: "localhost:3000"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.OriginsEnv, "") // a developer's shell must not seed this
			t.Setenv("SHELL", tc.shell)
			var stderr bytes.Buffer
			b := New(WithLogLevel("warn"), WithStderr(&stderr), WithShellFallback(tc.fallback))
			if err := b.Command().ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.args, err)
			}

			got := b.Origins()
			if !slices.EqualFunc(got, tc.want, func(a, b *url.URL) bool { return *a == *b }) {
				t.Errorf("Origins() = %+v, want %+v", got, tc.want)
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
			[]string{"http+ws://localhost:4000", "http://localhost:5173"},
			"already claimed",
		},
		{"the marker on a container", []string{"dockerd+ws://api"}, nil, "dockerd+ws"},
		{"the marker on an unproxyable scheme", []string{"ftp+ws://localhost:21"}, nil, "ftp+ws"},
		{"a container with no name", []string{"dockerd://"}, nil, "names no container"},
		{"a program with no name", []string{"file://"}, nil, "names no program"},
		{"a program with a path", []string{"file://htop/now"}, nil, "carries more than a program reference"},
		{"a container with a path", []string{"dockerd://api/sh"}, nil, "dockerd://api"},
		{"a container with a query", []string{"dockerd://api?tty=1"}, nil, "dockerd://api"},
		{"a container with a fragment", []string{"dockerd://api#sh"}, nil, "dockerd://api"},
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

	b := New(WithOrigin(name, "http://localhost:3000", "dockerd://api"))
	got := b.Origins()

	// The origin carries the resolved path, not the word typed: "top" names a
	// program only on the machine that looked it up, and the frame, the
	// reported map and a pasted-back copy all read this.
	want := filepath.Join(dir, name)
	if len(got) != 3 || got[0].Scheme != v1.FileScheme || got[0].Path != want {
		t.Fatalf("Origins() = %q, want the first to be %s://%s", originStrings(got), v1.FileScheme, want)
	}
	// And it survives being written out and read back, which is the promise
	// the frame makes when it puts the origin in its corner.
	//
	// A Unix promise only: a Windows absolute path is C:\..., which has no
	// spelling inside a file:// URL — url.URL escapes the separators either
	// way round. It costs nothing there, because a platform with no
	// pseudo-terminals refuses a program origin at startup regardless, and the
	// binder reads the path off the URL rather than off its printed form.
	if runtime.GOOS != "windows" {
		again, err := url.Parse(got[0].String())
		if err != nil {
			t.Fatalf("%q did not parse back: %v", got[0], err)
		}
		if again.Path != want {
			t.Errorf("%q parsed back to path %q, want %q", got[0], again.Path, want)
		}
	}
	if rest := originStrings(got[1:]); !slices.Equal(rest, []string{"http://localhost:3000", "dockerd://api"}) {
		t.Errorf("the other origins = %q, want them untouched", rest)
	}
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

// TestRunOutlastsATunnelThatNeverConnects pins that the wait before the
// browser cannot strand what comes after it.
//
// A run holds for the edge to accept a connection before opening a page,
// because a tunnel is ready a moment before the edge has registered its route
// and a browser opened into that moment shows an error for a tunnel that
// works. Everything after that wait is behind it — the cache save, and the
// select that keeps the process alive — so a tunnel that comes up without ever
// announcing a connection must still reach all of it. The counter's deadline
// is what guarantees that, and this is the run that would hang without one.
func TestRunOutlastsATunnelThatNeverConnects(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	tun := live(public)
	tun.silent = true

	h := newRunHarness(t, tun, ":3000")
	v1.Apply(h.b, WithOpen(true)) // its streams are buffers; say so out loud
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if want := []string{"url", "open", "save"}; !slices.Equal(h.order, want) {
		t.Errorf("effects in order %v, want %v — the wait swallowed what follows it", h.order, want)
	}
	if want := []string{public}; !slices.Equal(h.browser.opened, want) {
		t.Errorf("opened %q, want the address anyway %q", h.browser.opened, want)
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

// TestMirroringTellsTheBrowser covers what a drawn console reports: the
// terminal is already on a screen the person is looking at, and a tab on top
// of it would be a second copy of the one thing they can already see, counted
// as another viewer and competing for the same keys.
//
// WithOpen(true) is asked for so the console is the only thing that can be
// suppressing the tab — otherwise a runner with $CI set would pass this for
// the wrong reason.
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
	h := newRunHarness(t, live(public), "dockerd://my-container")
	h.binder.mirrors = true
	// Asked for, so the mirror is the only thing that can be suppressing it —
	// otherwise a runner with $CI set would pass this for the wrong reason.
	v1.Apply(h.b, WithOpen(true))
	h.console = tty
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if len(h.browser.opened) != 0 {
		t.Errorf("opened %q, want nothing — the console is already showing it", h.browser.opened)
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
