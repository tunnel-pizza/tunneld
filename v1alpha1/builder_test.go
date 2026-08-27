package v1alpha1_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

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
// off would be a real regression.
func TestOpenDefaultsOn(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{name: "default", want: true},
		{name: "flag turns it off", args: []string{"--open=false"}, want: false},
		{name: "variable turns it off", env: "false", want: false},
		{name: "flag beats the variable", env: "false", args: []string{"--open=true"}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.OpenEnv, tc.env)

			b := v1alpha1.New().WithURL("http://localhost:3000")
			// A deliberately bad level stops the run once the flags have
			// settled, before anything dials or any window opens.
			_, _, err := execute(t, b, append(append([]string{}, tc.args...), "--log-level", "loud")...)
			if !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
			}

			got, err := b.Build().Flags().GetBool("open")
			if err != nil {
				t.Fatalf("GetBool: %v", err)
			}
			if got != tc.want {
				t.Errorf("--open = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWithOpenSeedsTheDefault pins that an embedder can flip the default
// without forbidding the flag: WithOpen(false) makes --open default to false,
// and a user passing --open still gets a browser.
func TestWithOpenSeedsTheDefault(t *testing.T) {
	b := v1alpha1.New().WithURL("http://localhost:3000").WithOpen(false)

	if got := b.Build().Flags().Lookup("open").DefValue; got != "false" {
		t.Errorf("--open default = %q, want %q", got, "false")
	}

	_, _, err := execute(t, b, "--open", "--log-level", "loud")
	if !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
	}
	got, err := b.Build().Flags().GetBool("open")
	if err != nil {
		t.Fatalf("GetBool: %v", err)
	}
	if !got {
		t.Error("--open = false after the flag was passed, want the flag to beat the seed")
	}
}
