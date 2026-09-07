// The tests for cachedir.go. `package cachedir_test` is the outside-the-package
// view: New, Add and the pflag methods are the whole surface, and what a
// ValueImpl resolves to from a sequence of Set/Append/Replace calls is the
// contract worth pinning. The boolean-entry, absolute-path and dedup rules
// Add itself follows are pinned through the command in builder_test.go's
// TestCacheDir and TestDefaultCacheDir.
package cachedir_test

import (
	"path/filepath"
	"testing"

	"github.com/tunnel-pizza/tunneld/v1alpha1/cachedir"
)

// abs resolves a path the same way Add does, so a want and a got are
// comparable regardless of how t.TempDir spelled it on this platform.
func abs(t *testing.T, dir string) string {
	t.Helper()
	a, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", dir, err)
	}
	return a
}

// TestSetReplacesThenAppends pins the flag's own rule: the first --cache-dir
// on a command line replaces whatever was seeded, and every one after that
// appends. Set is what the flag package calls per occurrence, so this is
// pflag's own repeat-flag behaviour rather than anything cachedir invents.
func TestSetReplacesThenAppends(t *testing.T) {
	a, b := abs(t, t.TempDir()), abs(t, t.TempDir())

	v := cachedir.New()
	v.Add("/seed")
	if err := v.Set(a); err != nil {
		t.Fatalf("Set(%q): %v", a, err)
	}
	if err := v.Set(b); err != nil {
		t.Fatalf("Set(%q): %v", b, err)
	}

	if got, want := v.GetSlice(), []string{a, b}; !equal(got, want) {
		t.Errorf("GetSlice() = %v, want %v (Set should clear the seed on its first call, then append)", got, want)
	}
}

// TestReplaceClears pins that Replace, like Set's first call, discards
// whatever came before it rather than merging into it — the whole-variable
// swap the environment binding in Command relies on.
func TestReplaceClears(t *testing.T) {
	x := abs(t, t.TempDir())

	v := cachedir.New()
	v.Add("/seed")
	if err := v.Replace([]string{x}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if got, want := v.GetSlice(), []string{x}; !equal(got, want) {
		t.Errorf("GetSlice() after Replace = %v, want %v", got, want)
	}
}

// TestGetSliceTellsUnsetFromOff pins the distinction Command depends on to
// leave a deliberately emptied list alone: nil means nothing has ever been
// added, and a non-nil empty slice means a false entry turned the list off.
func TestGetSliceTellsUnsetFromOff(t *testing.T) {
	v := cachedir.New()
	if got := v.GetSlice(); got != nil {
		t.Errorf("GetSlice() on a fresh ValueImpl = %v, want nil", got)
	}

	v.Add("false")
	got := v.GetSlice()
	if got == nil {
		t.Fatal("GetSlice() after Add(\"false\") = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("GetSlice() after Add(\"false\") = %v, want empty", got)
	}
}

// TestString pins the pflag.Value rendering used in --help and DefValue: a
// comma-joined list inside brackets, the same shape pflag's own stringArray
// uses.
func TestString(t *testing.T) {
	a, b := abs(t, t.TempDir()), abs(t, t.TempDir())

	v := cachedir.New()
	v.Add(a, b)
	if got, want := v.String(), "["+a+","+b+"]"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
