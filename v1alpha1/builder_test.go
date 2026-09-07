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
	"slices"
	"strings"
	"testing"

	"github.com/cnuss/libtunnel"
	"github.com/spf13/pflag"
	v1 "github.com/tunnel-pizza/tunneld/v1"
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
// binds over, observed the only way an outsider can: through the built
// command. Name through Name, the writers through cobra, the rest through
// the flag defaults.
func TestOptionsLand(t *testing.T) {
	var sink bytes.Buffer
	b := New(
		WithName("expose"),
		WithURL("http://localhost:3000"),
		WithURL("http://localhost:4000"),
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
	for flag, want := range map[string]string{
		"url":       "[http://localhost:3000,http://localhost:4000]",
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
	b := New(WithURL("http://localhost:3000"))
	if first, second := b.Command(), b.Command(); first != second {
		t.Error("Command() returned a different command on the second call, want the cached one")
	}
}

// TestURLRequiredWhenUnseeded pins that a bare command refuses to run rather
// than minting a tunnel with nothing behind it.
func TestURLRequiredWhenUnseeded(t *testing.T) {
	_, _, err := execute(t, New())
	if err == nil {
		t.Fatal("running with no --url = nil error, want a rejection")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error %q does not name the missing flag", err)
	}
}

// TestURLOptionalWhenSeeded pins the embedding case: an origin supplied
// through WithURL makes --url optional rather than forbidden. The command gets
// past the required-flag check and fails on the deliberately bad log level,
// which is the assertion — it never reaches the network.
func TestURLOptionalWhenSeeded(t *testing.T) {
	b := New(WithURL("http://localhost:3000"), WithLogLevel("loud"))
	_, _, err := execute(t, b)
	if !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel (proving --url was optional)", err)
	}
}

// TestFlagReplacesSeededURLs pins that a command line overrides the seed
// wholesale instead of merging into it. The seeded origin is unusable and the
// flag's is fine, so an append would fail on the origin and a replace fails on
// the log level — which is the discriminator.
func TestFlagReplacesSeededURLs(t *testing.T) {
	b := New(WithURL("ftp://seeded.invalid"))
	_, _, err := execute(t, b, "--url", "http://localhost:3000", "--log-level", "loud")
	if errors.Is(err, v1.ErrInvalidOrigin) {
		t.Fatal("seeded origin survived the --url flag, want the flag to replace the seed")
	}
	if !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
	}
}

// TestRejectsUnusableFlags pins that the validation failures reach the caller
// as their v1 sentinels, so a program embedding tunneld can branch on the
// class rather than on message text.
func TestRejectsUnusableFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want error
	}{
		{"unproxyable scheme", []string{"--url", "ftp://localhost:21"}, v1.ErrInvalidOrigin},
		{"no host", []string{"--url", "http://"}, v1.ErrInvalidOrigin},
		{"empty origin", []string{"--url", "  "}, v1.ErrNoOrigin},
		{"unknown log level", []string{"--url", "http://localhost:3000", "--log-level", "loud"}, v1.ErrInvalidLogLevel},
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

// TestRejectsPositionalArgs pins that the command takes no bare arguments —
// origins arrive through --url, so a stray word is a mistake worth naming
// rather than something to ignore.
func TestRejectsPositionalArgs(t *testing.T) {
	if _, _, err := execute(t, New(), "--url", "http://localhost:3000", "stray"); err == nil {
		t.Fatal("running with a positional argument = nil error, want a rejection")
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
	if !strings.Contains(stdout, "expose --url") {
		t.Errorf("help %q does not use the configured command name", stdout)
	}
}

// TestOpenDefaultsOn pins that a plain invocation opens a browser and that
// both levers turn it off. The default is the whole point of the flag — a
// developer exposing something is about to look at it — so a silent flip to
// off would be a real regression. want is --no-open, so it reads inverted:
// true means no browser.
func TestOpenDefaultsOn(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{name: "default", want: false},
		{name: "flag turns it off", args: []string{"--no-open"}, want: true},
		{name: "variable turns it off", env: "true", want: true},
		{name: "flag beats the variable", env: "true", args: []string{"--no-open=false"}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.NoOpenEnv, tc.env)

			b := New(WithURL("http://localhost:3000"))
			// A deliberately bad level stops the run once the flags have
			// settled, before anything dials or any window opens.
			_, _, err := execute(t, b, append(append([]string{}, tc.args...), "--log-level", "loud")...)
			if !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
			}

			got, err := b.Command().Flags().GetBool("no-open")
			if err != nil {
				t.Fatalf("GetBool: %v", err)
			}
			if got != tc.want {
				t.Errorf("--no-open = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWithOpenSeedsTheDefault pins that an embedder can flip the default
// without forbidding the flag: WithOpen(false) makes --no-open default to
// true, and a user passing --no-open=false still gets a browser. The Go knob
// stays positive while the flag reads negative, so this is also what pins the
// two staying in step.
func TestWithOpenSeedsTheDefault(t *testing.T) {
	b := New(WithURL("http://localhost:3000"), WithOpen(false))

	if got := b.Command().Flags().Lookup("no-open").DefValue; got != "true" {
		t.Errorf("--no-open default = %q, want %q", got, "true")
	}

	_, _, err := execute(t, b, "--no-open=false", "--log-level", "loud")
	if !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
	}
	got, err := b.Command().Flags().GetBool("no-open")
	if err != nil {
		t.Fatalf("GetBool: %v", err)
	}
	if got {
		t.Error("--no-open = true after --no-open=false was passed, want the flag to beat the seed")
	}
}

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
			// splitEnvList drops empty entries, so a stray or trailing comma
			// is not a cache directory.
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
			// is the rule --url follows: a command line never merges into a
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
			probe := New(WithURL("http://localhost:3000"))
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

			opts := []Option{WithURL("http://localhost:3000")}
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

			b := New(WithURL("http://localhost:3000", "http://localhost:4000"))
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
	b := New(WithURL("http://localhost:3000"), WithMultiview(false))

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
		b := New(WithURL("http://localhost:3000"))
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

// fakeBinder stands in for the attach package: it hands display back
// unchanged and closes nothing, so TestRun never stands up a real listener.
type fakeBinder struct {
	err    error
	closed bool
}

func (f *fakeBinder) Bind(_ context.Context, display []*url.URL, _ v1.Logger) ([]*url.URL, io.Closer, error) {
	return display, f, f.err
}

func (f *fakeBinder) Close() error { f.closed = true; return nil }

// runHarness is run with every collaborator faked except the two that are
// pure: the real panel, because its URL and interceptor order are what the
// assertions check, and a real counter armed at one, because the verdict
// logic is what the gone case is about. order records the effects that
// matter in the sequence they landed.
type runHarness struct {
	engine *fakeEngine
	cache  *fakeCache
	opener *fakeOpener
	binder *fakeBinder
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
	h.binder = &fakeBinder{}
	tun.order = &h.order
	h.b = New(
		WithURL(urls...),
		WithProvider("example.test"),
		WithCacheDir(t.TempDir()), // run consults the cache only with a directory
		WithEngine(h.engine),
		WithCache(h.cache),
		WithOpener(h.opener),
		WithBinder(h.binder),
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
	for _, name := range []string{v1.URLEnv, v1.ProviderEnv, v1.CacheDirEnv, v1.LogEnv, v1.NoOpenEnv, v1.MultiviewEnv} {
		t.Setenv(name, "")
	}
	cmd := h.b.Command()
	cmd.SetOut(io.Discard)
	cmd.SetErr(&h.stderr)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

// captureStderr swaps os.Stderr for a pipe until the returned function is
// called, which restores it and hands back what was written. run's logger
// writes there rather than to the command's writer, so this is the only
// window onto it.
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
		if want := []string{public}; !slices.Equal(h.opener.opened, want) {
			t.Errorf("opened %q, want the panel address %q", h.opener.opened, want)
		}
		// TestReportNamesTheMultiviewPanel: the panel's own address is what
		// stderr leads with when there is one — it answers for every origin
		// at once, so the per-origin addresses become the indented list
		// beneath it.
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
		if !h.binder.closed {
			t.Error("the binder's closer was never called; run's defer did not run")
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
		h := newRunHarness(t, &fakeTunnel{done: make(chan struct{})}, ":3000")
		err := h.run(t, t.Context())
		if !errors.Is(err, v1.ErrNotReady) {
			t.Errorf("run() = %v, want ErrNotReady", err)
		}
	})

	t.Run("--no-open opens nothing", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", ":4000")
		ctx, cancel := context.WithCancel(t.Context())
		h.cache.onSave = cancel

		if err := h.run(t, ctx, "--no-open"); err != nil {
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

		if err := h.run(t, ctx); err != nil {
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
	// Command's RunE; captureStderr is what makes the listener's own
	// logger — which writes to os.Stderr rather than the command's writer —
	// observable at all.
	t.Run("enough gone verdicts end the run with ErrTunnelGone", func(t *testing.T) {
		tun := live(public)
		h := newRunHarness(t, tun, ":3000")
		h.cache.onSave = func() {
			for range 3 {
				tun.listen(libtunnel.Event{Kind: libtunnel.EventGone, Hostname: "foo.tunneled.pizza"})
			}
		}
		restore := captureStderr(t)

		err := h.run(t, t.Context(), "--log-level", "error")
		logged := restore()

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
		b := &BuilderImpl{urls: []string{":3000"}}
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
// `--url localhost:3000` work the way people expect. Driven through the
// whole run (parseOrigins no longer exists on its own to call directly), so
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

// TestParseOriginsRejects pins the failure modes as errors rather than as a
// public hostname that answers only errors. Each case asserts both the sentinel
// (so callers can branch on the class) and that the message names the offending
// input (so an operator can act on it).
//
// Driven through execute rather than the harness's run: every row here fails
// before the tunnel is ever touched, so the default (non-faked) collaborators
// are fine and nothing dials out. "no origins at all" is the one row that
// cannot be seeded with WithURL — an empty seed makes Command mark --url
// required, so the row instead seeds a placeholder (keeping --url optional)
// and empties it via $TUNNELD_URL, the one path left to reach the
// after-the-loop case at all.
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
			var b *BuilderImpl
			if tc.in == nil {
				t.Setenv(v1.URLEnv, ",")
				b = New(WithURL("http://placeholder.invalid"))
			} else {
				b = New(WithURL(tc.in...))
			}

			_, _, err := execute(t, b)
			if err == nil {
				t.Fatalf("execute(%q) = nil, want an error", tc.in)
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

// TestReportWritesOnlyToStderr pins the output contract: a running tunnel
// writes its addresses to stderr and nothing at all to stdout.
//
// stdout used to carry one bare URL per origin as a machine interface. It
// meant every address printed twice wherever both streams landed together,
// and the de-duplication meant to hide that could only recognise one file
// descriptor being literally the other — which a container's two pipes are
// not, so it never fired there. The map says which origin each address
// reaches, which the bare lines never did.
//
// Multiview is turned off so the report falls into its per-origin branch —
// TestRun's "mint" case already pins the panel branch, folding in
// TestReportNamesTheMultiviewPanel's doc.
func TestReportWritesOnlyToStderr(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000", ":4000")
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx, "--multiview=false"); err != nil {
		t.Fatalf("run() = %v", err)
	}

	for _, want := range []string{
		"  " + public + "?0\n    -> http://localhost:3000\n",
		"  " + public + "?1\n    -> http://localhost:4000\n",
	} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", h.stderr.String(), want)
		}
	}

	// Every address appears once: in the map, and nowhere else.
	for _, addr := range []string{public + "?0", public + "?1"} {
		if got := strings.Count(h.stderr.String(), addr); got != 1 {
			t.Errorf("%s appears %d times, want 1:\n%s", addr, got, h.stderr.String())
		}
	}
}

// TestLogger covers the level resolution: the --log-level value wins, the
// environment mirror is the fallback, and neither being set means silence.
// The flag is strict (an operator typo must not vanish) while the
// environment is lenient, matching what the underlying library does with its
// own knob.
//
// Driven through the whole run and read back with captureStderr, because
// logger no longer exists as a callable method once it is inlined into
// Command's RunE, and its logger writes to os.Stderr rather than the
// command's own writer.
//
// The lenient-environment case (an unparsable $TUNNELD_LOG falling back to
// info with a warning) has no row here: through the command,
// PersistentPreRunE mirrors $TUNNELD_LOG onto the strict --log-level flag
// before RunE ever runs — exactly what TestEnvLogLevelIsStrict (env_test.go)
// pins — so that path never reaches Logger()'s own fallback; TestLoggerLevels'
// "nonsense" row (env_test.go) is what pins Logger()'s leniency directly.
func TestLogger(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	cases := []struct {
		name    string
		level   string
		env     string
		wantErr error
		want    bool // "tunneld starting" present
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
			cmd.SetErr(io.Discard)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(ctx)
			logged := restore()

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
				t.Errorf("stderr contains %q = %v, want %v:\n%s", "tunneld starting", got, tc.want, logged)
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
