package v1alpha1

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/cnuss/libtunnel"
)

// modulePath is tunneld's import path — used to find tunneld's own entry in a
// build's info, which is where a `go install`ed binary records its version.
const modulePath = "github.com/tunnel-pizza/tunneld"

// version is stamped into a release binary via
// -ldflags "-X github.com/tunnel-pizza/tunneld/v1alpha1.version=<tag>". A `go build`
// or `go install` that doesn't pass it leaves this empty and Version derives
// the value from build info instead.
var version string

// Version reports the tunneld release this build is — e.g. "v0.0.5". It
// matches the git tag, so `tunneld version` names the exact artifact an
// operator is running.
//
// Resolution, in order: the release stamp (set only in a build that passes the
// ldflag); the module version recorded in build info (the `go install
// tunneld@v0.0.5` case, following a replace directive if one redirects it);
// the main-module version; and finally the short VCS revision of a local
// build, with a -dirty suffix for an uncommitted tree. A build carrying no
// version information at all returns "unknown", never the empty string —
// Version always self-identifies.
func Version() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	// tunneld linked as a dependency (a fork's binary, or a test binary of a
	// module that imports it) — return the module version that build pinned,
	// following a replace directive if one redirects it.
	for _, dep := range info.Deps {
		if dep.Path != modulePath {
			continue
		}
		if dep.Replace != nil {
			dep = dep.Replace
		}
		if dep.Version != "" {
			return dep.Version
		}
	}
	// tunneld is the main module (the normal case for this binary): the main
	// module version, then the VCS stamp.
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return vcsVersion(info)
}

// vcsVersion is the local-build fallback: the short VCS revision with a
// -dirty suffix for an uncommitted tree, or "unknown" when the build carries
// no VCS stamp.
func vcsVersion(info *debug.BuildInfo) string {
	var revision, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if revision == "" {
		return "unknown"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return revision + dirty
}

// VersionLine is the human-facing build banner printed by `tunneld version`
// and logged at startup. It names libtunnel too, since that is what actually
// speaks to the edge — a bug report needs both numbers.
func VersionLine() string {
	return fmt.Sprintf("tunneld %s (libtunnel %s, built %s)", Version(), libtunnel.Version(), runtime.Version())
}
