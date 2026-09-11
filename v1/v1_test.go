package v1_test

import (
	"errors"
	"slices"
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
		{v1.AttachScheme, "attach"},
		{v1.ExecScheme, "exec"},
		{v1.DockerProvider, "dockerd"},
		{v1.IdentityProvidersEnv, "TUNNELD_IDENTITY_PROVIDERS"},
		{v1.DefaultIdentityProviders, "github"},
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
		"ErrNoDocker":        v1.ErrNoDocker,
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

// TestApplyOrder pins the one rule every constructor in the tree relies on:
// options run in the order given, so a later one wins, and Apply hands back
// what it was given so a New can be a return statement.
func TestApplyOrder(t *testing.T) {
	type knobs struct {
		name string
		seen []string
	}
	set := func(v string) v1.Option[*knobs] {
		return func(k *knobs) { k.name = v; k.seen = append(k.seen, v) }
	}

	k := &knobs{}
	if got := v1.Apply(k, set("default"), set("caller")); got != k {
		t.Fatal("Apply returned a different value than it was given")
	}
	if k.name != "caller" {
		t.Errorf("name = %q, want the later option to win", k.name)
	}
	if want := []string{"default", "caller"}; !slices.Equal(k.seen, want) {
		t.Errorf("applied in order %v, want %v", k.seen, want)
	}
	if got := v1.Apply(&knobs{}); got.name != "" || got.seen != nil {
		t.Error("Apply with no options changed its argument")
	}
}
