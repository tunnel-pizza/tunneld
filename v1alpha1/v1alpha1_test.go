package v1alpha1

import (
	"errors"
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
	"github.com/tunnel-pizza/tunneld/v1alpha1/display"
)

// TestNewSatisfiesTheContract pins that the implementation is assignable to
// the v1 interface — the compile-time half of the contract, which a signature
// drift in either package would break here rather than at a call site.
//
// The assignment is the whole test: it either compiles or it does not. There
// is nothing to assert afterwards, because New returns a pointer and an
// interface holding one is never nil.
func TestNewSatisfiesTheContract(*testing.T) {
	var _ v1.Builder = New()
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
// applied first and the caller's options after, so WithMultiview(false) is not
// overwritten by the seeded DefaultMultiview. Swapping the two Apply calls in New
// fails this.
func TestCallerOptionBeatsDefault(t *testing.T) {
	b := New(WithOrigin("http://localhost:3000"), WithMultiview(false))
	if got := b.Command().Flags().Lookup("multiview").DefValue; got != "false" {
		t.Errorf("--multiview default = %q, want %q (WithMultiview(false) lost to the default)", got, "false")
	}
}

// TestNewWiresEveryCollaborator pins that New seeds every one of them, and
// that each With* option lands: a nil handed to one is what the wiring check
// at the top of Command names. A bare BuilderImpl{} fails the same check on
// its first collaborator, the engine. The check runs before Command binds a
// flag to any field, so there is no panic to route around and no case left
// unobservable.
//
// The cache is the one that may be nil, because nil is what --no-cache means.
//
// wired no longer exists as a callable method once it is inlined into
// Command, so every row here executes the built command instead.
// --log-level loud stops the "every collaborator seeded" case just past the
// wiring check and well before anything touches the network, which is what
// proves New wired every collaborator without actually minting a tunnel.
func TestNewWiresEveryCollaborator(t *testing.T) {
	_, _, err := execute(t, New(WithOrigin(":3000")), "--log-level", "loud")
	if !errors.Is(err, v1.ErrInvalidLogLevel) {
		t.Fatalf("New(): error = %v, want ErrInvalidLogLevel (proving every collaborator was wired)", err)
	}

	for _, tc := range []struct {
		name string
		b    *BuilderImpl
	}{
		{"engine", New(WithOrigin(":3000"), WithEngine(nil))},
		{"browser", New(WithOrigin(":3000"), WithDisplay(nil))},
		{"counter", New(WithOrigin(":3000"), WithCounter(nil))},
		{"binder", New(WithOrigin(":3000"), WithBinder(nil))},
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
		if err == nil || !strings.Contains(err.Error(), "engine") {
			t.Errorf("BuilderImpl{}: error = %v, want an error naming engine, the first missing collaborator", err)
		}
	})

	// The cache is the exception, and the exception is the feature: nil is
	// what caching turned off looks like, so a run configured that way is an
	// ordinary run rather than a misassembled builder.
	t.Run("a nil cache is not missing", func(t *testing.T) {
		// Past the wiring check and stopped on the bad level, the same way the
		// every-collaborator case above proves it got that far.
		_, _, err := execute(t, New(WithOrigin(":3000"), WithCache(nil)), "--log-level", "loud")
		if !errors.Is(err, v1.ErrInvalidLogLevel) {
			t.Errorf("with WithCache(nil): error = %v, want the run past the wiring check", err)
		}
	})
}

// TestContractOptionsLand pins that a contract option replaces the default
// rather than sitting beside it: what run reads is what the caller gave.
func TestContractOptionsLand(t *testing.T) {
	e, c, o, n, g := &fakeEngine{}, &fakeCache{}, &fakeDisplay{DisplayImpl: display.New()}, counter.New(), &fakeBinder{}
	b := New(WithEngine(e), WithCache(c), WithDisplay(o), WithCounter(n), WithBinder(g))

	if b.engine != Engine(e) || b.cache != Cache(c) ||
		b.display != Display(o) || b.counter != Counter(n) || b.binder != Binder(g) {
		t.Error("a contract option did not land on the field run reads")
	}

	// And the wrapper lands on the same field, with a cache pointed where it
	// was told rather than at the user's own cache directory.
	dir := t.TempDir()
	if got := New(WithCacheDir(dir)).cache; got == nil {
		t.Error("WithCacheDir left no cache behind")
	}
}
