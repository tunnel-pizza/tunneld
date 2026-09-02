package v1alpha1_test

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

// execute runs a built command with args, capturing both streams. Every case
// here is an offline one — the command fails during validation, before the
// tunnel is ever dialed — so the tests never touch the network.
func execute(t *testing.T, b v1.Builder, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := b.Build()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// TestWithersChain pins the fluent contract: every With* returns the Builder,
// so a whole configuration is one expression.
func TestWithersChain(t *testing.T) {
	var sink bytes.Buffer
	b := v1alpha1.New().
		WithName("expose").
		WithURL("http://localhost:3000").
		WithURL("http://localhost:4000").
		WithProvider("example.test").
		WithLogLevel("warn").
		WithStdout(&sink).
		WithStderr(&sink)

	if got, want := b.Name(), "expose"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := b.Build().Name(), "expose"; got != want {
		t.Errorf("built command Name() = %q, want %q", got, want)
	}
}

// TestNameDefaults pins that an unnamed builder produces the canonical command
// name, and that WithName is what an embedding program uses to mount it under
// its own verb.
func TestNameDefaults(t *testing.T) {
	if got, want := v1alpha1.New().Name(), v1.CommandName; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := v1alpha1.New().Build().Name(), v1.CommandName; got != want {
		t.Errorf("built command Name() = %q, want %q", got, want)
	}
}

// TestBuildIsIdempotent pins that Build assembles once. A second assembly
// would bind a second set of flags over the same fields, so the cached command
// is correctness, not just an optimization.
func TestBuildIsIdempotent(t *testing.T) {
	b := v1alpha1.New().WithURL("http://localhost:3000")
	if first, second := b.Build(), b.Build(); first != second {
		t.Error("Build() returned a different command on the second call, want the cached one")
	}
}

// TestURLRequiredWhenUnseeded pins that a bare command refuses to run rather
// than minting a tunnel with nothing behind it.
func TestURLRequiredWhenUnseeded(t *testing.T) {
	_, _, err := execute(t, v1alpha1.New())
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
	b := v1alpha1.New().WithURL("http://localhost:3000").WithLogLevel("loud")
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
	b := v1alpha1.New().WithURL("ftp://seeded.invalid")
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
			_, _, err := execute(t, v1alpha1.New(), tc.args...)
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
	if _, _, err := execute(t, v1alpha1.New(), "--url", "http://localhost:3000", "stray"); err == nil {
		t.Fatal("running with a positional argument = nil error, want a rejection")
	}
}

// TestVersionSubcommand pins that `tunneld version` reports without touching
// the network, and that the banner names both builds — a bug report needs the
// tunneld version and the tunnel library's.
func TestVersionSubcommand(t *testing.T) {
	stdout, _, err := execute(t, v1alpha1.New(), "version")
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
	stdout, _, err := execute(t, v1alpha1.New().WithName("expose"), "--help")
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

			b := v1alpha1.New().WithURL("http://localhost:3000")
			// A deliberately bad level stops the run once the flags have
			// settled, before anything dials or any window opens.
			_, _, err := execute(t, b, append(append([]string{}, tc.args...), "--log-level", "loud")...)
			if !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
			}

			got, err := b.Build().Flags().GetBool("no-open")
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
	b := v1alpha1.New().WithURL("http://localhost:3000").WithOpen(false)

	if got := b.Build().Flags().Lookup("no-open").DefValue; got != "true" {
		t.Errorf("--no-open default = %q, want %q", got, "true")
	}

	_, _, err := execute(t, b, "--no-open=false", "--log-level", "loud")
	if !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
	}
	got, err := b.Build().Flags().GetBool("no-open")
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
// working directory rather than that particular one. "." in a want is that
// directory, written "$WD" because its real path is only known at run time.
func TestCacheDir(t *testing.T) {
	cases := []struct {
		name string
		seed []string
		env  string
		args []string
		want []string
	}{
		{name: "the working directory by default", want: []string{"$WD"}},
		{
			// The value the compose example ships. Three entries in, two out:
			// "." and "true" are both the working directory.
			name: "the compose value collapses to two",
			env:  ".,true,/tmp",
			want: []string{"$WD", "/tmp"},
		},
		{
			name: "every spelling of true is the working directory",
			env:  "1,t,T,TRUE,True,true",
			want: []string{"$WD"},
		},
		{name: "an empty entry is the working directory too", args: []string{"--cache-dir", ""}, want: []string{"$WD"}},
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
			env:  ".,false,/tmp",
		},
		{name: "false first disables the rest", env: "false,/tmp"},
		{name: "false last disables what came before", args: []string{"--cache-dir", "/a", "--cache-dir", "false"}},
		{
			// Sticky within one source: a directory named after the switch
			// cannot quietly turn it back on.
			name: "a later directory cannot re-enable it",
			args: []string{"--cache-dir", "false", "--cache-dir", "/a"},
		},
		{
			// Build fills an unset list with the working directory, and has
			// to tell "unset" from "emptied on purpose" to leave this one
			// alone. Nothing else in this table separates the two.
			name: "a seed that disabled it is not re-filled by the default",
			seed: []string{"false"},
		},
		{
			// A later source still overrides, the same as any other value.
			name: "the flag overrides a variable that disabled it",
			env:  "false", args: []string{"--cache-dir", "/a"},
			want: []string{"/a"},
		},
		{
			name: "the variable overrides a seed that disabled it",
			seed: []string{"false"}, env: "/tmp",
			want: []string{"/tmp"},
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
			env:  ",,/tmp,",
			want: []string{"/tmp"},
		},
		{name: "repeated flags append", args: []string{"--cache-dir", "/a", "--cache-dir", "/b", "--cache-dir", "/a"}, want: []string{"/a", "/b"}},
		{name: "the flag beats the variable", env: "/from-env", args: []string{"--cache-dir", "/from-flag"}, want: []string{"/from-flag"}},
		{name: "a seed stands when nothing overrides it", seed: []string{"/seeded"}, want: []string{"/seeded"}},
		{
			// Both overrides replace the seed rather than extending it, which
			// is the rule --url follows: a command line never merges into a
			// seeded set, and neither does the environment.
			name: "the variable replaces a seed",
			seed: []string{"/seeded"}, env: "/tmp",
			want: []string{"/tmp"},
		},
		{
			name: "the flag replaces a seed",
			seed: []string{"/seeded"}, args: []string{"--cache-dir", "/a", "--cache-dir", "/b"},
			want: []string{"/a", "/b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			workdir, err := os.Getwd()
			if err != nil {
				t.Fatalf("Getwd: %v", err)
			}
			t.Setenv(v1.CacheDirEnv, tc.env)

			b := v1alpha1.New().WithURL("http://localhost:3000")
			if len(tc.seed) > 0 {
				b.WithCacheDir(tc.seed...)
			}
			// A deliberately bad level stops the run once the flags have
			// settled, before anything dials.
			args := append(append([]string{}, tc.args...), "--log-level", "loud")
			if _, _, err := execute(t, b, args...); !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
			}

			// GetSlice rather than GetStringArray: the latter round-trips
			// through a comma-separated String(), which splits any path that
			// contains a comma — and t.TempDir builds one out of the subtest
			// name.
			got := b.Build().Flags().Lookup("cache-dir").Value.(pflag.SliceValue).GetSlice()
			want := make([]string, 0, len(tc.want))
			for _, dir := range tc.want {
				want = append(want, strings.Replace(dir, "$WD", workdir, 1))
			}
			if !slices.Equal(got, want) {
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

			b := v1alpha1.New().WithURL("http://localhost:3000", "http://localhost:4000")
			_, _, err := execute(t, b, append(append([]string{}, tc.args...), "--log-level", "loud")...)
			if !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
			}

			got, err := b.Build().Flags().GetBool("multiview")
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
	b := v1alpha1.New().WithURL("http://localhost:3000").WithMultiview(false)

	if got := b.Build().Flags().Lookup("multiview").DefValue; got != "false" {
		t.Errorf("--multiview default = %q, want %q", got, "false")
	}
}
