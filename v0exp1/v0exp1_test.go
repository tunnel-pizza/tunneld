package v0exp1

import (
	"errors"
	"log/slog"
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
	if got != want || !errors.Is(gotErr, wantErr) {
		t.Errorf("Experimental().Builtin().Origin() = %q, %v; want %q, %v", got, gotErr, want, wantErr)
	}
}

// TestMcp holds Experimental().Mcp() to the package it fronts: a handler and
// a closer, and the closer can be closed twice — once by the run's defer,
// once by whatever else ends it.
func TestMcp(t *testing.T) {
	m := Experimental().Mcp()
	if m == nil {
		t.Skip("the MCP server is turned off")
	}
	h, closer := m.Handler([]McpOrigin{{Name: "http://localhost:3000", Kind: McpHTTP}}, slog.New(slog.DiscardHandler))
	if h == nil || closer == nil {
		t.Fatalf("Handler = %v, %v; want both", h, closer)
	}
	for range 2 {
		if err := closer.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}
