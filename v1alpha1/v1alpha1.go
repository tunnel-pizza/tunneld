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
	"net/http"
	"sync"

	"github.com/cnuss/libtunnel"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/docker"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/console"
	"github.com/tunnel-pizza/tunneld/v1alpha1/display"
	"github.com/tunnel-pizza/tunneld/v1alpha1/identity"
	"github.com/tunnel-pizza/tunneld/v1alpha1/identity/anthropic"
	"github.com/tunnel-pizza/tunneld/v1alpha1/identity/github"
	"github.com/tunnel-pizza/tunneld/v1alpha1/logs"
	"github.com/tunnel-pizza/tunneld/v1alpha1/motd"
	"github.com/tunnel-pizza/tunneld/v1alpha1/pid"
	"github.com/tunnel-pizza/tunneld/v1alpha1/router"
	"github.com/tunnel-pizza/tunneld/v1alpha1/run"
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

// WithTunnelFactory replaces how a spec becomes a tunnel. The default is
// libtunnel.From: From("") mints fresh and From(spec) hints with what a run
// cached, so there is one call and nothing to choose between — which is why
// this is a function and not a contract with an implementation behind it.
// What it is for is a run that must not reach the edge: a test drives a fake
// tunnel through here, and an embedder with a tunnel of its own can do the
// same.
func WithTunnelFactory(from func(spec string) libtunnel.TunnelV1) Option {
	return func(b *BuilderImpl) { b.newTunnel = from }
}

// Cache is cache.Cache.
type Cache = cache.Cache

// Pid is pid.Pid.
type Pid = pid.Pid

// Log is logs.Log.
type Log = logs.Log

// WithLog replaces a run's logging. The default is logs.New(), which New also
// hands the console and the binder; a replacement leaves their log view and
// muting on the default's ring.
func WithLog(l Log) Option {
	return func(b *BuilderImpl) { b.log = l }
}

// WithPid sets how a run is found and handed back from outside it. The
// default is nil, neither: a program that mounts tunneld under its own name
// is a process -k would end whole, so it opts in here or stays out of reach.
// The tunneld binary passes pid.New().
func WithPid(p Pid) Option {
	return func(b *BuilderImpl) { b.pid = p }
}

// WithCache replaces where a tunnel's spec is kept between runs. The default
// is cache.New(), one file per tunnel under the user's cache directory.
//
// nil turns caching off, and is the state --no-cache leaves a run in: no
// caching is no cache, rather than a switch some other field has to be read
// against. It is what makes "off, but with an implementation configured"
// impossible to be in.
func WithCache(c Cache) Option {
	return func(b *BuilderImpl) { b.cache = c }
}

// noCache is the cache a run with caching off reads and writes through: a
// Load that finds nothing and a Save that keeps nothing. WithCache(nil) and
// --no-cache both land here inside Run, so the load and the save are one line
// each rather than a branch around a nil.
type noCache struct{}

func (noCache) Load(...cache.Option) string { return "" }
func (noCache) Save(...cache.Option)        {}
func (noCache) String() string              { return "" }
func (noCache) Handlers(string) map[string]func(http.ResponseWriter, *http.Request) {
	return nil
}
func (noCache) Secret() []byte                { return nil }
func (noCache) Key() string                   { return "" }
func (noCache) Grant(string) bool             { return false }
func (noCache) Mutable(string) (string, bool) { return "", false }
func (noCache) SetMutable(string, string)     {}

// Spec is never a new spec: a run with caching off keeps its tunnel.
func (noCache) Spec() <-chan string { return nil }

// WithCacheDir caches specs in dir rather than under the user's cache
// directory — a mounted volume in a container, a temporary directory in a
// test.
//
// A wrapper over WithCache rather than a field of its own, because the
// directory belongs to the cache and New has already built one by the time a
// caller's options run: the way to change where a spec lands is to hand over a
// cache that puts it there. An embedder wanting more than the directory
// reaches the same lever with cache.New and its own options.
//
// There is no flag and no variable behind this. A spec is credentials, and
// which directory holds them is the machine's answer rather than a run's.
func WithCacheDir(dir string) Option {
	return WithCache(cache.New(cache.WithDir(dir)))
}

// Console is console.Console.
type Console = console.Console

// Display is display.Display.
type Display = display.Display

// WithDisplay replaces what serves the tunnel's bare address and opens it
// once the tunnel is live. The default is display.New(display.WithMotd(board)):
// the panel from multiview.html, and the host's browser launched as-is, with
// board the one motd instance New also gives WithMotd, so the panel shows
// what the builder learned. A replacement that should show the messages is
// built with that same board.
func WithDisplay(display Display) Option {
	return func(b *BuilderImpl) { b.display = display }
}

// Router is router.Router.
type Router = router.Router

// WithRouter replaces what stands between the tunnel and the origins. The
// default is router.New().
func WithRouter(r Router) Option {
	return func(b *BuilderImpl) { b.router = r }
}

// Binder turns the origins the operator typed into the origins the tunnel
// dials, standing a loopback server in for each origin that names something to
// serve rather than an address to reach — a container, a local program. The dialable list
// keeps shown's length and order — index n means origin n everywhere
// downstream — and the closer shuts every server the binding started.
type Binder interface {
	Bind(ctx context.Context, shown Origins, log v1.Logger) (dialable Origins, bound attach.Bound, err error)
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

// Motd is motd.Motd.
type Motd = motd.Motd

// Auth is auth.Auth.
type Auth = auth.Auth

// Run is one run's tunnel, from its spec to its end: minted from the spec,
// brought up and put in front of everybody — the addresses, the map, the bound
// origins told where they answer from, the provider's messages, the browser
// or the console, the save, a waiting launcher handed back — and waited on
// until the run is over. Everything the run settled before its first tunnel,
// and every collaborator a tunnel is shown through, is handed over with the
// call. Nil for a run told to stop; the tunnel's error when it fails to come
// up or ends on its own.
type Run interface {
	Run(ctx context.Context, opts ...run.Option) error
}

// WithMotd replaces what keeps and renders the provider's messages of the day.
// The default is motd.New(), shared into the binder and the display; a
// replacement is shared the same way by whoever builds it:
//
//	board := motd.New()
//	v1alpha1.New(v1alpha1.WithMotd(board),
//	    v1alpha1.WithDisplay(display.New(display.WithMotd(board))),
//	    v1alpha1.WithBinder(attach.New(attach.WithMotd(board), …)))
func WithMotd(m Motd) Option {
	return func(b *BuilderImpl) { b.motd = m }
}

// WithAuth replaces what stands between a visitor and everything the tunnel
// serves. The default is auth.New, reading the tunnel secret from the cache
// the run uses.
func WithAuth(a Auth) Option {
	return func(b *BuilderImpl) { b.auth = a }
}

// secret is the running tunnel's secret, from the cache this run uses: what
// the default auth keys its cookie with.
func (b *BuilderImpl) secret() []byte {
	if b.runCache == nil {
		return nil
	}
	return b.runCache.Secret()
}

// WithRun replaces what takes a run's tunnel from its spec to its end. The
// default is run.New().
func WithRun(r Run) Option {
	return func(b *BuilderImpl) { b.run = r }
}

// WithBinder replaces what stands a loopback origin in for a container or a
// program. The default is
//
//	attach.New(attach.WithTargets(docker.New(), shell.New()), attach.WithBanner(…),
//	    attach.WithLogs(log), attach.WithMotd(board))
//
// attach serves, and each provider resolves the one scheme it answers. log
// is the logging New shares with the console and board the motd instance it
// gives WithMotd; a replacement built without them serves a frame whose logs
// view is empty and with no messages above it.
func WithBinder(binder Binder) Option {
	return func(b *BuilderImpl) { b.binder = binder }
}

// The defaults satisfy their contracts, checked here so a drift fails the
// build rather than the first run.
var (
	_ v1.Builder = (*BuilderImpl)(nil)
	_ Cache      = (*cache.CacheImpl)(nil)
	_ Display    = (*display.DisplayImpl)(nil)
	_ Binder     = (*attach.BinderImpl)(nil)
	_ Router     = (*router.RouterImpl)(nil)
	_ Console    = (*console.ConsoleImpl)(nil)
	_ Motd       = (*motd.MotdImpl)(nil)
	_ Auth       = (*auth.AuthImpl)(nil)
	_ Run        = (*run.RunImpl)(nil)
	_ Log        = (*logs.LogImpl)(nil)
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
	// The first thing built, because everything after it may log: the one
	// logger a run has, which the console mutes and the binder's frames read
	// the recent lines of, and the command points at stderr once it knows
	// the level.
	log := logs.New()

	// One board for the three places a message is shown, built before the
	// builder for the same reason the ring is: the binder and the display
	// are constructed here with it, and the builder learns into it later.
	board := motd.New()

	// The builder first, so the default auth can read the run's secret off
	// it: which cache a run uses is only settled in Command.
	b := &BuilderImpl{log: log}
	b = v1.Apply(b,
		WithMultiview(v1.DefaultMultiview),
		WithShellFallback(v1.DefaultShellFallback),
		WithIdentityProviders(splitList(v1.DefaultIdentityProviders)...),
		WithIdentity(identity.New(identity.WithProviders(github.New(), anthropic.New()))),
		WithMotd(board),
		WithAuth(auth.New(auth.WithSecret(b.secret), auth.WithLog(log.Logger()))),
		WithRun(run.New()),
		WithTunnelFactory(libtunnel.From),
		WithCache(cache.New()),
		WithDisplay(display.New(display.WithMotd(board))),
		WithRouter(router.New()),
		WithConsole(console.New(
			console.WithLogs(log),
			console.WithHint(stopHint),
		)),
		WithBinder(attach.New(
			attach.WithTargets(docker.New(), shell.New()),
			// Built here, before a flag has been parsed: no run to name yet.
			attach.WithBanner(frameLine()),
			attach.WithLogs(log),
			attach.WithMotd(board),
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
	noCache   bool

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

	// qr is whether the run prints its address as a QR code on stderr once
	// the tunnel is up. Flag-backed like the rest; off unless asked for.
	qr bool

	// pid is how the run is found and handed back from outside it, for the
	// npm launcher; nil, the default, is neither. See WithPid.
	pid Pid

	// newTunnel is how a spec becomes a tunnel: libtunnel.From, or what a
	// test put there so a run never reaches the edge. See WithTunnelFactory.
	newTunnel func(spec string) libtunnel.TunnelV1

	// The collaborators Command's RunE composes, each behind a contract
	// declared above. Seeded by New; a test or a contributor swaps one with
	// its With* option.
	display  Display
	binder   Binder
	router   Router
	identity Identity
	// cache is the one collaborator allowed to be nil: that is what caching
	// turned off looks like, and --no-cache is how a run asks for it.
	cache Cache

	// stdout carries the help text and version banner, stderr the tunnel's
	// own banner, the origin map, and the tunnel's logs. They are staging
	// only: Command hands them to the command with SetOut/SetErr and
	// everything downstream reads them back through
	// OutOrStdout/ErrOrStderr, so cobra stays the single owner of where
	// output goes. Nil means whatever cobra defaults to.
	stdout, stderr io.Writer

	// log is the run's logging: the one logger, the recent lines a terminal
	// shows, and the run's log file. The binder and the console were handed
	// the same one at construction — see New.
	log Log

	// console is the screen this run was started on, seeded with what
	// outlives a single run — the ring to keep off a drawn frame, and the
	// line to leave on a returned prompt. Run binds it to the origins and
	// streams it actually got, and the browser package decides whether it is
	// what the tunnel gets put in front of.
	console Console

	// motd keeps what the provider said with the spec for every surface.
	motd Motd

	// auth stands between a visitor and everything the tunnel serves.
	auth Auth

	// runCache is the cache the current run reads and writes through: the
	// field's, an in-memory one under --no-cache, or noCache. Set in Command,
	// read by the default auth for the tunnel secret.
	runCache Cache

	// run takes each run's tunnel from its spec to its end.
	run Run

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
