package v1_test

import (
	"errors"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// TestEnvNamesAreStable pins the operator-facing strings. They are a
// compatibility promise like any exported name — a deployment sets them in a
// unit file or a Dockerfile, where a rename fails silently — so a change here
// has to be a deliberate edit to this table.
func TestEnvNamesAreStable(t *testing.T) {
	cases := []struct{ got, want string }{
		{v1.LogEnv, "TUNNELD_LOG"},
		{v1.CommandName, "tunneld"},
		{v1.DefaultProvider, "tunnel.pizza"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("constant = %q, want %q", tc.got, tc.want)
		}
	}
}

// TestSentinelsAreDistinct pins that no two sentinels are the same value.
// Callers branch on them with errors.Is, so an accidental aliasing would make
// one condition silently answer for another.
func TestSentinelsAreDistinct(t *testing.T) {
	sentinels := map[string]error{
		"ErrInvalidEnv":      v1.ErrInvalidEnv,
		"ErrNoOrigin":        v1.ErrNoOrigin,
		"ErrInvalidOrigin":   v1.ErrInvalidOrigin,
		"ErrInvalidLogLevel": v1.ErrInvalidLogLevel,
		"ErrNotReady":        v1.ErrNotReady,
	}
	for name, err := range sentinels {
		if err == nil {
			t.Errorf("%s is nil", name)
			continue
		}
		for otherName, other := range sentinels {
			if name == otherName {
				continue
			}
			if errors.Is(err, other) {
				t.Errorf("%s matches %s, want distinct sentinels", name, otherName)
			}
		}
	}
}
