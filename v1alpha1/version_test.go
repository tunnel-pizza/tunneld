package v1alpha1

import (
	"runtime/debug"
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
// version, VCS revision, or "devel") and never returns "".
func TestVersionFallbackNonEmpty(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = ""
	if got := Version(); got == "" {
		t.Error("Version() = empty with no stamp, want a derived identifier")
	}
}

// TestVCSVersion covers the local-build fallback against synthetic build info,
// which is the only way to exercise every branch — a real `go test` binary
// carries whichever stamp the toolchain chose to embed.
func TestVCSVersion(t *testing.T) {
	const long = "0123456789abcdef0123456789abcdef01234567"

	cases := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name: "revision truncated to 12",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: long},
			},
			want: "0123456789ab",
		},
		{
			name: "dirty tree suffixed",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: long},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "0123456789ab-dirty",
		},
		{
			name: "clean tree unsuffixed",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: "abc123",
		},
		{
			name:     "no vcs stamp at all",
			settings: nil,
			want:     "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vcsVersion(&debug.BuildInfo{Settings: tc.settings})
			if got != tc.want {
				t.Errorf("vcsVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVersionDevelBeatsUnknown pins the `go run` case. That toolchain path
// skips VCS stamping, so the only thing build info carries is Main.Version
// "(devel)" — and reporting "unknown" there reads as a broken build rather
// than an unreleased one.
func TestVersionDevelBeatsUnknown(t *testing.T) {
	if got := vcsVersion(&debug.BuildInfo{}); got != "unknown" {
		t.Fatalf("vcsVersion with no settings = %q, want the unknown sentinel", got)
	}
	// Version's own "(devel)" branch is what turns that into "devel"; the
	// build running this test may carry a real stamp, so assert the rule
	// rather than the value.
	if got := Version(); got == "" || got == "unknown" {
		t.Errorf("Version() = %q, want a self-identifying value", got)
	}
}
