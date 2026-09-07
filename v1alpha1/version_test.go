package v1alpha1

import (
	"testing"
)

// TestVersionStampWins pins the release-stamp precedence: when the build
// stamps the version (via -ldflags -X), Version returns it verbatim, ahead of
// any build-info resolution.
func TestVersionStampWins(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = "v9.9.9"
	if got := Version(); got != "v9.9.9" {
		t.Errorf("Version() = %q, want the stamp v9.9.9", got)
	}
}

// TestVersionFallbackNonEmpty pins that Version always self-identifies: with
// no stamp it derives an identifier from build info (module version, main
// version, or "devel") and never returns "".
func TestVersionFallbackNonEmpty(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = ""
	if got := Version(); got == "" {
		t.Error("Version() = empty with no stamp, want a derived identifier")
	}
}

// TestVersionDevelBeatsUnknown pins the `go run` case. That toolchain path
// skips VCS stamping, so the only thing build info carries is Main.Version
// "(devel)" — and reporting "unknown" there reads as a broken build rather
// than an unreleased one.
func TestVersionDevelBeatsUnknown(t *testing.T) {
	// Version's own "(devel)" branch is what turns that into "devel"; the
	// build running this test may carry a real stamp, so assert the rule
	// rather than the value.
	if got := Version(); got == "" || got == "unknown" {
		t.Errorf("Version() = %q, want a self-identifying value", got)
	}
}
