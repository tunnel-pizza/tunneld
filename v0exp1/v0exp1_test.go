package v0exp1

import (
	"errors"
	"log/slog"
	"net/http"
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

// TestMcp holds Experimental().Mcp() to the package it fronts: built from
// the options it is handed, a handler to mount, and a closer that can be
// closed twice — once by the run's defer, once by whatever else ends it.
func TestMcp(t *testing.T) {
	origins := []McpOrigin{{Name: "http://localhost:3000", Kind: McpHTTP}}
	m := Experimental().Mcp(McpWithOrigins(origins), McpWithLog(slog.New(slog.DiscardHandler)))
	if m == nil {
		t.Skip("the MCP server is turned off")
	}
	var _ http.Handler = m
	for range 2 {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}
