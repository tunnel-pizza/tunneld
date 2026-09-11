// Package v1 is the stable public surface for tunneld. The Builder interface
// here is the contract callers depend on across releases; the implementation
// lives in v1alpha1 and may change between alpha revisions.
//
// The tunneld tier is two packages:
//
//   - github.com/tunnel-pizza/tunneld/v1 (this package) — the Builder
//     contract, the Err* sentinels, and the constants naming every
//     environment knob and default. Declare types with these and match
//     errors against them.
//   - github.com/tunnel-pizza/tunneld/v1alpha1 — the implementation: New,
//     the command assembly, the tunnel it runs, the version resolution.
//
// New lives in v1alpha1 rather than here, so application code constructs from
// there and matches errors here:
//
//	cmd := v1alpha1.New(v1alpha1.WithOrigin("http://localhost:3000")).Command()
//	if err := cmd.ExecuteContext(ctx); err != nil { ... }
//
// A façade package re-exporting New alongside these names would read better,
// and cannot exist: a constructor has to import what it constructs, v1alpha1
// already imports this package for the sentinels, and Go does not allow the
// cycle. Anything that moved the declarations out from under v1alpha1 would
// hit the same wall one layer down, where attach/docker reaches for
// ErrInvalidOrigin.
//
// The builder assembles the `tunneld` command: a New that takes options,
// finalized by Command, which returns a *cobra.Command ready to Execute. That
// shape is what lets tunneld be both a binary and an embeddable subcommand —
// a host program builds the command, renames it, seeds its origins, redirects
// its streams, and hangs it off its own root without reimplementing anything.
//
// Two other things live at this layer because they are equally part of the
// contract: the Err* sentinels callers match with errors.Is, and the constants
// naming every environment knob and default.
package v1

import (
	"context"
	"errors"
	"log/slog"
	"net/url"

	"github.com/spf13/cobra"
)

// Logger is the logger the tunnel and its helpers write to — an alias for
// *slog.Logger, so a caller names it without importing log/slog and a
// *slog.Logger from anywhere satisfies it.
//
// An alias rather than an interface of our own: slog is already the standard
// library's answer, and a narrower interface here would buy nothing except a
// conversion at every call site.
type Logger = *slog.Logger

// Option configures a value of type T while it is being constructed. Every
// New in v1alpha1 and its subpackages takes a list of them, applies its own
// defaults first and the caller's after, so the caller's always wins. A
// package aliases the instantiated type to its own Option and exposes With*
// constructors returning that alias:
//
//	cmd := v1alpha1.New(v1alpha1.WithOrigin("http://localhost:3000")).Command()
//
// One generic type rather than an Option per package, so the rule for how
// options are applied is declared once, in Apply, and every constructor in
// the tree reads the same way. It lives here rather than in v1alpha1 for the
// same reason the sentinels do: the subpackages cannot import the root, and
// every one of them needs it.
type Option[T any] func(T)

// Apply runs opts against t in order and returns t, so a constructor is one
// expression per tier — its defaults, then the caller's. Later options
// overwrite earlier ones, which is the whole precedence rule.
func Apply[T any](t T, opts ...Option[T]) T {
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// The sentinel errors, centralized: declared here with errors.New, wrapped by
// the implementation in v1alpha1, and matched by callers with errors.Is. Two
// disciplines govern this block.
//
// First, an error's doc names the likely cause and the lever — what a caller
// can actually do about it. "Invalid value" tells a caller nothing; naming the
// variable to fix tells them everything.
//
// Second, a sentinel is never deleted. When the condition it reported stops
// existing, the variable stays (marked Deprecated, with the history in its
// doc) so callers matching on it keep compiling. Deleting one turns a
// no-longer-reachable branch in caller code into a build failure — all cost,
// no benefit, since an unreachable errors.Is is simply never true.

// ErrInvalidEnv reports an environment override that is set but unparsable —
// the environment binding in v1alpha1 wraps it. Because env beats code, a typo'd
// value would otherwise fall back to the code value silently and the operator
// would never learn the knob did nothing; this surfaces it instead. The lever
// is the variable named in the wrapped message: correct the value or unset it
// to fall back to the code value deliberately.
var ErrInvalidEnv = errors.New("invalid environment value")

// ErrNoOrigin reports a command built and run with nothing to expose: no
// origin argument, no OriginsEnv, and no WithOrigin seed — or every origin it
// was given dropped as unusable, which arrives at the same place. A tunnel
// with no origin would come up pointing at a loopback socket nobody serves on
// — a public hostname that answers only errors — so this fails before the mint
// instead. The lever is an argument, OriginsEnv, or WithOrigin when
// embedding.
var ErrNoOrigin = errors.New("no origin")

// ErrInvalidOrigin reports a container origin that named something tunneld
// could not serve: a container the daemon has never heard of, one that is not
// running, or one whose inspect comes back with no configuration to read. A
// daemon that cannot be reached at all is ErrNoDocker instead — there the
// origin is fine and Docker is not, and the lever is a different one.
//
// A malformed origin does not earn it. An unparsable URL, a scheme that is
// none of http, https, AttachScheme or ExecScheme, a served origin naming no
// reference, a served origin carrying more than one — each is dropped with a
// warning and the origins that work are exposed anyway; see Builder.Origins. A
// bare host:port is not malformed at all — it implies http.
//
// The wrapped message names the offending value; the lever is to correct it.
var ErrInvalidOrigin = errors.New("invalid origin")

// ErrUnknownIdentity reports an --identity-providers entry, or an
// IdentityProvidersEnv entry bound onto it, that names no provider tunneld
// has.
//
// An error rather than a skip, and reported before the tunnel is minted: a
// misspelled provider that quietly looked for nothing would be
// indistinguishable from a machine with no identity to find, and the run would
// mint anonymously without anybody learning why. The wrapped message names the
// offending entry and the providers that do exist.
var ErrUnknownIdentity = errors.New("unknown identity provider")

// ErrInvalidLogLevel reports a --log-level value, or a LogEnv value bound onto
// it, that is not debug, info, warn or error. It is an error rather than a
// silent fallback because somebody typed it: quietly reading an unknown level
// as info would hide the typo behind logs that look almost right. That is the
// same promise ErrInvalidEnv makes for the other environment knobs — an
// override nobody notices failing is worse than one that stops the process.
var ErrInvalidLogLevel = errors.New("invalid log level")

// ErrNotReady reports a tunnel that never became reachable end to end and
// failed without a cause of its own — the fallback when neither the tunnel's
// own error nor the context's explains the failure. In practice it means the
// edge connection or the hostname resolution gave up quietly; --log-level
// debug is the lever, since the underlying library logs the attempt.
var ErrNotReady = errors.New("tunnel did not become ready")

// ErrNoDocker reports an attach://dockerd/ origin whose Docker daemon could not
// be reached: the socket refused the connection, or the API answered an error
// that is not about this particular container. It is separate from
// ErrInvalidOrigin because the origin may be perfectly well-formed and the
// daemon simply not running, which is by far the likeliest failure of a
// container origin and has a different lever — start Docker, or point
// $DOCKER_HOST at the socket that has it.
var ErrNoDocker = errors.New("docker daemon unreachable")

// ErrTunnelGone reports a tunnel the edge has disowned while it was running:
// the hostname stopped resolving, or a fresh registration was refused
// outright. It is the reaped-tunnel case, and it is terminal — the spec that
// names the tunnel is dead, so reconnecting cannot bring it back and only a
// new mint will.
//
// The engine does not end a tunnel on this by itself; cloudflared retries
// forever either way, and stopping is a decision only the program running it
// can make. tunneld makes it, because a process still holding a public
// hostname that resolves nowhere is serving nobody, and a supervisor that
// restarts it gets a working tunnel back. --cache-dir=false and a restart is
// the whole recovery.
var ErrTunnelGone = errors.New("tunnel gone")

// The environment variables and defaults, centralized: every code knob with an
// env-expressible value has a mirror here, and env beats code — an operator
// reconfigures a deployed binary without a rebuild. Each variable is read
// lazily, where its knob takes effect, rather than once at init, so a value
// set after construction still lands.
//
// Naming: TUNNELD_<KNOB> for a core knob, TUNNELD__<IMPL>_<KNOB> (double
// underscore) for an implementation-scoped one. The doubled separator
// namespaces the implementation, so two implementations can each expose a
// TIMEOUT knob (TUNNELD__FOO_TIMEOUT, TUNNELD__BAR_TIMEOUT) without colliding
// with each other or with a core TUNNELD_TIMEOUT.
//
// tunneld's tunnel engine is github.com/cnuss/libtunnel, which carries its own
// LIBTUNNEL_* environment surface for everything this one does not expose —
// origin TLS, spec replay, edge pinning, the cache directory. Those pass
// straight through; they are documented in that library, not mirrored here.
const (
	// LogEnv names the level (debug|info|warn|error) of the tunnel's logger:
	// set, it writes to stderr at that level; unset, it is silent. It is the
	// mirror of --log-level, which beats it. An unrecognized value is an error
	// (ErrInvalidLogLevel), not a fallback to info.
	//
	// The name predates the flag, which is why it is TUNNELD_LOG rather than
	// the TUNNELD_LOG_LEVEL a mechanical derivation would produce — the
	// binding names it explicitly for that reason.
	LogEnv = "TUNNELD_LOG"

	// OriginsEnv names the local origins to expose — the mirror of the
	// command's positional arguments, which beat it. Several origins are
	// comma-separated, in the same order argv would take them: the first is
	// the default and each later one answers on a bare ?n parameter.
	//
	// Comma is the separator because that is what the tunnel engine uses for
	// its own list-valued variables. It is also the one limitation of this
	// mirror: an origin URL carrying a literal comma has to arrive as an
	// argument, which is parsed for no separator at all.
	OriginsEnv = "TUNNELD_ORIGINS"

	// ProviderEnv names the quick-tunnel provider host to mint against — the
	// mirror of --provider, which beats it. Unset, the provider is
	// DefaultProvider.
	ProviderEnv = "TUNNELD_PROVIDER"

	// CacheDirEnv names the directories tunnel specs are cached in, comma
	// separated and in order — the mirror of --cache-dir, which beats it.
	//
	// An entry strconv.ParseBool reads as a boolean is an instruction rather
	// than a path: true (and an empty entry) means the default location,
	// false turns caching off rather than caching into a directory named
	// "false". Everything else is a path.
	//
	// One false entry disables the whole list, wherever it appears in it, so
	// ".,false,/tmp" caches nowhere. Entries become absolute and repeats
	// collapse, so ".,true,/tmp" is the working directory, the default, and
	// /tmp.
	//
	// Unset, the cache is that default: a per-project directory under the
	// user's cache directory, named for the working directory it belongs to.
	// Not the working directory itself — a spec is credentials, and a
	// repository is the one place they must not be written by default.
	CacheDirEnv = "TUNNELD_CACHE_DIR"

	// MultiviewEnv names whether to serve the multiview panel — the mirror of
	// --multiview, which beats it. Any value strconv.ParseBool accepts works.
	// Turning it off hands the tunnel's bare address back to the default
	// origin; every origin stays reachable on its own index either way.
	MultiviewEnv = "TUNNELD_MULTIVIEW"

	// ShellFallbackEnv names whether a run given no origin anywhere falls back
	// to $SHELL — the mirror of --shell-fallback, which beats it. Any value
	// strconv.ParseBool accepts works.
	//
	// Turning it off restores ErrNoOrigin for a run with nothing to expose,
	// which is what a script wants: a caller that meant to pass an origin and
	// did not should be told so, not handed a public terminal onto the machine
	// it is running on.
	ShellFallbackEnv = "TUNNELD_SHELL_FALLBACK"

	// IdentityProvidersEnv names the identity providers to look for a mint
	// credential with, comma-separated and in order — the mirror of
	// --identity-providers, which beats it. Empty turns the lookup off.
	IdentityProvidersEnv = "TUNNELD_IDENTITY_PROVIDERS"

	// CommandName is the built command's default name, overridable with
	// WithName so an embedding program can mount it under its own verb.
	CommandName = "tunneld"

	// DefaultProvider is the quick-tunnel service tunneld mints against. It is
	// also the underlying library's default; naming it here puts it in --help
	// and makes it overridable with WithProvider rather than only through the
	// engine's environment.
	DefaultProvider = "tunnel.pizza"

	// AttachScheme names a terminal attached to something already running as
	// an origin instead of an HTTP service: an
	// attach://<provider>/<reference> origin serves that terminal on the same
	// public hostname and the same ?n index as any other origin.
	//
	// A served origin is spelled by the verb, and the authority beside it says
	// where the verb happens. attach://dockerd/api attaches to a container's
	// PID 1; the day there is a second way into the same container it is
	// exec://dockerd/api, differing in the word that says what is being done
	// rather than in the word that says to what. DockerProvider is the only
	// authority this scheme answers today.
	AttachScheme = "attach"

	// ExecScheme names something run as an origin instead of an HTTP service:
	// an exec:///path/to/program origin runs that program and exposes its
	// terminal, on the same public hostname and the same ?n index as any
	// other origin.
	//
	// The empty authority is this machine, which is why the path is absolute
	// and why exec:// is the only served scheme with no provider to name. An
	// authority is a provider — exec://<provider>/<reference> is the same verb
	// somewhere else — with one exception below.
	//
	// ExecScheme is also what a bare argument becomes when this machine can
	// run it. A word that names a command on $PATH is rewritten while origins
	// are settled, so `tunneld htop` says what somebody meant rather than
	// becoming http://htop — a hostname that resolves nowhere, minted and
	// published before anyone finds out. The check is the host's own PATH
	// lookup, so the same argument is a program on one machine and a hostname
	// on another; that is the cost of the shorthand, and the reason an
	// explicit scheme always wins.
	//
	// The exception: exec://<word>, an authority with nothing after it, is
	// looked up the same way before it is read as a provider. Nothing can be
	// asked of a provider without a reference anyway, so the reading that can
	// succeed is preferred to the one that cannot. Either way the rewrite
	// carries the resolved path rather than the word — exec:///usr/bin/htop,
	// not exec://htop — because a bare name says which program only on the
	// machine that looked it up.
	ExecScheme = "exec"

	// DockerProvider is the authority that names the Docker daemon in a served
	// origin: the dockerd in attach://dockerd/<container-name-or-id>.
	//
	// The daemon, not the container — the same reading as dockerd's own socket
	// — because the container reference is what follows it. It is a separate
	// word from the scheme because the two are separate axes now: the scheme
	// says what is being done, and this says by whom.
	DockerProvider = "dockerd"
)

// DefaultMultiview is whether the tunnel's own address answers with a panel
// framing every origin. On, because the alternative for someone exposing three
// services is three tabs and no way to see them together — and because with
// several origins the bare address has no better meaning, every origin having
// an index of its own.
const DefaultMultiview = true

// DefaultIdentityProviders is the list a run looks for a mint credential with
// when nothing says otherwise: the providers, comma-separated and in order,
// the first one to find a credential winning.
//
// A string rather than a slice so it can be a constant, and comma-separated
// because that is the shape IdentityProvidersEnv carries — one spelling for
// the default and for the override.
//
// The names here are the providers' own, and v1 cannot import them to check:
// a test in v1alpha1 asserts that what this names is what New registers.
const DefaultIdentityProviders = "github"

// DefaultShellFallback is whether a run with no origin from any source — no
// argument, no ShellFallbackEnv sibling, no WithOrigin seed — exposes $SHELL
// rather than failing with ErrNoOrigin. On, because a bare tunneld having
// something to do is worth more than the refusal it replaces, and a shell is
// the one origin every machine has: it needs no port to be listening and
// tunneld already knows how to serve a program.
//
// It is a knob because it is not always worth more. An embedding program that
// mounts tunneld under its own verb inherits this default, and a user who
// typed that verb meaning to name an origin gets a public terminal onto their
// machine instead of being told they forgot one. That program turns it off
// with WithShellFallback(false); an operator does the same with
// --shell-fallback=false or ShellFallbackEnv.
const DefaultShellFallback = true

// Builder assembles the tunneld command. Obtain one from v1alpha1.New,
// configured by that package's options, and call the terminal Command to
// produce a *cobra.Command.
//
//	cmd := v1alpha1.New(v1alpha1.WithOrigin("http://localhost:3000")).Command()
//	err := cmd.ExecuteContext(ctx)
//
// Every option's value is a default, not a fixed setting: the command's flags
// bind over the same fields, so an argv value wins. Seeding an origin with
// WithOrigin therefore lets the command run with no arguments at all, which is
// what an embedding program wants — a working default the user can still
// override.
//
// The command's context is its shutdown handle. Run it with ExecuteContext and
// cancel that context (a signal, in the binary's case) to tear the tunnel
// down, during startup as well as after it is live.
//
// The interface is only what a caller calls once the builder exists. The
// With* configuration is construction-time and lives on the constructor,
// which is why it is not here: a setter returning this interface would make
// any knob the interface does not name unreachable after it.
type Builder interface {
	// Command assembles the configured command and returns it. It is the
	// terminal step; calling it more than once returns the same command.
	Command() *cobra.Command
	// Origins reports the local origins the command exposes, in order: the
	// first is the default and each later one answers on a bare ?n routing
	// parameter. The layers settle argv > environment > seed — an argument,
	// then OriginsEnv, then whatever WithOrigin seeded — and each replaces
	// the one under it rather than extending it.
	//
	// Argv is read off the built command, so before it has run this reports
	// what the environment or the seed would expose, and from inside the run
	// it reports what did.
	//
	// An origin tunneld cannot expose — an unparsable URL, a scheme that is
	// none of http, https, AttachScheme or ExecScheme, a served origin naming
	// no reference, a served origin carrying more than one — is dropped with a
	// warning on the tunnel's own log rather than failing the run, so this is
	// what the tunnel was given and not what it was asked for. A run left
	// with no origins at all fails with ErrNoOrigin.
	Origins() []*url.URL
	// Name returns the configured command name (CommandName if WithName was
	// never given).
	Name() string
	// Run brings the tunnel up, reports the public URLs, and blocks until ctx
	// is canceled or the tunnel fails. It is the command's body, so executing
	// the command from Command and calling this do the same work; ctx is the
	// shutdown handle either way, and canceling it tears the tunnel down
	// during startup as well as after.
	//
	// This is the door for a program that wants a tunnel and not a CLI: no
	// command to assemble that nobody will see, no ExecuteContext reading an
	// os.Args it was not given, and the configuration stays the options it was
	// built with. A process shell wants the other door — see Command.
	//
	// Called without executing the command, no argv has been parsed, so the
	// origins are what the environment and the seeds settle on; see Origins.
	// The environment is bound either way, so env still beats code.
	//
	// A run with no origins at all fails with ErrNoOrigin. A tunnel that fails
	// on its own returns the cause, and a canceled ctx is a clean stop rather
	// than an error.
	Run(ctx context.Context) error
}
