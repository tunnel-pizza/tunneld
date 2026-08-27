// Package golib is a thin, stable façade over stable/alpha versioned packages.
//
// The package is split into three pieces:
//
//   - golib (this package) — thin façade exposing New, Version, and aliases
//     for the caller-facing types. Stable surface for application code.
//   - github.com/cnuss/golib/v1 — the stable Builder[T] interface and Result
//     type, plus the Err* sentinels and *Env constants. Application code
//     normally reaches these through the aliases here; import v1 directly to
//     implement the interface or to reach a symbol the façade doesn't re-export.
//   - github.com/cnuss/golib/v1alpha1 — the current implementation. Internals
//     (BuilderImpl, helpers) may change between alpha revisions; pin only if
//     you need direct access to the struct.
//
// New[T]() returns a Builder[T] you configure with With* methods and finalize
// with Build().
package golib

import (
	"runtime/debug"

	v1 "github.com/cnuss/golib/v1"
	"github.com/cnuss/golib/v1alpha1"
)

// BuilderV1 is the builder returned by New: an alias for v1.Builder[T],
// re-exported so callers can name the type without importing v1. The V1 suffix
// versions the root name — when a v2 contract lands, BuilderV2 can sit beside
// this one in the same façade and callers migrate type by type instead of all
// at once. Alias, not a defined type: the two spellings are the same type, so
// a v1.Builder[T] from anywhere satisfies a BuilderV1[T] and back.
type BuilderV1[T any] = v1.Builder[T]

// The supporting types, re-exported from v1 so application code imports only
// the root package. These are unversioned: they are data carried across the
// contract rather than the contract itself, so a v2 that keeps them keeps
// these names.
type (
	Result[T any] = v1.Result[T] // structured output of Build: Name + Value
)

// modulePath is golib's import path — used to find golib's own entry in an
// importer's build info.
const modulePath = "github.com/cnuss/golib"

// version is stamped into a release binary via
// -ldflags "-X github.com/cnuss/golib.version=<tag>". The library ships no
// binary of its own, so nothing here sets it and it stays empty — Version
// derives the value from build info instead. A fork that grows a cmd/ gets
// stamping by passing that ldflag from its build.
var version string

// Version reports the golib release this build links against — e.g. "v0.0.5".
// It matches the git tag, so a consumer can log or report the exact library
// version it compiles against.
//
// Resolution, in order: the release stamp (set only in a build that passes the
// ldflag); the module version recorded in the importer's build info (the
// common consumer case — the version required in their go.mod, following a
// replace directive if one redirects it); the main-module version; and finally
// the short VCS revision of a local build, with a -dirty suffix for an
// uncommitted tree. A build carrying no version information at all returns
// "unknown", never the empty string — Version always self-identifies.
func Version() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	// Consumer build: golib is a dependency — return the module version the
	// importer pinned (following a replace directive if one redirects it).
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
	// golib is the main module (its own binary, or its tests): the main
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

// New returns an unconfigured Builder for values of type T. Configure it with
// the With* methods, then call Build.
//
//	res := golib.New[string]().WithName("greeting").WithValue("hello").Build()
func New[T any]() BuilderV1[T] {
	return v1alpha1.New[T]()
}
