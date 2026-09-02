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
//	cmd := v1alpha1.New().WithURL("http://localhost:3000").Build()
//	if err := cmd.ExecuteContext(ctx); err != nil { ... }
//
// A façade package re-exporting New alongside these names would read better,
// and cannot exist: a constructor has to import what it constructs, v1alpha1
// already imports this package for the sentinels, and Go does not allow the
// cycle. Anything that moved the declarations out from under v1alpha1 would
// hit the same wall one layer down, where attach/docker reaches for
// ErrInvalidOrigin.
//
// The builder assembles the `tunneld` command: a fluent chain of With* setters
// finalized by Build, which returns a *cobra.Command ready to Execute. That
// shape is what lets tunneld be both a binary and an embeddable subcommand —
// a host program builds the command, renames it, seeds its origins, redirects
// its streams, and hangs it off its own root without reimplementing anything.
//
// Two other things live at this layer because they are equally part of the
// contract: the Err* sentinels callers match with errors.Is, and the constants
// naming every environment knob and default.
package v1

import (
	"errors"
	"io"
	"log/slog"

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
// see EnvBool and EnvDuration in v1alpha1. Because env beats code, a typo'd
// value would otherwise fall back to the code value silently and the operator
// would never learn the knob did nothing; this surfaces it instead. The lever
// is the variable named in the wrapped message: correct the value or unset it
// to fall back to the code value deliberately.
var ErrInvalidEnv = errors.New("invalid environment value")

// ErrNoOrigin reports a command built and run with nothing to expose: no
// --url flag and no WithURL seed. A tunnel with no origin would come up
// pointing at a loopback socket nobody serves on — a public hostname that
// answers only errors — so this fails before the mint instead. The lever is
// --url (repeatable), or WithURL when embedding.
var ErrNoOrigin = errors.New("no origin")

// ErrInvalidOrigin reports a --url value tunneld cannot expose: an unparsable
// URL, a scheme that is none of http, https or DockerScheme, or a URL with no
// host. A bare host:port is not an error — it implies http.
//
// A dockerd:// value earns it for reasons of its own, all of them about the
// reference rather than the syntax: anything beyond a container name (a path,
// a query, a fragment, credentials), a container the daemon has never heard
// of, one that is not running, and one whose inspect comes back with no
// configuration to read. A daemon that cannot be reached at all is ErrNoDocker
// instead — there the origin is fine and Docker is not, and the lever is a
// different one.
//
// The wrapped message names the offending value; the lever is to correct it.
var ErrInvalidOrigin = errors.New("invalid origin")

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

// ErrNoDocker reports a dockerd:// origin whose Docker daemon could not be
// reached: the socket refused the connection, or the API answered an error
// that is not about this particular container. It is separate from
// ErrInvalidOrigin because the origin may be perfectly well-formed and the
// daemon simply not running, which is by far the likeliest failure of a
// container origin and has a different lever — start Docker, or point
// $DOCKER_HOST at the socket that has it.
var ErrNoDocker = errors.New("docker daemon unreachable")

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

	// URLEnv names the local origins to expose — the mirror of --url, which
	// beats it. Several origins are comma-separated, in the same order the
	// repeated flag would take them: the first is the default and each later
	// one answers on a bare ?n parameter.
	//
	// Comma is the separator because that is what the tunnel engine uses for
	// its own list-valued variables. It is also the one limitation of this
	// mirror: an origin URL carrying a literal comma has to arrive through the
	// flag, which parses no separator at all.
	URLEnv = "TUNNELD_URL"

	// ProviderEnv names the quick-tunnel provider host to mint against — the
	// mirror of --provider, which beats it. Unset, the provider is
	// DefaultProvider.
	ProviderEnv = "TUNNELD_PROVIDER"

	// CacheDirEnv names the directories tunnel specs are cached in, comma
	// separated and in order — the mirror of --cache-dir, which beats it.
	//
	// An entry strconv.ParseBool reads as a boolean is an instruction rather
	// than a path: true (and an empty entry) means the working directory,
	// false turns caching off rather than caching into a directory named
	// "false". Everything else is a path.
	//
	// One false entry disables the whole list, wherever it appears in it, so
	// ".,false,/tmp" caches nowhere. Entries become absolute and repeats collapse, so ".,true,/tmp"
	// is the working directory and then /tmp.
	//
	// Unset, the cache is the working directory — the same thing setting it
	// to true says explicitly.
	CacheDirEnv = "TUNNELD_CACHE_DIR"

	// NoOpenEnv names whether to leave the browser alone once the tunnel is
	// live — the mirror of --no-open, which beats it. Any value
	// strconv.ParseBool accepts works; anything else is an error. Set it to
	// true on a server or in CI, where there is no browser to open.
	//
	// It is spelled as the negative because that is the only thing anyone ever
	// asks for: opening is the default, so the flag exists to be turned on,
	// and `--no-open` says on the command line what `--open=false` needed an
	// argument to say.
	NoOpenEnv = "TUNNELD_NO_OPEN"

	// MultiviewEnv names whether to serve the multiview panel — the mirror of
	// --multiview, which beats it. Any value strconv.ParseBool accepts works.
	// Turning it off hands the tunnel's bare address back to the default
	// origin; every origin stays reachable on its own index either way.
	MultiviewEnv = "TUNNELD_MULTIVIEW"

	// CommandName is the built command's default name, overridable with
	// WithName so an embedding program can mount it under its own verb.
	CommandName = "tunneld"

	// DefaultProvider is the quick-tunnel service tunneld mints against. It is
	// also the underlying library's default; naming it here puts it in --help
	// and makes it overridable with WithProvider rather than only through the
	// engine's environment.
	DefaultProvider = "tunnel.pizza"

	// DockerScheme names a running container as an origin instead of an HTTP
	// service: --url dockerd://<container-name-or-id> serves a terminal
	// attached to that container, on the same public hostname and the same
	// ?n index as any other origin.
	//
	// The daemon, not the container, is what the scheme names — the same
	// reading as dockerd's own socket — because the container reference is
	// the authority component that follows it.
	DockerScheme = "dockerd"
)

// DefaultOpen is whether a tunnel opens its public URL in a browser once it is
// live. On, because the overwhelmingly common case is a developer exposing
// something they are about to look at; the lever for every other case is
// --no-open or NoOpenEnv.
//
// The Go knob stays positive where the command line reads negative: a caller
// writing WithOpen(false) is being explicit in a way a bare flag cannot be,
// and WithNoOpen(false) would be a double negative to mean "open".
const DefaultOpen = true

// DefaultMultiview is whether the tunnel's own address answers with a panel
// framing every origin. On, because the alternative for someone exposing three
// services is three tabs and no way to see them together — and because with
// several origins the bare address has no better meaning, every origin having
// an index of its own.
const DefaultMultiview = true

// Builder assembles the tunneld command. Configure it with the With* methods
// (each returns the Builder for chaining), then call the terminal Build to
// produce a *cobra.Command. Obtain one from v1alpha1.New.
//
//	cmd := v1alpha1.New().WithURL("http://localhost:3000").Build()
//	err := cmd.ExecuteContext(ctx)
//
// Every With* value is a default, not a fixed setting: the command's flags
// bind over the same fields, so an argv value wins. Seeding an origin with
// WithURL therefore makes --url optional rather than forbidden, which is what
// an embedding program wants — a working default the user can still override.
//
// The command's context is its shutdown handle. Run it with ExecuteContext and
// cancel that context (a signal, in the binary's case) to tear the tunnel
// down, during startup as well as after it is live.
type Builder interface {
	// WithName sets the built command's name — the verb in usage strings and
	// what cobra matches when the command is mounted under another root.
	// Unset, the name is CommandName.
	WithName(name string) Builder
	// WithURL seeds the local origins to expose, in order: the first is the
	// default origin and each later one answers on a bare ?n parameter.
	// Repeated calls append. A --url flag on the command line replaces the
	// whole seeded set rather than adding to it.
	//
	// A missing scheme implies http and a missing host implies localhost, so
	// ":8000", "localhost:8000" and "http://localhost:8000" name one origin.
	WithURL(urls ...string) Builder
	// WithProvider sets the quick-tunnel provider host to mint against.
	// Unset, the provider is DefaultProvider.
	WithProvider(host string) Builder
	// WithCacheDir sets the directories tunnel specs are cached in, in
	// order, appending across calls. A boolean entry is an instruction
	// rather than a path: true (and an empty entry) names the working
	// directory, and one false disables the whole list wherever it appears
	// in it. Entries become absolute and repeats
	// collapse. Unset, they come from CacheDirEnv, and from the working
	// directory if that is unset too.
	WithCacheDir(dirs ...string) Builder
	// WithLogLevel sets the tunnel's log level (debug|info|warn|error) on
	// stderr. Unset, the level comes from LogEnv, and silence if that is
	// unset too.
	WithLogLevel(level string) Builder
	// WithOpen sets whether a public URL is opened in a browser once the
	// tunnel is live — the multiview panel when there is one, otherwise the
	// default origin. Exactly one page is opened either way, since a fan of
	// tabs is rarely what anyone wanted. Unset, the behaviour is DefaultOpen.
	WithOpen(open bool) Builder
	// WithMultiview sets whether the tunnel's own address answers with a panel
	// framing every origin. It does nothing with a single origin, which has
	// nothing to sit beside and keeps the bare address for itself. Unset, the
	// behaviour is DefaultMultiview.
	WithMultiview(multiview bool) Builder
	// WithStdout redirects the help text and the version banner. Build
	// passes it to the command's SetOut, so calling SetOut on the built
	// command overrides this. Unset, output goes to the process's stdout.
	//
	// A running tunnel writes nothing there: its addresses go to stderr with
	// the rest of what a person reads.
	WithStdout(w io.Writer) Builder
	// WithStderr redirects the banner, the origin map, and the tunnel's logs.
	// Build passes it to the command's SetErr, so calling SetErr on the built
	// command overrides this. Unset, output goes to the process's stderr.
	WithStderr(w io.Writer) Builder
	// Build assembles the configured command and returns it. It is the
	// terminal step; calling it more than once returns the same command.
	Build() *cobra.Command
	// Name returns the configured command name (CommandName if WithName was
	// never called).
	Name() string
}
