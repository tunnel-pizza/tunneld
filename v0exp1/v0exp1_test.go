package v0exp1

import (
	"errors"
	"reflect"
	"testing"

	"github.com/tunnel-pizza/tunneld/v0exp1/internal/shell/builtin"
)

// TestBuiltin holds Experimental().Builtin() to the package it fronts: the
// same origin, and the same sentinel, so errors.Is matches either name.
func TestBuiltin(t *testing.T) {
	if !errors.Is(ErrBuiltinNoTerminal, builtin.ErrNoTerminal) {
		t.Errorf("ErrBuiltinNoTerminal is not builtin.ErrNoTerminal")
	}
	b := Experimental().Builtin()
	if b == nil {
		t.Skip("the built-in shell is turned off")
	}
	got, gotErr := b.Origin()
	want, wantErr := builtin.Origin()
	if !reflect.DeepEqual(got, want) || !errors.Is(gotErr, wantErr) {
		t.Errorf("Experimental().Builtin().Origin() = %v, %v; want %v, %v", got, gotErr, want, wantErr)
	}
}
