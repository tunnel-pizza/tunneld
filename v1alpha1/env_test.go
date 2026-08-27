package v1alpha1

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

const boolVar = "TUNNELD__TEST_BOOL"

// TestEnvBool covers the (value, fixed, err) contract: an unset knob is not
// fixed, an explicit false is fixed (which a bare bool return could not
// distinguish), and an unparsable value is reported rather than swallowed.
func TestEnvBool(t *testing.T) {
	cases := []struct {
		name      string
		set       bool
		env       string
		wantValue bool
		wantFixed bool
		wantErr   bool
	}{
		{name: "unset", set: false},
		{name: "empty reads as unset", set: true, env: ""},
		{name: "true", set: true, env: "true", wantValue: true, wantFixed: true},
		{name: "explicit false is still fixed", set: true, env: "false", wantFixed: true},
		{name: "1 parses as true", set: true, env: "1", wantValue: true, wantFixed: true},
		{name: "garbage is fixed but errors", set: true, env: "yes-please", wantFixed: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(boolVar, tc.env)
			}

			value, fixed, err := EnvBool(boolVar)
			if value != tc.wantValue {
				t.Errorf("value = %v, want %v", value, tc.wantValue)
			}
			if fixed != tc.wantFixed {
				t.Errorf("fixed = %v, want %v", fixed, tc.wantFixed)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, v1.ErrInvalidEnv) {
				t.Errorf("err = %v, want it to wrap v1.ErrInvalidEnv", err)
			}
		})
	}
}

const durationVar = "TUNNELD__TEST_DURATION"

// TestEnvDuration mirrors TestEnvBool for the duration knob, including the
// zero-but-fixed case that separates "set to 0s" from "unset".
func TestEnvDuration(t *testing.T) {
	cases := []struct {
		name      string
		set       bool
		env       string
		wantValue time.Duration
		wantFixed bool
		wantErr   bool
	}{
		{name: "unset", set: false},
		{name: "empty reads as unset", set: true, env: ""},
		{name: "milliseconds", set: true, env: "500ms", wantValue: 500 * time.Millisecond, wantFixed: true},
		{name: "explicit zero is still fixed", set: true, env: "0s", wantFixed: true},
		{name: "bare number errors", set: true, env: "30", wantFixed: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(durationVar, tc.env)
			}

			value, fixed, err := EnvDuration(durationVar)
			if value != tc.wantValue {
				t.Errorf("value = %v, want %v", value, tc.wantValue)
			}
			if fixed != tc.wantFixed {
				t.Errorf("fixed = %v, want %v", fixed, tc.wantFixed)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, v1.ErrInvalidEnv) {
				t.Errorf("err = %v, want it to wrap v1.ErrInvalidEnv", err)
			}
		})
	}
}

// TestEnvErrorNamesTheLever pins the doc discipline in code: the message has
// to name the variable and the offending value, since that is the whole lever
// an operator has.
func TestEnvErrorNamesTheLever(t *testing.T) {
	t.Setenv(boolVar, "yes-please")

	_, _, err := EnvBool(boolVar)
	if err == nil {
		t.Fatal("EnvBool() = nil error for an unparsable value")
	}
	for _, want := range []string{boolVar, "yes-please"} {
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
	cmd := New().Build()

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

// TestSplitEnvList covers the list-valued variable parsing: comma-separated,
// space-tolerant, and empty entries dropped so a trailing comma does not
// become an origin nothing can proxy to.
func TestSplitEnvList(t *testing.T) {
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
			got := splitEnvList(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitEnvList(%q) = %q, want %q", tc.in, got, tc.want)
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
// is partly cobra's — applyEnv marking a flag changed is what stops required
// flag validation from rejecting an origin the environment supplied.
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
			cmd := b.Build()
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

	cmd := New().WithURL("http://seeded:1").Build()
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
	if first, second := newEnv(), newEnv(); first == second {
		t.Error("newEnv() returned the same instance twice, want one per builder")
	}
}

// TestEnvLogLevelIsStrict pins that an unparsable level fails the same way
// whichever side it came from. Env beats code, so a typo'd variable that fell
// back silently would be indistinguishable from one that worked — the promise
// ErrInvalidEnv already makes for the other knobs.
func TestEnvLogLevelIsStrict(t *testing.T) {
	t.Setenv(v1.LogEnv, "loud")
	t.Setenv(v1.URLEnv, "http://localhost:3000")

	cmd := New().Build()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	if err := cmd.ExecuteContext(t.Context()); !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Errorf("error = %v, want ErrInvalidLogLevel", err)
	}
}
