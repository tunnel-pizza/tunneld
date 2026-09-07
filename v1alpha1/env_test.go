package v1alpha1

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// TestEnvErrorNamesTheLever pins the doc discipline in code: an environment
// override that is set but unparsable must fail loudly and name the variable
// and the offending value, since that is the whole lever an operator has to
// recover from the error.
func TestEnvErrorNamesTheLever(t *testing.T) {
	t.Setenv(v1.NoOpenEnv, "maybe")

	cmd := New(WithURL(":3000")).Command()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("ExecuteContext() = nil error for an unparsable NoOpenEnv value")
	}
	if !errors.Is(err, v1.ErrInvalidEnv) {
		t.Errorf("err = %v, want it to wrap v1.ErrInvalidEnv", err)
	}
	for _, want := range []string{v1.NoOpenEnv, "maybe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

// TestLoggerSilentByDefault pins the silent default: a library that logs
// uninvited pollutes its importer's output.
func TestLoggerSilentByDefault(t *testing.T) {
	t.Setenv(v1.LogEnv, "")

	if Logger().Enabled(t.Context(), 1000) {
		t.Error("Logger() is enabled with TUNNELD_LOG unset, want silent")
	}
}

// TestLoggerLevels pins that the env var selects the level, and that an
// unrecognized value falls back to info rather than silencing the logs the
// operator was trying to turn on.
func TestLoggerLevels(t *testing.T) {
	cases := []struct {
		env         string
		wantDebug   bool
		wantWarning bool
	}{
		{env: "debug", wantDebug: true, wantWarning: true},
		{env: "warn", wantDebug: false, wantWarning: true},
		{env: "nonsense", wantDebug: false, wantWarning: true}, // info fallback
	}

	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(v1.LogEnv, tc.env)

			log := Logger()
			if got := log.Enabled(t.Context(), -4); got != tc.wantDebug {
				t.Errorf("debug enabled = %v, want %v", got, tc.wantDebug)
			}
			if got := log.Enabled(t.Context(), 4); got != tc.wantWarning {
				t.Errorf("warn enabled = %v, want %v", got, tc.wantWarning)
			}
		})
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
		"url":       "TUNNELD_URL",
		"provider":  "TUNNELD_PROVIDER",
		"log-level": "TUNNELD_LOG",
	}
	for flag, env := range want {
		if got := flagEnv[flag]; got != env {
			t.Errorf("flagEnv[%q] = %q, want %q", flag, got, env)
		}
	}
}

// TestEnvListSplitting covers the list-valued TUNNELD_URL variable through
// the built command: comma-separated, space-tolerant, and empty entries
// dropped so a trailing comma does not become an origin nothing can proxy
// to.
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
			t.Setenv(v1.URLEnv, tc.in)

			cmd := New().Command()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"--log-level", "loud"})
			err := cmd.ExecuteContext(t.Context())

			if len(tc.want) == 0 {
				// An empty variable reads as unset, so nothing satisfies the
				// required --url flag and that is the error that comes back
				// instead of an empty origin list.
				if err == nil || !strings.Contains(err.Error(), "url") {
					t.Fatalf("error = %v, want it to name the missing url flag", err)
				}
				return
			}

			// ErrInvalidLogLevel proves the environment applied and the run
			// got past the required-flag check.
			if !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
			}
			got := cmd.Flags().Lookup("url").Value.(pflag.SliceValue).GetSlice()
			if len(got) != len(tc.want) {
				t.Fatalf("--url = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("item %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestApplyEnvPrecedence pins flag > env > default, one row per rung. The
// command is executed rather than poked at, because the behaviour under test
// is partly cobra's — PersistentPreRunE marking a flag changed is what stops
// required flag validation from rejecting an origin the environment supplied.
func TestApplyEnvPrecedence(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want []string // the settled --url values
	}{
		{
			name: "environment supplies the origin",
			env:  map[string]string{"TUNNELD_URL": "http://env:1"},
			want: []string{"http://env:1"},
		},
		{
			name: "environment supplies several origins",
			env:  map[string]string{"TUNNELD_URL": "http://env:1,http://env:2"},
			want: []string{"http://env:1", "http://env:2"},
		},
		{
			name: "flag beats environment",
			env:  map[string]string{"TUNNELD_URL": "http://env:1"},
			args: []string{"--url", "http://flag:1"},
			want: []string{"http://flag:1"},
		},
		{
			name: "flag replaces the whole environment list",
			env:  map[string]string{"TUNNELD_URL": "http://env:1,http://env:2"},
			args: []string{"--url", "http://flag:1"},
			want: []string{"http://flag:1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			b := New()
			cmd := b.Command()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			// A deliberately bad level stops the run after the flags settle
			// and before anything dials, so the assertion never needs a
			// network.
			cmd.SetArgs(append(append([]string{}, tc.args...), "--log-level", "loud"))

			if err := cmd.ExecuteContext(t.Context()); !errors.Is(err, v1.ErrInvalidLogLevel) {
				t.Fatalf("error = %v, want ErrInvalidLogLevel (the flags never settled)", err)
			}

			got, err := cmd.Flags().GetStringArray("url")
			if err != nil {
				t.Fatalf("GetStringArray: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("--url = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("origin %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestApplyEnvSeededDefault pins that the environment overrides a WithURL seed
// rather than extending it — the same rule the command line follows, so an
// embedder's default behaves the same whichever way a user overrides it.
func TestApplyEnvSeededDefault(t *testing.T) {
	t.Setenv(v1.URLEnv, "http://env:1")

	cmd := New(WithURL("http://seeded:1")).Command()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--log-level", "loud"})

	if err := cmd.ExecuteContext(t.Context()); !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
	}

	got, err := cmd.Flags().GetStringArray("url")
	if err != nil {
		t.Fatalf("GetStringArray: %v", err)
	}
	if len(got) != 1 || got[0] != "http://env:1" {
		t.Errorf("--url = %q, want the environment value to replace the seed", got)
	}
}

// TestApplyEnvIsPerBuilder pins that the binding is a viper instance per
// builder, not the package global: two commands in one process must not share
// a key space, or an embedded tunneld would inherit its host's configuration.
func TestApplyEnvIsPerBuilder(t *testing.T) {
	t.Setenv(v1.URLEnv, "http://env:1")

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
	t.Setenv(v1.URLEnv, "http://localhost:3000")

	cmd := New().Command()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	if err := cmd.ExecuteContext(t.Context()); !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Errorf("error = %v, want ErrInvalidLogLevel", err)
	}
}
