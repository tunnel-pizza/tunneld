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
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// New returns a BuilderImpl carrying the defaults that are not the zero value.
// It is the entry point for application code, and satisfies v1.Builder.
//
// Only the booleans need seeding: their defaults are on, and a bool field
// cannot express "unset" separately from "off". Setting them here rather than
// at the flag binding keeps one rule for every knob — a flag's default is
// always the field it binds over, so WithOpen(false) is honoured exactly like
// every other seed.
func New() *BuilderImpl {
	return &BuilderImpl{open: v1.DefaultOpen, multiview: v1.DefaultMultiview}
}

// BuilderImpl is the default Builder implementation. Its fields are the
// command's flag targets as well as its seeded defaults: Build binds each flag
// over the field it defaults from, so an argv value simply overwrites the
// seed and there is no second copy of the configuration to keep in sync.
type BuilderImpl struct {
	name      string
	urls      []string
	provider  string
	logLevel  string
	multiview bool

	// open is the seed WithOpen writes; noOpen is what --no-open binds over.
	// Two fields rather than one because the command line reads negative and
	// the Go knob reads positive: Build registers --no-open defaulting to
	// !open, and pflag writes that default straight into noOpen, so noOpen is
	// authoritative from Build onwards and open is only ever the seed it came
	// from.
	open   bool
	noOpen bool

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
