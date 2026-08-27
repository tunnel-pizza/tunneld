package golib

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
// version, or VCS revision) and never returns "".
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
