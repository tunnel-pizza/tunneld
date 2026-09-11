// Package v1alpha1 is the current implementation behind the v1.Builder
// interface: the command builder, the tunnel it runs, and the version
// resolution behind the build banner. Application code constructs from here
// (New, configured by this package's With* options) and matches errors
// against v1. Anything here may change between alpha revisions — depend on
// the v1 contract, not these internals.
package v1alpha1

import (
	"context"
	"io"
	"net/url"
	"sync"
	"time"

	"github.com/cnuss/libtunnel"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/docker"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cachedir"
	"github.com/tunnel-pizza/tunneld/v1alpha1/console"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
	"github.com/tunnel-pizza/tunneld/v1alpha1/display"
	"github.com/tunnel-pizza/tunneld/v1alpha1/engine"
	"github.com/tunnel-pizza/tunneld/v1alpha1/identity"
	"github.com/tunnel-pizza/tunneld/v1alpha1/identity/github"
	"github.com/tunnel-pizza/tunneld/v1alpha1/logs"
)

// Option configures a BuilderImpl at construction. The nine builder options
// in builder.go seed what the command's flags default to; the contract
// options in this file replace a collaborator Command's RunE composes.
type Option = v1.Option[*BuilderImpl]

// Origins is the local origins a run exposes — v1's type, aliased so this
// package can name it without a prefix. Not a contract: it has no external
// effect and nothing to replace, so it carries no With* option and takes no
// seat in the list below.
type Origins = v1.Origins

// The contracts Command's RunE composes. Each is something with an external
// effect — the edge, the disk, the daemon, the browser, an HTTP probe —
// implemented once in a v1alpha1/<name> subpackage, seeded by New, and
// replaceable with the matching With* option below. A function that maps a
// value to a value gets no contract; see CONTRIBUTING.

// CacheDirs is the --cache-dir list: the directories tunnel specs are
// cached in, with the boolean-entry and absolute-path rules
// cachedir.ValueImpl.Add documents, bound onto the flag as a pflag value.
// GetSlice is nil until something is added and empty after a false entry,
// which is how Command tells "unset" from "turned off".
type CacheDirs interface {
	pflag.Value
	pflag.SliceValue
	// Add appends directories by the list's rules: a boolean entry is an
	// instruction, entries become absolute, repeats collapse, and a false
	// entry empties the list and holds it empty.
	Add(dirs ...string)
}

// WithCacheDirs replaces the list implementation. The default is
// cachedir.New(). Not to be confused with WithCacheDir, which adds entries
// to whichever list is there.
func WithCacheDirs(c CacheDirs) Option {
	return func(b *BuilderImpl) { b.cacheDirs = c }
}

// Engine mints or replays the tunnel run drives. spec is a cached envelope to
// replay, "" to mint; provider is the quick-tunnel host, "" for the default;
// token is the mint credential, "" for an anonymous mint.
type Engine interface {
	Tunnel(spec, provider, token string) libtunnel.TunnelV1
}

// WithEngine replaces what mints or replays the tunnel. The default is
// engine.New(), libtunnel's Cloudflare backend; a test hands in a fake.
func WithEngine(e Engine) Option {
	return func(b *BuilderImpl) { b.engine = e }
}

// Cache persists a tunnel's spec between runs, in the directories
// --cache-dir settled on.
type Cache interface {
	Load(dirs []string, log v1.Logger) string
	Save(dirs []string, log v1.Logger)
	Discard(dirs []string, log v1.Logger)
}

// WithCache replaces where a tunnel's spec is kept between runs. The default
// is cache.New(), a TUNNEL.env in each directory.
func WithCache(c Cache) Option {
	return func(b *BuilderImpl) { b.cache = c }
}

// Console is the screen a run was started on, when it turns out to be one.
//
// For is the whole of it, and it answers rather than asks: given the bound
// origins and the command's own streams, it says nil when there is no screen —
// nothing to show, or nowhere to show it — and one ready to be handed over
// otherwise. Nothing out here counts origins or tests a stream — the binder
// already decided the first by carrying Show, and the second is a question
// about streams that the thing drawing on them should be the one to ask.
//
// What comes back is what the browser package takes when it decides a console
// is what this run gets shown on: one interface, declared where the console
// is, named by the package that chooses between it and a tab.
type Console interface {
	For(bound attach.Bound, streams console.Streams) console.Screen
}

// Display puts the tunnel in front of a person: it answers the bare public
// address when several origins have to share it, and it opens that address
// once the edge serves it.
//
// URL and Interceptors are two halves of one decision and answer over the
// same condition — "" and no interceptors when there is no panel to serve —
// so the caller reads an answer rather than asking whether to ask.
//
// Open reads the same way. It is told what the run is doing, in the options it
// takes, and decides for itself whether that means a browser — there is no
// "should I" for a caller to answer, and no second place where opening one is
// decided.
type Display interface {
	URL(enabled bool, public *url.URL, origins []*url.URL) string
	Interceptors(enabled bool, origins []*url.URL, log v1.Logger) []libtunnel.Interceptor
	Open(ctx context.Context, log v1.Logger, opts ...display.Option)
}

// WithDisplay replaces what serves the tunnel's bare address and opens it
// once the tunnel is live. The default is display.New(): the panel from
// multiview.html, and the host's browser launched as-is.
func WithDisplay(display Display) Option {
	return func(b *BuilderImpl) { b.display = display }
}

// Counter folds tunnel events into a verdict: has the edge disowned it.
type Counter interface {
	Count(e libtunnel.Event)
	IsGone() bool
	IsEstablished() bool
	Established(ctx context.Context, cancel context.CancelFunc) <-chan struct{}
}

// DefaultEstablishDeadline is how long New arms a builder to wait for the
// public URL to answer before reporting addresses anyway. Ten seconds is far
// longer than the gap has been observed to be — under a second — and it only
// ever costs that much when something is wrong, in which case the run carries
// on rather than never reporting at all.
const DefaultEstablishDeadline = 10 * time.Second

// WithEstablishDeadline bounds the wait for the public URL to answer. Zero
// reports the addresses without waiting at all.
func WithEstablishDeadline(d time.Duration) Option {
	return func(b *BuilderImpl) { b.establishDeadline = d }
}

// WithCounter replaces the counter that decides when the edge has disowned
// the tunnel. The default is counter.New(), armed at counter.DefaultMaxGone.
func WithCounter(c Counter) Option {
	return func(b *BuilderImpl) { b.counter = c }
}

// Binder turns the origins the operator typed into the origins the tunnel
// dials, standing a loopback server in for each origin that names something to
// serve rather than an address to reach — a container, a local program. The dialable list
// keeps shown's length and order — index n means origin n everywhere
// downstream — and the closer shuts every server the binding started.
type Binder interface {
	Bind(ctx context.Context, shown []*url.URL, log v1.Logger) (dialable []*url.URL, bound attach.Bound, err error)
}

// WithConsole replaces the console a run may draw its terminal on. Seeded by
// New with the log ring and the hint; a caller replaces it to draw somewhere
// else, or to draw nothing.
func WithConsole(c Console) Option {
	return func(b *BuilderImpl) { b.console = c }
}

// Identity resolves an ordered list of provider names to the credential a mint
// request should carry. Nothing a run does depends on finding one: a machine
// with no identity mints anonymously, which is what every machine did before
// this existed.
type Identity interface {
	// Known reports whether every name in the list has a provider behind it,
	// and the error a typo earns. Asked before anything external happens.
	Known(names []string) error
	// Token is what the first provider to answer found, or "" when none did.
	// It is a credential: it is never logged and never put in an error.
	Token(ctx context.Context, names []string, log v1.Logger) string
}

// WithIdentity replaces what a run resolves its mint credential through. The
// default is identity.New(identity.WithProviders(github.New())): the providers
// a list may name, each of which knows one kind of machine.
func WithIdentity(i Identity) Option {
	return func(b *BuilderImpl) { b.identity = i }
}

// WithBinder replaces what stands a loopback origin in for a container or a
// program. The default is
// attach.New(attach.WithTargets(docker.New(), shell.New()), attach.WithBanner(…)):
// attach serves, and each provider resolves the one scheme it answers.
func WithBinder(binder Binder) Option {
	return func(b *BuilderImpl) { b.binder = binder }
}

// The defaults satisfy their contracts, checked here so a drift fails the
// build rather than the first run.
var (
	_ v1.Builder = (*BuilderImpl)(nil)
	_ CacheDirs  = (*cachedir.ValueImpl)(nil)
	_ Engine     = (*engine.EngineImpl)(nil)
	_ Cache      = (*cache.CacheImpl)(nil)
	_ Display    = (*display.DisplayImpl)(nil)
	_ Counter    = (*counter.CounterImpl)(nil)
	_ Binder     = (*attach.BinderImpl)(nil)
	_ Console    = (*console.ConsoleImpl)(nil)
)

// New returns a BuilderImpl carrying its defaults, then configured by opts.
// It is the entry point for application code, and satisfies v1.Builder.
//
// Two tiers, two calls: the defaults go first, in the same vocabulary a caller
// uses to override them, and the caller's options after so a later one wins.
// Of the flag seeds, only the booleans need seeding here — their defaults are
// on, and a bool field cannot express "unset" separately from "off". Setting
// them here rather than at the flag binding keeps one rule for every knob: a
// flag's default is always the field it binds over, so WithMultiview(false) is
// honoured exactly like every other seed.
//
// WithOpen is the exception, and is not seeded: whether to open a browser is
// derived per run rather than defaulted, so "unset" is the state that matters
// and its field is a pointer for exactly that reason.
func New(opts ...Option) *BuilderImpl {
	// Built before the builder, because two things need the same one: the
	// terminal, which shows the lines, and the logger the command assembles
	// later — by which time the binder has already been constructed with it.
	recent := logs.New()

	b := v1.Apply(&BuilderImpl{recent: recent},
		WithEstablishDeadline(DefaultEstablishDeadline),
		WithMultiview(v1.DefaultMultiview),
		WithShellFallback(v1.DefaultShellFallback),
		WithIdentityProviders(splitList(v1.DefaultIdentityProviders)...),
		WithIdentity(identity.New(identity.WithProviders(github.New()))),
		WithCacheDirs(cachedir.New()),
		WithEngine(engine.New()),
		WithCache(cache.New()),
		WithDisplay(display.New()),
		WithCounter(counter.New()),
		WithConsole(console.New(
			console.WithLogs(recent),
			console.WithHint(stopHint),
		)),
		WithBinder(attach.New(
			attach.WithTargets(docker.New(), shell.New()),
			attach.WithBanner(VersionLine()),
			attach.WithLogs(recent),
		)),
	)
	return v1.Apply(b, opts...)
}

// BuilderImpl is the default Builder implementation. Its fields are the
// command's flag targets as well as its seeded defaults: Command binds each
// flag over the field it defaults from, so an argv value simply overwrites
// the seed and there is no second copy of the configuration to keep in sync.
type BuilderImpl struct {
	name      string
	origins   []string
	provider  string
	logLevel  string
	multiview bool

	// open is what WithOpen wrote, and nil is nobody having written anything.
	// A pointer because the three states are real: open, do not open, and
	// work it out — the last of which is the one nearly every run wants, and
	// a bool cannot hold beside the other two.
	open *bool

	// identityProviders is the ordered list a run looks for a mint credential
	// with. Flag-backed like the knobs above; empty is the lookup turned off,
	// and a run that finds nothing mints anonymously.
	identityProviders []string

	// shellFallback is whether Origins answers "nothing settled anywhere"
	// with $SHELL. Flag-backed like the two above, so an embedding program
	// seeds it, an operator overrides it, and neither has to reach into the
	// process environment to find out what a run will do.
	shellFallback bool

	// establishDeadline bounds the wait for the public URL to answer before
	// the addresses are reported. The counter that answers that wait
	// takes no deadline of its own — a caller that will not wait forever
	// brings a context that will not either — so the bound lives here, where
	// the policy is. New always seeds it.
	establishDeadline time.Duration

	// The collaborators Command's RunE composes, each behind a contract
	// declared above. Seeded by New; a test or a contributor swaps one with
	// its With* option.
	cacheDirs CacheDirs
	engine    Engine
	cache     Cache
	display   Display
	counter   Counter
	binder    Binder
	identity  Identity

	// stdout carries the help text and version banner, stderr the tunnel's
	// own banner, the origin map, and the tunnel's logs. They are staging
	// only: Command hands them to the command with SetOut/SetErr and
	// everything downstream reads them back through
	// OutOrStdout/ErrOrStderr, so cobra stays the single owner of where
	// output goes. Nil means whatever cobra defaults to.
	stdout, stderr io.Writer

	// recent keeps tunneld's own log lines so a terminal can show them. The
	// command wraps its log handler in this, and the binder was handed the
	// same one at construction — see New, and Logs in v1alpha1/attach.
	recent *logs.RingImpl

	// console is the screen this run was started on, seeded with what
	// outlives a single run — the ring to keep off a drawn frame, and the
	// line to leave on a returned prompt. Run binds it to the origins and
	// streams it actually got, and the browser package decides whether it is
	// what the tunnel gets put in front of.
	console Console

	// Command assembles once; subsequent calls return the cached command.
	commandOnce sync.Once
	command     *cobra.Command

	// env is this builder's environment binding — every flag's variable, plus
	// the origins key that has no flag to hang off. Per-builder and never
	// viper's package global, so two commands in one process (a host
	// program's and an embedded tunneld's) do not share one key space, and
	// neither do two tests in one binary.
	//
	// Built on first use rather than in New, because Origins reads it and
	// may be called on a builder whose command was never assembled.
	envOnce sync.Once
	env     *viper.Viper
}
