package mcp

import (
	"slices"
	"testing"
)

// TestTools pins the surface: the tools tools/list names, which the SDK
// lists by name.
func TestTools(t *testing.T) {
	res, err := connect(t, New()).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if want := []string{"terminal_exec", "terminal_screen", "terminal_send", "terminal_sessions"}; !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}
