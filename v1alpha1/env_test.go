package v1alpha1_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/cnuss/golib/v1"
	"github.com/cnuss/golib/v1alpha1"
)

const boolVar = "GOLIB__TEST_BOOL"

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

			value, fixed, err := v1alpha1.EnvBool(boolVar)
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

const durationVar = "GOLIB__TEST_DURATION"

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

			value, fixed, err := v1alpha1.EnvDuration(durationVar)
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

	_, _, err := v1alpha1.EnvBool(boolVar)
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

	if v1alpha1.Logger().Enabled(t.Context(), 1000) {
		t.Error("Logger() is enabled with GOLIB_LOG unset, want silent")
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

			log := v1alpha1.Logger()
			if got := log.Enabled(t.Context(), -4); got != tc.wantDebug {
				t.Errorf("debug enabled = %v, want %v", got, tc.wantDebug)
			}
			if got := log.Enabled(t.Context(), 4); got != tc.wantWarning {
				t.Errorf("warn enabled = %v, want %v", got, tc.wantWarning)
			}
		})
	}
}
