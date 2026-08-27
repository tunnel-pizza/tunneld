// Package lib is tunneld's thin, stable façade over the stable/alpha versioned
// packages. Application code — including tunneld's own main — imports only
// this package.
//
// The tunneld tier is split into three pieces:
//
//   - github.com/tunnel-pizza/tunneld/lib (this package) — the façade: New,
//     Version, and aliases for the caller-facing types. Stable surface.
//   - github.com/tunnel-pizza/tunneld/v1 — the stable Builder contract, plus
//     the Err* sentinels and the environment/default constants. Application
//     code normally reaches these through the aliases here; import v1 directly
//     to implement the interface or to reach a symbol the façade doesn't
//     re-export.
//   - github.com/tunnel-pizza/tunneld/v1alpha1 — the current implementation:
//     the command assembly, the tunnel it runs, the version resolution.
//     Internals may change between alpha revisions; pin only if you need
//     direct access to the struct.
//
// New() returns a Builder you configure with With* methods and finalize with
// Build, which yields a *cobra.Command ready to Execute:
//
//	cmd := lib.New().WithURL("http://localhost:3000").Build()
//	if err := cmd.ExecuteContext(ctx); err != nil { ... }
package lib

import (
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

// BuilderV1 is the builder returned by New: an alias for v1.Builder,
// re-exported so callers can name the type without importing v1. The V1 suffix
// versions the name — when a v2 contract lands, BuilderV2 can sit beside this
// one in the same façade and callers migrate type by type instead of all at
// once. Alias, not a defined type: the two spellings are the same type, so a
// v1.Builder from anywhere satisfies a BuilderV1 and back.
type BuilderV1 = v1.Builder

// DefaultProvider is the quick-tunnel service tunneld mints against,
// re-exported from v1 so a caller naming it needn't import the contract.
const DefaultProvider = v1.DefaultProvider

// New returns an unconfigured Builder for the tunneld command. Configure it
// with the With* methods, then call Build.
//
//	cmd := lib.New().WithURL("http://localhost:3000").Build()
func New() BuilderV1 {
	return v1alpha1.New()
}

// Version reports the tunneld release this build is — e.g. "v0.0.5". It
// matches the git tag, so a consumer can log or report the exact version it
// runs.
func Version() string { return v1alpha1.Version() }

// VersionLine is the human-facing build banner: the tunneld build identifier,
// the tunnel library it links against, and the Go toolchain that built it.
func VersionLine() string { return v1alpha1.VersionLine() }
