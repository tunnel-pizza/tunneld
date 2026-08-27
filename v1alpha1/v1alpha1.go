// Package v1alpha1 is the current implementation behind the v1.Builder
// interface: the fluent command builder, the tunnel it runs, and the version
// resolution behind the build banner. The tunneld façade in lib wraps this;
// callers reaching directly into v1alpha1 use it for the concrete struct.
// Anything here may change between alpha revisions — depend on the v1
// contract, not these internals.
package v1alpha1

import (
	"io"
	"sync"

	"github.com/spf13/cobra"
)

// New returns an unconfigured BuilderImpl. The lib.New façade wraps this and
// returns the v1.Builder interface.
func New() *BuilderImpl {
	return &BuilderImpl{}
}

// BuilderImpl is the default Builder implementation. Its fields are the
// command's flag targets as well as its seeded defaults: Build binds each flag
// over the field it defaults from, so an argv value simply overwrites the
// seed and there is no second copy of the configuration to keep in sync.
type BuilderImpl struct {
	name     string
	urls     []string
	provider string
	logLevel string

	// stdout carries the public URLs, stderr the banner, the origin map, and
	// the tunnel's logs. They are staging only: Build hands them to the
	// command with SetOut/SetErr and everything downstream reads them back
	// through OutOrStdout/ErrOrStderr, so cobra stays the single owner of
	// where output goes. Nil means whatever cobra defaults to.
	stdout, stderr io.Writer

	// Build assembles once; subsequent calls return the cached command.
	builtOnce sync.Once
	built     *cobra.Command
}
