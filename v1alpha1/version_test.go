package v1alpha1

import (
	"net/url"
	"strings"
	"testing"

	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
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

// TestVersionLineNamesTheCache pins the third number a bug report needs. Which
// spec a run replayed is the question behind every report of a hostname that
// changed when it should not have, and the key is the whole answer.
func TestVersionLineNamesTheCache(t *testing.T) {
	u, err := url.Parse("http://localhost:3000")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	o := origins.New(origins.WithDir("/work/project"), origins.WithURL(u))

	line := VersionLine(o)
	if !strings.Contains(line, "cache "+o.Key()) {
		t.Errorf("VersionLine() = %q, want the cache key %q in it", line, o.Key())
	}
	if !strings.Contains(line, Version()) {
		t.Errorf("VersionLine() = %q, want the version in it", line)
	}

	// Nothing to name before a run has origins — the frame's banner is built
	// in New, before a flag has been parsed.
	for _, empty := range []Origins{nil, origins.New(origins.WithDir("/work/project"))} {
		if got := VersionLine(empty); strings.Contains(got, "cache ") {
			t.Errorf("VersionLine(%v) = %q, want no cache clause", empty, got)
		}
	}
}
