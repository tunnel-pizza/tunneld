package v1alpha1

import (
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
	"github.com/tunnel-pizza/tunneld/v1alpha1/panel"
)

// TestNewSatisfiesTheContract pins that the implementation is assignable to
// the v1 interface — the compile-time half of the contract, which a signature
// drift in either package would break here rather than at a call site.
func TestNewSatisfiesTheContract(t *testing.T) {
	var b v1.Builder = New()
	if b == nil {
		t.Fatal("New() = nil, want a builder")
	}
}

// TestNewIsUnconfigured pins that New carries no state of its own: two
// builders are independent, so configuring one never leaks into another.
func TestNewIsUnconfigured(t *testing.T) {
	first, second := New(WithName("expose")), New()

	if got, want := first.Name(), "expose"; got != want {
		t.Errorf("first builder Name() = %q, want %q", got, want)
	}
	if got, want := second.Name(), v1.CommandName; got != want {
		t.Errorf("second builder Name() = %q, want the untouched default %q", got, want)
	}
}

// TestCallerOptionBeatsDefault pins the two tiers in New: the defaults are
// applied first and the caller's options after, so WithOpen(false) is not
// overwritten by the seeded DefaultOpen. Swapping the two Apply calls in New
// fails this.
func TestCallerOptionBeatsDefault(t *testing.T) {
	b := New(WithURL("http://localhost:3000"), WithOpen(false))
	if got := b.Command().Flags().Lookup("no-open").DefValue; got != "true" {
		t.Errorf("--no-open default = %q, want %q (WithOpen(false) lost to the default)", got, "true")
	}
}

// TestNewWiresEveryCollaborator pins that New seeds all six, and that each
// With* option lands: a nil handed to one is what wired names. A bare
// BuilderImpl{} fails the same check on its first collaborator.
func TestNewWiresEveryCollaborator(t *testing.T) {
	if err := New().wired(); err != nil {
		t.Fatalf("New().wired() = %v, want every collaborator seeded", err)
	}
	if err := (&BuilderImpl{}).wired(); err == nil || !strings.Contains(err.Error(), "engine") {
		t.Errorf("BuilderImpl{}.wired() = %v, want an error naming the first missing collaborator", err)
	}

	for _, tc := range []struct {
		name string
		b    *BuilderImpl
	}{
		{"engine", New(WithEngine(nil))},
		{"cache", New(WithCache(nil))},
		{"panel", New(WithPanel(nil))},
		{"opener", New(WithOpener(nil))},
		{"counter", New(WithCounter(nil))},
		{"targets", New(WithTargets(nil))},
	} {
		err := tc.b.wired()
		if err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("wired() with no %s = %v, want an error naming it", tc.name, err)
		}
	}
}

// TestContractOptionsLand pins that a contract option replaces the default
// rather than sitting beside it: what run reads is what the caller gave.
func TestContractOptionsLand(t *testing.T) {
	e, c, p, o, n, g := &fakeEngine{}, &fakeCache{}, panel.New(), &fakeOpener{}, counter.New(), &stubTargets{}
	b := New(WithEngine(e), WithCache(c), WithPanel(p), WithOpener(o), WithCounter(n), WithTargets(g))

	if b.engine != Engine(e) || b.cache != Cache(c) || b.panel != Panel(p) ||
		b.opener != Opener(o) || b.counter != Counter(n) || b.targets != Targets(g) {
		t.Error("a contract option did not land on the field run reads")
	}
}
