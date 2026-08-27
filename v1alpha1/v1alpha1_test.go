package v1alpha1_test

import (
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

// TestNewSatisfiesTheContract pins that the implementation is assignable to
// the v1 interface — the compile-time half of the contract, which a signature
// drift in either package would break here rather than at a call site.
func TestNewSatisfiesTheContract(t *testing.T) {
	var b v1.Builder = v1alpha1.New()
	if b == nil {
		t.Fatal("New() = nil, want a builder")
	}
}

// TestNewIsUnconfigured pins that New carries no state of its own: two
// builders are independent, so configuring one never leaks into another.
func TestNewIsUnconfigured(t *testing.T) {
	first, second := v1alpha1.New(), v1alpha1.New()
	first.WithName("expose")

	if got, want := second.Name(), v1.CommandName; got != want {
		t.Errorf("second builder Name() = %q, want the untouched default %q", got, want)
	}
}
