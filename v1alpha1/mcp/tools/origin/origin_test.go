package origin

import (
	"slices"
	"testing"
)

// TestTools pins the origin_ tools this package hands the server.
func TestTools(t *testing.T) {
	var names []string
	for _, tool := range Tools {
		names = append(names, tool.Name)
	}
	if want := []string{"origin_add", "origin_remove"}; !slices.Equal(names, want) {
		t.Errorf("Tools = %v, want %v", names, want)
	}
}
