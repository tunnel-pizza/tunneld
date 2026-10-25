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
	"fmt"
	"log/slog"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/tunnel-pizza/tunneld/v1/origin"
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

// ErrRunning reports a run refused because the same run — the same working
// directory, origins and arguments — is already going on this machine, in the
// background or another terminal. Two of them would answer on one hostname
// and share one registration, so the second is refused before it mints
// anything; the message names the first one's pid.
var ErrRunning = errors.New("already running")

// ExitError ends a run that its programs ended: every program it started has
// exited, and Code is the status the process exits with, the first non-zero
// one among theirs. A run whose programs all exited 0 ends with no error.
type ExitError struct{ Code int }

func (e ExitError) Error() string {
	return fmt.Sprintf("a program the run started exited with status %d", e.Code)
}

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

	// NoCacheEnv turns the spec cache off — the mirror of --no-cache, which
	// beats it. Any value strconv.ParseBool reads as true turns it off; unset
	// or false leaves it on.
	//
	// On, a run files its spec under the user's cache directory, named for the
	// working directory it was started in and the origins it serves, and the
	// next run of the same thing resumes that hostname. Off, every run mints a
	// fresh one, which is what somebody wants when a hostname must not be
	// reused.
	//
	// Where the file goes is not configurable from the environment: a spec is
	// credentials, and the directory that holds them is the machine's own
	// answer rather than a run's. An embedding program that needs another one
	// passes v1alpha1.WithCacheDir.
	NoCacheEnv = "TUNNELD_NO_CACHE"

	// MultiviewEnv names whether to serve the multiview panel — the mirror of
	// --multiview, which beats it. Any value strconv.ParseBool accepts works.
	// Turning it off hands the tunnel's bare address back to the default
	// origin; every origin stays reachable on its own index either way.
	MultiviewEnv = "TUNNELD_MULTIVIEW"

	// QREnv names whether a run prints its address as a QR code on stderr
	// once the tunnel is up — the mirror of --qr, which beats it. Any value
	// strconv.ParseBool accepts works. stdout is untouched either way: it
	// carries the addresses and nothing else.
	QREnv = "TUNNELD_QR"

	// MCPEnv names whether a run serves MCP on its stdin and stdout — the
	// mirror of --mcp, which beats it. Any value strconv.ParseBool accepts
	// works. On, stdout carries JSON-RPC and nothing else, so what a run
	// would print there goes to stderr.
	MCPEnv = "TUNNELD_MCP"

	// PreflightEnv names whether a run checks the network paths a tunnel
	// depends on and exits, minting nothing — the mirror of --preflight,
	// which beats it. Any value strconv.ParseBool accepts works.
	PreflightEnv = "TUNNELD_PREFLIGHT"

	// IdentityProvidersEnv names the identity providers to look for a mint
	// credential with, comma-separated and in order — the mirror of
	// --identity-providers, which beats it. Empty turns the lookup off.
	IdentityProvidersEnv = "TUNNELD_IDENTITY_PROVIDERS"

	// NotifyPidEnv is how the npm launcher asks a detached run to say when it
	// is up. It carries the launcher's pid, and a run whose parent has that
	// pid sends it SIGUSR2 once its addresses are out, having first moved its
	// stdout and stderr to a log beside its spec, so the launcher can exit and
	// hand the console back. A value naming any other process is ignored:
	// SIGUSR2 ends a process that has not asked for it, and whatever started a
	// run is not always a launcher.
	//
	// Not a knob. Nothing mirrors it and nobody sets it by hand; it is the one
	// line of protocol between the launcher and the binary it ships with.
	NotifyPidEnv = "TUNNELD_NOTIFY_PID"

	// WWWAuthenticateEnv is a tunnel's password protection: a WWW-Authenticate
	// challenge whose private pw parameter is the password's PBKDF2 hash,
	//
	//	Basic realm="0t8qsb6pq3.tunneld.domain", pw="$pbkdf2-sha256$i=600000$…$…"
	//
	// and empty for public. Environment only, deliberately: there is no flag,
	// since a command line is readable by every user through ps and lands in
	// shell history, and set-and-empty is a choice (public), not an absence.
	// A PATCH to /_tunneld/.env can change it on a running tunnel.
	WWWAuthenticateEnv = "TUNNELD_WWW_AUTHENTICATE"

	// OwnerEnv is the line the cache file says the tunnel's owner on, as the
	// latest mint answered it (OwnerHeader), absent when it named nobody.
	// Written by the run for whoever opens the file, never read: not from
	// the file, not from the environment.
	OwnerEnv = "TUNNELD_OWNER"

	// SpecPathEnv is where the run's spec file is, in the environment of
	// every program it starts: known before the tunnel is, so a program can
	// read the file once the run has written it, for its hostname among the
	// rest. Unset when the run keeps no file. Written for the run's programs,
	// never read.
	SpecPathEnv = "TUNNELD_ENV"

	// HTTPOriginEnv is what every program a run starts is told the tunnel's
	// origin by: https://<hostname>, the address its first origin answers
	// on. A cached run knows it before it starts its programs; an uncached
	// one starts them once the tunnel is up, so a program never starts
	// without it. Written, never read: one inherited from the environment a
	// run was started in is not passed on as this run's.
	HTTPOriginEnv = "HTTP_ORIGIN"

	// UserAgentHeader names which tunneld is asking, on every mint request.
	UserAgentHeader = "User-Agent"

	// AuthenticateHeader carries a tunnel's challenges in their public form,
	// comma-joined in one value: on every answer under /_tunneld/, ping and
	// a grant's PATCH among them, absent when the tunnel is public; and on
	// every mint request, empty when it is public.
	AuthenticateHeader = "X-Tunneld-Authenticate"

	// ControlPath is the prefix tunneld keeps for itself on the tunnel
	// hostname; every other path is the origins'. See router.ControlPath.
	ControlPath = "/_tunneld/"

	// GrantHeader carries a single-use grant to PATCH /_tunneld/.env, on every
	// answer to GET /_tunneld/.env made with the tunnel secret.
	GrantHeader = "X-Tunneld-Grant"

	// OriginsHeader is what a tunnel serves, on every mint request: each
	// origin in order, as scheme, host and path only, comma-joined in one
	// value (RFC 9110 §5.6.1). Credentials, a query and a fragment never
	// leave the machine. A provider shows them to the tunnel's owner and the
	// agents the owner approves; n in ?n is an origin's place in the list.
	OriginsHeader = "X-Tunneld-Origins"

	// HostHeader is the machine's hostname, on every mint request: the
	// HOSTNAME a run saves, so a provider can say which machine localhost in
	// an origin is. Empty when the system has no name to give.
	HostHeader = "X-Tunneld-Host"

	// AgentHeader is the agent host that started the run, on every mint
	// request: AI_AGENT as the run found it, so a provider can say which
	// agent a tunnel is serving. Empty when no agent says it started the run.
	AgentHeader = "X-Tunneld-Agent"

	// OwnerHeader is who the provider says owns a tunnel, on its mint answer,
	// in the form a provider access token's sub takes ("github:<id>"):
	// absent when the mint named nobody. The owner's token opens the MCP
	// server on any tunnel, and the owner counts as listed on one set to
	// Single Sign-On.
	OwnerHeader = "X-Tunneld-Owner"

	// CommandName is the built command's default name, overridable with
	// WithName so an embedding program can mount it under its own verb.
	CommandName = "tunneld"

	// DefaultProvider is the quick-tunnel service tunneld mints against. It is
	// also the underlying library's default; naming it here puts it in --help
	// and makes it overridable with WithProvider rather than only through the
	// engine's environment.
	DefaultProvider = "tunnel.pizza"

	// The origin package's constants, here so a caller of this package names
	// them without a second import.
	AttachScheme   = origin.AttachScheme
	ExecScheme     = origin.ExecScheme
	HttpScheme     = origin.HttpScheme
	ArgKey         = origin.ArgKey
	DockerProvider = origin.DockerProvider
)

// DefaultMultiview is whether the tunnel's own address answers with a panel
// framing every origin. On, because the alternative for someone exposing three
// services is three tabs and no way to see them together — and because with
// several origins the bare address has no better meaning, every origin having
// an index of its own.
const DefaultMultiview = true

// DefaultIdentityProviders is the list a run looks for a mint credential with
// when nothing says otherwise: the providers, comma-separated and in order.
// Every one is asked at once, and the first in the list to find a credential
// wins — the order is a preference, not a queue.
//
// github first, being the identity a developer's machine is most likely to
// have; anthropic after, the credential a Claude Code workspace keeps, which
// anywhere else finds nothing without a wait.
//
// A string rather than a slice so it can be a constant, and comma-separated
// because that is the shape IdentityProvidersEnv carries — one spelling for
// the default and for the override.
//
// The names here are the providers' own, and v1 cannot import them to check:
// a test in v1alpha1 asserts that what this names is what New registers.
const DefaultIdentityProviders = "github,anthropic"

// Origins is the local origins a run exposes, in order: the first is the
// default and each later one answers on a bare ?n routing parameter.
//
// A type rather than a []*url.URL, because the list answers questions no
// slice can: Key is what identifies this run's tunnel, and it is the name its
// cached spec is filed under; WebSocket is the origin marked +ws; Dialable is
// where each origin is dialed, a served one's address kept beside the origin
// rather than in a second list. Len, At and All are the slice's own
// vocabulary, so a caller reads the list the way it would read a slice.
type Origins interface {
	// Len is how many origins this run exposes.
	Len() int
	// At is the origin at i, which the caller has already bounded by Len.
	At(i int) Origin
	// All is every origin, in order, as a copy: sorting, reordering or
	// changing what comes back cannot change the run.
	All() []Origin
	// From is the origin an address on the tunnel reaches, routed as a
	// visit is: by the first bare numeric parameter, none meaning the first
	// (https://<host>/?1 is origin 1). An address that names no origin is
	// ErrInvalidOrigin. The host is not checked: what is asked for here has
	// already reached this tunnel.
	From(url string) (Origin, error)
	// Key identifies the tunnel these origins are: the working directory they
	// were settled in and the origins themselves, sorted so the order they
	// were typed does not make a second tunnel, deduplicated so a repeat does
	// not either. Stable across runs, and safe as a filename.
	Key() string
	// WebSocket is the index of the origin a WebSocket handshake goes to when
	// nothing in it routes, the one an operator marked +ws, and false when
	// none is marked.
	WebSocket() (int, bool)
	// Dialable is the address the tunnel dials for each origin, in order:
	// the loopback address tunneld serves it at, for a program or a
	// container, else its URI. Each a copy.
	Dialable() []*url.URL
}

// Origin, Scheme and Provider are the origin package's, aliased so a caller
// of this package names them without a second import.
type (
	Origin   = origin.Origin
	Scheme   = origin.Scheme
	Provider = origin.Provider
)

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
	// what the tunnel was given and not what it was asked for.
	//
	// The list is a value with an identity: Key names the tunnel these origins
	// are, which is what the spec cache files it under.
	Origins() Origins
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
	// A tunnel that fails on its own returns the cause, and a canceled ctx is
	// a clean stop rather than an error.
	Run(ctx context.Context) error
}
