package terminal

import (
	"slices"
	"testing"
)

// TestTools pins the terminal_ tools this package hands the server.
func TestTools(t *testing.T) {
	var names []string
	for _, tool := range Tools {
		names = append(names, tool.Name)
	}
	if want := []string{"terminal_sessions", "terminal_screen", "terminal_read", "terminal_wait", "terminal_send", "terminal_exec"}; !slices.Equal(names, want) {
		t.Errorf("Tools = %v, want %v", names, want)
	}
}
