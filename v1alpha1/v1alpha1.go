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
	"github.com/tunnel-pizza/tunneld/v1alpha1/browser"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cachedir"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
	"github.com/tunnel-pizza/tunneld/v1alpha1/engine"
	"github.com/tunnel-pizza/tunneld/v1alpha1/logs"
)

// Option configures a BuilderImpl at construction. The nine builder options
// in builder.go seed what the command's flags default to; the contract
// options in this file replace a collaborator Command's RunE composes.
type Option = v1.Option[*BuilderImpl]

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
	Load(dirs []string, log v1.Logger) string
	Save(dirs []string, log v1.Logger)
	Discard(dirs []string, log v1.Logger)
}

// WithCache replaces where a tunnel's spec is kept between runs. The default
// is cache.New(), a TUNNEL.env in each directory.
func WithCache(c Cache) Option {
	return func(b *BuilderImpl) { b.cache = c }
}

// Browser puts the tunnel in front of a person: it answers the bare public
// address when several origins have to share it, and it opens that address
// once the edge serves it.
//
// URL and Interceptors are two halves of one decision and answer over the
// same condition — "" and no interceptors when there is no panel to serve —
// so the caller reads an answer rather than asking whether to ask.
type Browser interface {
	URL(enabled bool, public *url.URL, origins []*url.URL) string
	Interceptors(enabled bool, origins []*url.URL, log v1.Logger) []libtunnel.Interceptor
	Open(ctx context.Context, addr string, stderr io.Writer, log v1.Logger)
}

// WithBrowser replaces what serves the tunnel's bare address and opens it
// once the tunnel is live. The default is browser.New(): the panel from
// multiview.html, and the host's browser launched as-is.
func WithBrowser(browser Browser) Option {
	return func(b *BuilderImpl) { b.browser = browser }
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
// keeps display's length and order — index n means origin n everywhere
// downstream — and the closer shuts every server the binding started.
type Binder interface {
	Bind(ctx context.Context, display []*url.URL, log v1.Logger) (dialable []*url.URL, close io.Closer, err error)
}

// Announcer is a bound origin that can be told the public address it answers
// on, once the tunnel has one.
//
// Discovered on the closer Bind returns rather than required by Binder, and
// that is an import direction rather than a preference: an implementation
// lives in a subpackage, the subpackage cannot name a type declared here, and
// a contract whose method signature mentions one could never be satisfied from
// there. So the capability is optional in the way http.Flusher is — a binder
// that has nothing to announce simply is not one.
//
// The addresses are indexed the way display was, which is the same rule
// everything downstream of Bind already follows.
type Announcer interface {
	Announce(public []string)
}

// Quitter is a bound origin that can be asked, from inside, to end the run.
//
// Discovered on the closer Bind returns, the same way Announcer is and for the
// same reason: an implementation lives in a subpackage and cannot name a type
// declared here. A binder with nothing to ask on simply is not one.
//
// The channel closes at most once and carries nothing. What it means is that
// somebody watching a terminal chose to end the process serving it — the one
// way out of a terminal you opened from your own machine.
type Quitter interface {
	Quit() <-chan struct{}
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
	_ Browser    = (*browser.BrowserImpl)(nil)
	_ Counter    = (*counter.CounterImpl)(nil)
	_ Binder     = (*attach.BinderImpl)(nil)
)

// New returns a BuilderImpl carrying its defaults, then configured by opts.
// It is the entry point for application code, and satisfies v1.Builder.
//
// Two tiers, two calls: the defaults go first, in the same vocabulary a caller
// uses to override them, and the caller's options after so a later one wins.
// Of the flag seeds, only the booleans need seeding here — their defaults are
// on, and a bool field cannot express "unset" separately from "off". Setting
// them here rather than at the flag binding keeps one rule for every knob: a
// flag's default is always the field it binds over, so WithOpen(false) is
// honoured exactly like every other seed.
func New(opts ...Option) *BuilderImpl {
	// Built before the builder, because two things need the same one: the
	// terminal, which shows the lines, and the logger the command assembles
	// later — by which time the binder has already been constructed with it.
	recent := logs.New()

	b := v1.Apply(&BuilderImpl{recent: recent},
		WithOpen(v1.DefaultOpen),
		WithEstablishDeadline(DefaultEstablishDeadline),
		WithMultiview(v1.DefaultMultiview),
		WithCacheDirs(cachedir.New()),
		WithEngine(engine.New()),
		WithCache(cache.New()),
		WithBrowser(browser.New()),
		WithCounter(counter.New()),
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

	// open is the seed WithOpen writes; noOpen is what --no-open binds over.
	// Two fields rather than one because the command line reads negative and
	// the Go knob reads positive: Command registers --no-open defaulting to
	// !open, and pflag writes that default straight into noOpen, so noOpen is
	// authoritative from Command onwards and open is only ever the seed it
	// came from.
	open   bool
	noOpen bool

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
	browser   Browser
	counter   Counter
	binder    Binder

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
