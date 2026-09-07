package v1alpha1

import (
	"errors"
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cachedir"
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

// TestNewWiresEveryCollaborator pins that New seeds all seven, and that each
// With* option lands: a nil handed to one is what the wiring check at the top
// of Command names. A bare BuilderImpl{} fails the same check on its first
// collaborator, cacheDirs — the check runs before Command reads any field
// (Command needs cacheDirs itself, to seed and bind --cache-dir, so a check
// inside RunE would always have run too late for that one), so there is no
// panic to route around and no case left unobservable.
//
// wired no longer exists as a callable method once it is inlined into
// Command, so every row here executes the built command instead.
// --log-level loud stops the "every collaborator seeded" case just past the
// wiring check and well before anything touches the network, which is what
// proves New wired every collaborator without actually minting a tunnel.
func TestNewWiresEveryCollaborator(t *testing.T) {
	_, _, err := execute(t, New(WithURL(":3000")), "--log-level", "loud")
	if !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("New(): error = %v, want ErrInvalidLogLevel (proving every collaborator was wired)", err)
	}

	for _, tc := range []struct {
		name string
		b    *BuilderImpl
	}{
		{"cacheDirs", New(WithURL(":3000"), WithCacheDirs(nil))},
		{"engine", New(WithURL(":3000"), WithEngine(nil))},
		{"cache", New(WithURL(":3000"), WithCache(nil))},
		{"panel", New(WithURL(":3000"), WithPanel(nil))},
		{"opener", New(WithURL(":3000"), WithOpener(nil))},
		{"counter", New(WithURL(":3000"), WithCounter(nil))},
		{"binder", New(WithURL(":3000"), WithBinder(nil))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := execute(t, tc.b)
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Errorf("with no %s: error = %v, want it named", tc.name, err)
			}
		})
	}

	t.Run("bare struct", func(t *testing.T) {
		_, _, err := execute(t, &BuilderImpl{})
		if err == nil || !strings.Contains(err.Error(), "cacheDirs") {
			t.Errorf("BuilderImpl{}: error = %v, want an error naming cacheDirs, the first missing collaborator", err)
		}
	})
}

// TestContractOptionsLand pins that a contract option replaces the default
// rather than sitting beside it: what run reads is what the caller gave.
func TestContractOptionsLand(t *testing.T) {
	d, e, c, p, o, n, g := cachedir.New(), &fakeEngine{}, &fakeCache{}, panel.New(), &fakeOpener{}, counter.New(), &fakeBinder{}
	b := New(WithCacheDirs(d), WithEngine(e), WithCache(c), WithPanel(p), WithOpener(o), WithCounter(n), WithBinder(g))

	if b.cacheDirs != CacheDirs(d) || b.engine != Engine(e) || b.cache != Cache(c) || b.panel != Panel(p) ||
		b.opener != Opener(o) || b.counter != Counter(n) || b.binder != Binder(g) {
		t.Error("a contract option did not land on the field run reads")
	}
}
