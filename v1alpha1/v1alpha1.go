// Package v1alpha1 is the current implementation behind the v1.Builder
// interface: the command builder, the tunnel it runs, and the version
// resolution behind the build banner. Application code constructs from here
// (New, configured by this package's With* options) and matches errors
// against v1. Anything here may change between alpha revisions — depend on
// the v1 contract, not these internals.
package v1alpha1

import (
	"io"
	"sync"

	"github.com/spf13/cobra"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Option configures a BuilderImpl at construction. The nine builder options
// in builder.go seed what the command's flags default to; the contract
// options in this file replace a collaborator run composes.
type Option = v1.Option[*BuilderImpl]

// New returns a BuilderImpl carrying its defaults, then configured by opts.
// It is the entry point for application code, and satisfies v1.Builder.
//
// Two tiers, two calls: the defaults go first, in the same vocabulary a
// caller uses to override them, and the caller's options after so a later
// one wins. Only the booleans need seeding here — their defaults are on, and
// a bool field cannot express "unset" separately from "off". Setting them
// here rather than at the flag binding keeps one rule for every knob: a
// flag's default is always the field it binds over, so WithOpen(false) is
// honoured exactly like every other seed.
func New(opts ...Option) *BuilderImpl {
	b := v1.Apply(&BuilderImpl{counter: NewCounter().WithMaxGone(3)},
		WithOpen(v1.DefaultOpen),
		WithMultiview(v1.DefaultMultiview),
	)
	return v1.Apply(b, opts...)
}

// BuilderImpl is the default Builder implementation. Its fields are the
// command's flag targets as well as its seeded defaults: Build binds each flag
// over the field it defaults from, so an argv value simply overwrites the
// seed and there is no second copy of the configuration to keep in sync.
type BuilderImpl struct {
	name string
	urls []string
	// cacheDirs distinguishes nil from empty, and that is the whole of the
	// cache switch's state. Nil is unset, and Build fills it with the working
	// directory. Empty but not nil is a list a false entry emptied on
	// purpose, which Build leaves alone and WithCacheDir will not add to. A
	// later source starts over by setting the field back to nil.
	cacheDirs []string
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

	// TODO Doc
	counter *Counter

	// stdout carries the help text and version banner, stderr the tunnel's own
	// banner, the origin map, and
	// the tunnel's logs. They are staging only: Build hands them to the
	// command with SetOut/SetErr and everything downstream reads them back
	// through OutOrStdout/ErrOrStderr, so cobra stays the single owner of
	// where output goes. Nil means whatever cobra defaults to.
	stdout, stderr io.Writer

	// Build assembles once; subsequent calls return the cached command.
	builtOnce sync.Once
	built     *cobra.Command
}
