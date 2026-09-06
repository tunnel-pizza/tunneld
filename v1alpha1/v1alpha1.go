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

	"github.com/cnuss/libtunnel"
	"github.com/spf13/cobra"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
	"github.com/tunnel-pizza/tunneld/v1alpha1/engine"
)

// Option configures a BuilderImpl at construction. The nine builder options
// in builder.go seed what the command's flags default to; the contract
// options in this file replace a collaborator run composes.
type Option = v1.Option[*BuilderImpl]

// The contracts run composes. Each is something with an external effect —
// the edge, the disk, the daemon, the browser, an HTTP probe — implemented
// once in a v1alpha1/<name> subpackage, seeded by New, and replaceable with
// the matching With* option below. A function that maps a value to a value
// gets no contract; see CONTRIBUTING.

// Engine mints or replays the tunnel run drives. spec is a cached envelope to
// replay, "" to mint; provider is the quick-tunnel host, "" for the default.
type Engine interface {
	Tunnel(spec, provider string) libtunnel.TunnelV1
}

// WithEngine replaces what mints or replays the tunnel. The default is
// engine.New(), libtunnel's Cloudflare backend; a test hands in a fake.
func WithEngine(e Engine) Option {
	return func(b *BuilderImpl) { b.engine = e }
}

// Cache persists a tunnel's spec between runs, in the directories
// --cache-dir settled on.
type Cache interface {
	Cached(dirs []string, log v1.Logger) string
	Save(dirs []string, log v1.Logger)
	Discard(dirs []string, log v1.Logger)
}

// WithCache replaces where a tunnel's spec is kept between runs. The default
// is cache.New(), a TUNNEL.env in each directory.
func WithCache(c Cache) Option {
	return func(b *BuilderImpl) { b.cache = c }
}

// Counter folds tunnel events into a verdict: has the edge disowned it.
type Counter interface {
	Count(e libtunnel.Event)
	IsGone() bool
}

// WithCounter replaces the counter that decides when the edge has disowned
// the tunnel. The default is counter.New(), armed at counter.DefaultMaxGone.
func WithCounter(c Counter) Option {
	return func(b *BuilderImpl) { b.counter = c }
}

// The defaults satisfy their contracts, checked here so a drift fails the
// build rather than the first run.
var (
	_ v1.Builder = (*BuilderImpl)(nil)
	_ Engine     = (*engine.EngineImpl)(nil)
	_ Cache      = (*cache.CacheImpl)(nil)
	_ Counter    = (*counter.CounterImpl)(nil)
)

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
	b := v1.Apply(&BuilderImpl{},
		WithOpen(v1.DefaultOpen),
		WithMultiview(v1.DefaultMultiview),
		WithEngine(engine.New()),
		WithCache(cache.New()),
		WithCounter(counter.New()),
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

	// The collaborators run composes, each behind a contract declared above.
	// Seeded by New; a test or a contributor swaps one with its With* option.
	engine  Engine
	cache   Cache
	counter Counter

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
