package v1alpha1

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/cnuss/libtunnel"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell"
	"github.com/tunnel-pizza/tunneld/v1alpha1/console"
	"github.com/tunnel-pizza/tunneld/v1alpha1/display"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
)

// WithName sets the built command's name — the verb in usage strings and
// what cobra matches when the command is mounted under another root. Unset,
// the name is v1.CommandName.
func WithName(name string) Option {
	return func(b *BuilderImpl) { b.name = name }
}

// WithOrigin seeds the local origins to expose, in order: the first is the
// default origin and each later one answers on a bare ?n parameter. Repeated
// options append. An origin argument on the command line replaces the whole
// seeded set rather than adding to it.
//
// A missing scheme implies http and a missing host implies localhost, so
// ":8000", "localhost:8000" and "http://localhost:8000" name one origin.
func WithOrigin(origins ...string) Option {
	return func(b *BuilderImpl) { b.origins = append(b.origins, origins...) }
}

// WithProvider sets the quick-tunnel provider host to mint against. Unset,
// the provider is v1.DefaultProvider.
func WithProvider(host string) Option {
	return func(b *BuilderImpl) { b.provider = host }
}

// WithLogLevel sets the tunnel's log level (debug|info|warn|error) on
// stderr. Unset, the level comes from v1.LogEnv, and silence if that is
// unset too.
func WithLogLevel(level string) Option {
	return func(b *BuilderImpl) { b.logLevel = level }
}

// WithIdentityProviders seeds the providers a run looks for a mint credential
// with, in order. The flag and v1.IdentityProvidersEnv both beat it.
//
// An empty list turns the lookup off, and a run that finds no credential mints
// anonymously — which is what every run did before there was anything to find.
func WithIdentityProviders(names ...string) Option {
	return func(b *BuilderImpl) { b.identityProviders = names }
}

// WithOpen settles whether a public URL is opened in a browser once the tunnel
// is live — the multiview panel when there is one, otherwise the default
// origin. Exactly one page is opened either way, since a fan of tabs is rarely
// what anyone wanted.
//
// Unset, and that is the ordinary case, the run works it out: see opening. The
// signals it reads are the ones a person would — a terminal to have been
// started from, a display to open on, a console already drawing the terminal —
// and they are right often enough that there is no flag for this and no
// environment variable either.
//
// This is the lever for the run they are wrong about, and for a program with
// an opinion of its own: a service embedding tunneld knows nobody is watching,
// however interactive its own streams look.
func WithOpen(open bool) Option {
	return func(b *BuilderImpl) { b.open = &open }
}

// WithMultiview sets whether the tunnel's own address answers with a panel
// framing every origin. It does nothing with a single origin, which has
// nothing to sit beside and keeps the bare address for itself. Unset, the
// behaviour is v1.DefaultMultiview.
func WithMultiview(multiview bool) Option {
	return func(b *BuilderImpl) { b.multiview = multiview }
}

// WithShellFallback sets whether a run with no origin from any source — no
// argument, no TUNNELD_ORIGINS, no WithOrigin seed — exposes $SHELL rather
// than failing with v1.ErrNoOrigin. Unset, the behaviour is
// v1.DefaultShellFallback.
//
// An embedding program is the caller this exists for. It inherits the default
// along with everything else, and a user who typed its verb meaning to name an
// origin would get a public terminal onto the machine instead of being told
// they forgot one. WithShellFallback(false) restores the refusal.
//
// It is also how a test says "nothing to expose" and means it, without
// reaching into the process environment to unset a variable the run reads
// behind its back.
func WithShellFallback(fallback bool) Option {
	return func(b *BuilderImpl) { b.shellFallback = fallback }
}

// WithStdout redirects the help text and the version banner. Command passes
// it to the command's SetOut, so calling SetOut on the built command
// overrides this. Unset, output goes to the process's stdout.
//
// A running tunnel writes nothing there: its addresses go to stderr with the
// rest of what a person reads.
func WithStdout(w io.Writer) Option {
	return func(b *BuilderImpl) { b.stdout = w }
}

// WithStderr redirects the banner, the origin map, and the tunnel's logs.
// Command passes it to the command's SetErr, so calling SetErr on the built
// command overrides this. Unset, output goes to the process's stderr.
func WithStderr(w io.Writer) Option {
	return func(b *BuilderImpl) { b.stderr = w }
}

// Name returns the configured command name, defaulting to v1.CommandName.
func (b *BuilderImpl) Name() string {
	if b.name == "" {
		return v1.CommandName
	}
	return b.name
}

// flagEnv is the flag → environment variable registry: every flag with an
// env-expressible value, bound to the constant naming it in v1. Explicit
// rather than derived — viper's AutomaticEnv would mangle a name out of each
// flag, which puts the authority over the operator-facing strings in a key
// replacer instead of in v1, where the rest of this package's knobs are
// declared. It also keeps LogEnv spelled TUNNELD_LOG rather than the
// TUNNELD_LOG_LEVEL a derivation would produce.
// originsKey is what the origins variable binds to in the per-builder
// environment. It is not a flag name — origins have no flag — so it is spelled
// here rather than in flagEnv, which exists to pair flags with variables.
const originsKey = "origins"

// servedSchemes is every scheme whose value names something tunneld serves on
// the origin's behalf rather than an address it proxies to. The binder stands
// a loopback attach server up for each one, and the provider behind that
// scheme is what resolves the reference.
//
// The words are for the operator: a message that says "names no container" for
// attach:// and "names no program" for exec:// is one somebody can act on,
// where a message about a malformed authority component is not. A scheme
// absent from here is not served, which is what makes the parser and the
// binder agree on the same list.
var servedSchemes = map[string]struct {
	noun, example string
	// resolve, when set, is how an authority with nothing after it is read
	// before it is read as a provider: exec://htop is the program, because a
	// provider with no reference could not be asked for anything anyway, so
	// the reading that can succeed wins over the one that cannot.
	//
	// It is the same function the bare-argument shorthand uses, held here
	// rather than called by name below so the two readings of a word cannot
	// drift — whatever `tunneld htop` accepts, exec://htop accepts.
	resolve func(string) (string, bool)
}{
	v1.AttachScheme: {noun: "container", example: "attach://dockerd/my-container"},
	v1.ExecScheme:   {noun: "program", example: "exec:///usr/bin/htop", resolve: shell.Resolve},
}

// splitList parses a list-valued variable: comma-separated, surrounding space
// trimmed, empty entries dropped so a trailing comma is not an origin. Comma
// is the separator the tunnel engine uses for its own list variables, and the
// reason an origin carrying one has to arrive as an argument instead.
func splitList(value string) []string {
	items := make([]string, 0, strings.Count(value, ",")+1)
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

var flagEnv = map[string]string{
	"provider":  v1.ProviderEnv,
	"no-cache":  v1.NoCacheEnv,
	"log-level": v1.LogEnv,
	"multiview": v1.MultiviewEnv,

	"shell-fallback": v1.ShellFallbackEnv,

	"identity-providers": v1.IdentityProvidersEnv,
}

// Command assembles the configured command. It is the terminal step; the
// command is built once and cached, so repeated calls return the same
// *cobra.Command rather than a second one with a second set of flags bound to
// these fields.
func (b *BuilderImpl) Command() *cobra.Command {
	b.commandOnce.Do(func() {
		name := b.Name()

		// Report the first collaborator New would have seeded and did not: a
		// BuilderImpl assembled as a bare struct rather than through New.
		// One check, here at the first place Command needs every
		// collaborator rather than inside RunE, so a bare struct is refused
		// before any flag is bound to a field nobody seeded.
		//
		// The cache is not among them: nil is a legal state there, and the
		// one --no-cache leaves a run in.
		//
		// A missing collaborator short-circuits with a minimal command whose
		// RunE returns the error and nothing else — no flag binding, no env
		// binding, so nothing downstream ever touches the nil field either.
		for _, c := range []struct {
			name    string
			missing bool
		}{
			{"engine", b.engine == nil},
			{"browser", b.display == nil},
			{"counter", b.counter == nil},
			{"binder", b.binder == nil},
		} {
			if c.missing {
				err := fmt.Errorf("builder has no %s: construct it with New", c.name)
				b.command = &cobra.Command{
					Use:           name,
					SilenceUsage:  true,
					SilenceErrors: true,
					RunE: func(*cobra.Command, []string) error {
						return err
					},
				}
				return
			}
		}

		// An embedder that seeded origins made them this command's default, so
		// help says which — otherwise the one thing a seeded build does
		// differently from a bare one is the one thing --help does not
		// mention. Origins have no flag any more, and a flag's default is
		// where this used to be visible.
		var seeded string
		if len(b.origins) > 0 {
			seeded = "\n\nWith no origin arguments, this command exposes: " + strings.Join(b.origins, ", ")
		}

		cmd := &cobra.Command{
			Use:   name + " [origin ...]",
			Short: "Expose local origins to the public internet through a quick tunnel",
			Long: name + ` exposes already-running local services to the public internet
through an in-process quick tunnel — no cloudflared binary, no account, no DNS.

Pass an origin per argument. They share one hostname: the first is the default,
each later one answers on a bare ?n parameter (n is that argument's position).

  ` + name + ` http://localhost:3000 http://localhost:4000

    https://<host>/?0   -> http://localhost:3000
    https://<host>/?1   -> http://localhost:4000

An origin can also be a running container, which is served as a terminal in
the browser rather than proxied:

  ` + name + ` attach://dockerd/my-container

Mark one origin http+ws (or https+ws) when a service opens its own WebSocket —
a dev server's live reload, say. A handshake carries nothing that says which
origin it belongs to, so without the marker it goes to the first one:

  ` + name + ` :4000 http+ws://localhost:5173

With no arguments at all — and nothing in the environment or seeded by an
embedding program — it exposes this machine's own shell, $SHELL, the same way:

  ` + name + `

The public URLs go to stdout, the origin map and every log line to stderr.` + seeded,
			// Origins are the arguments, so any number is accepted here and
			// the count is judged in RunE, where a value from the environment
			// or a seed counts as well as one from argv.
			Args:          cobra.ArbitraryArgs,
			SilenceUsage:  true, // usage answers a flag error, not a tunnel failure
			SilenceErrors: true, // the caller prints the error, prefixed, exactly once
			// Persistent, so it also covers a subcommand — and placed here rather
			// than in RunE because cobra runs this hook ahead of required-flag
			// validation, which is what lets a variable satisfy a required flag.
			// Ahead of cobra's required-flag validation, so a flag satisfied
			// by its variable counts as supplied. Run calls the same thing,
			// for a caller who never executes the command.
			PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return b.applyEnv(cmd) },
			// RunE is the built command's body, and it is one line: everything it
			// does is Run, which a caller with no command reaches directly. The
			// context is cobra's, set by ExecuteContext, and it is the shutdown
			// handle either way.
			RunE: func(cmd *cobra.Command, _ []string) error { return b.Run(cmd.Context()) },
		}

		// Hand the configured writers to cobra rather than keeping a second
		// mechanism beside its own: SetOut/SetErr is where a *cobra.Command
		// records this, and OutOrStdout/ErrOrStderr then answer for the whole
		// command — help, usage, and the version banner as well as the URLs. A
		// caller that would rather set them on the built command still can, and
		// wins, being the later and more specific call.
		if b.stdout != nil {
			cmd.SetOut(b.stdout)
		}
		if b.stderr != nil {
			cmd.SetErr(b.stderr)
		}

		// Each flag binds over the field it defaults from, so a seeded value is a
		// default and an argv value overwrites it.
		// Each usage string names the flag's environment mirror, so --help doubles
		// as the reference for configuring a container. The registry behind those
		// names is flagEnv, declared above Command.
		// Negative, where the option behind it is positive: a command line says
		// what is unusual about this run, and an embedder configuring a
		// default should not have to read a negation twice.
		cmd.Flags().BoolVar(&b.noCache, "no-cache", b.noCache,
			"don't cache the tunnel spec: mint a fresh hostname every run [$"+v1.NoCacheEnv+"]")
		cmd.Flags().StringVar(&b.provider, "provider", cmp.Or(b.provider, v1.DefaultProvider),
			"quick-tunnel provider host to mint against [$"+v1.ProviderEnv+"]")
		// StringSlice rather than StringArray: applyEnv hands a variable's
		// value to pflag.SliceValue.Replace, and only the slice type splits
		// the comma-separated list the mirror carries.
		cmd.Flags().StringSliceVar(&b.identityProviders, "identity-providers", b.identityProviders,
			"identity providers to find a mint credential with, in order; empty sends none [$"+v1.IdentityProvidersEnv+", comma-separated]")
		cmd.Flags().StringVar(&b.logLevel, "log-level", b.logLevel,
			"tunnel log level on stderr: debug, info, warn, error (default: silent) [$"+v1.LogEnv+"]")
		cmd.Flags().BoolVar(&b.multiview, "multiview", b.multiview,
			"answer the tunnel's own URL with a panel framing every origin [$"+v1.MultiviewEnv+"]")
		cmd.Flags().BoolVar(&b.shellFallback, "shell-fallback", b.shellFallback,
			"with no origin given anywhere, expose $SHELL rather than refusing to start [$"+v1.ShellFallbackEnv+"]")
		// The version subcommand prints the build banner and exits — the
		// build id of the binary plus the tunnel library it links against,
		// since that library is what actually speaks to the edge and a bug
		// report needs both numbers.
		//
		// The banner is written to OutOrStdout explicitly. cmd.Print and
		// friends route through OutOrStderr, which falls back to os.Stderr,
		// and a version a script cannot read off stdout is a version nobody
		// can pipe.
		cmd.AddCommand(&cobra.Command{
			Use:   "version",
			Short: "Print the " + name + " build identifier and exit",
			Args:  cobra.NoArgs,
			Run: func(cmd *cobra.Command, _ []string) {
				// Against the root's flags rather than this subcommand's:
				// applyEnv binds a variable onto the flag that mirrors it, and
				// the flags a run is configured by live on the parent. Without
				// this the banner would report a cache key for a run that
				// TUNNELD_NO_CACHE has already turned off. A bad value is the
				// run's error to report, not this one's — printing a version
				// is not the place to refuse.
				_ = b.applyEnv(b.command)
				fmt.Fprintln(cmd.OutOrStdout(), VersionLine(b.cached()))
			},
		})

		b.command = cmd
	})
	return b.command
}

// Run brings the tunnel up, reports the public URLs, and blocks until ctx is
// canceled or the tunnel fails. ctx is the shutdown handle — canceling it (a
// signal, in the binary's case) tears the tunnel down during startup as well
// as after, so this returns rather than hanging. A tunnel that fails on its
// own returns the cause.
//
// This is the command's body, and calling it is the other way in. A program
// that wants a tunnel and not a CLI configures the builder with options and
// calls this; it never assembles a command it will not show anybody, and
// never risks ExecuteContext parsing an os.Args it was not given. The binary
// goes the long way round, through Command and ExecuteContext, because a
// process shell is what cobra is for.
//
// The command is still where the streams and the flag values live, so this
// assembles it — Command is cached, so from inside RunE that is the same
// command already running. Reached directly, no argv has been parsed, so the
// origins are whatever the environment and the seeds settle on; reached
// through RunE, arguments have replaced them. Either way the environment is
// bound first: PersistentPreRunE does it for a command being executed, and
// applyEnv is idempotent, so doing it again here costs nothing and a direct
// caller is not the one path where env stops beating code.
//
// The engine is github.com/cnuss/libtunnel driving Cloudflare's edge in
// process — no cloudflared binary, no account, no DNS to configure.
func (b *BuilderImpl) Run(ctx context.Context) error {
	cmd := b.Command()
	if err := b.applyEnv(cmd); err != nil {
		return err
	}
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	// The logger: resolve the tunnel's log sink from the level the
	// command settled on — the --log-level flag, or v1.LogEnv bound
	// onto it by PersistentPreRunE. An unrecognized level is an
	// error either way: somebody typed it, and a silent downgrade
	// to info would hide the typo. Neither set is silence: a
	// library that logs uninvited pollutes its importer's output.
	//
	// Resolved first, ahead of the origins, because settling those
	// warns about the ones it drops and this is what those
	// warnings go to.
	log, err := b.logger()
	if err != nil {
		return err
	}

	// The origins this run exposes, settled argv > environment >
	// seed and parsed. Anything unusable was dropped with a
	// warning rather than failing the run, so what is left is what
	// the tunnel gets — but nothing left at all is still an error,
	// since a tunnel with no origin is a public hostname that
	// answers only errors. The message names both ways of
	// supplying one, because that is the choice an operator makes
	// to fix it.
	origins := b.Origins()
	if origins.Len() == 0 {
		return fmt.Errorf("%w: pass a local service URL as an argument (e.g. %s http://localhost:3000), or set $%s", v1.ErrNoOrigin, b.Name(), v1.OriginsEnv)
	}

	// A misspelled provider is an error, and it is worth finding out now:
	// everything below this opens something — a daemon connection, a
	// listener, a tunnel — and a typo should cost none of it.
	if err := b.identity.Known(b.identityProviders); err != nil {
		return err
	}

	// The handle the event listener ends the run through. A signal
	// cancels the parent with no cause; a reap cancels this one with
	// ErrTunnelGone, and the cause is what tells the two apart at
	// the bottom of this function. Wrapped here so everything below
	// — the tunnel, the attach servers, the browser probe — comes
	// down with it.
	ctx, gone := context.WithCancelCause(ctx)
	defer gone(nil)

	// A container is not an HTTP service, so tunneld serves one on
	// its behalf and hands the tunnel the loopback address instead.
	// origins stays what the operator typed — it is what the
	// reported map and the panel show.
	dialable, bound, err := b.binder.Bind(ctx, origins, log)
	if err != nil {
		return err
	}
	defer bound.Close()

	// The cache this run uses, or none. A local rather than the field, so a
	// run started with --no-cache does not leave an embedder's builder without
	// a cache for the next one.
	spec := b.cache
	if b.noCache {
		spec = nil
	}

	cached := ""
	if spec != nil {
		cached = spec.Load(origins, log)
	}

	// Pure-lazy: nothing dials until URL below trips the start.
	// WithContext upgrades URL from "the hostname resolves" to
	// "reachable end to end" and makes it return nil on cancel, so a
	// signal during startup exits cleanly.
	start := func(spec string) libtunnel.TunnelV1 {
		// Events: the tunnel's lifecycle listener. It logs what
		// happened and ends the run once the edge has disowned the
		// tunnel for long enough to be sure.
		//
		// The engine keeps retrying a reaped tunnel indefinitely —
		// that is cloudflared's behaviour and libtunnel leaves it
		// alone — so without this the process sits there holding a
		// hostname that resolves nowhere, reporting nothing.
		// Cancelling with a cause is what turns that into an exit
		// code a supervisor can act on.
		//
		// The logger is closed over rather than resolved again,
		// since it was already resolved above and a bad --log-level
		// refused; asking a second time here would have to discard
		// that error to satisfy the listener's signature.
		var once sync.Once
		listen := func(e libtunnel.Event) {
			log.Debug("received event", "e", e)
			b.counter.Count(e)
			if b.counter.IsGone() {
				// The counter stays tripped once it has been, and
				// verdicts keep arriving while the tunnel comes
				// down. Without the latch every one of them repeats
				// the error and cancels again.
				once.Do(func() {
					log.Error("the edge has disowned this tunnel; stopping", "hostname", e.Hostname)
					gone(fmt.Errorf("%w: %s", v1.ErrTunnelGone, e.Hostname))
				})
			}
		}

		// The credential the mint request carries, looked for here rather than
		// with the rest of the settling because looking costs a subprocess and
		// a run that failed earlier should not have paid for it. Nothing found
		// is the ordinary case: the mint is anonymous, as every mint was
		// before this existed.
		token := b.identity.Token(ctx, b.identityProviders, log)

		tun := b.engine.Tunnel(spec, b.provider, token).
			WithLogger(log).
			WithContext(ctx).
			WithEventListener(listen).
			// libtunnel takes the addresses themselves: the list's identity
			// is this run's business, and what it proxies to is a slice.
			WithLocalURL(dialable.URLs()...)
		// Served in front of the origin proxy, so the panel needs no
		// port of its own and no origin ever sees the request. The
		// list is empty when there is no panel to serve, which is
		// the only place that decision is made.
		for _, ic := range b.display.Interceptors(b.multiview, origins, log) {
			tun.WithInterceptor(ic)
		}
		return tun
	}
	tun := start(cached)

	log.Info("tunneld starting", "version", Version(), "libtunnel", libtunnel.Version(), "origins", origins.Len())

	// The banner goes out before the tunnel is asked for a URL, not
	// after it answers. Minting is the slow part and the part that
	// fails, and tying the banner to success meant a start that
	// failed printed nothing at all — no version, no sign the
	// program had run. Everything above this line is configuration,
	// so a bad flag or an origin that cannot be reached still fails
	// without one.
	// The origins only when they name a file this run would use: a banner
	// reporting a cache key for a run that caches nothing names something that
	// does not exist, which is worse than naming nothing.
	banner := origins
	if spec == nil {
		banner = nil
	}
	fmt.Fprintln(stderr, VersionLine(banner))

	// Something turning, because the wait below is the long one: minting,
	// dialing the edge, and then the hostname becoming resolvable, which is
	// seconds of a program that has printed its version and gone quiet.
	//
	// On stderr with the banner it follows, never stdout: that stream is one
	// public address per origin and nothing else, and a spinner in it is a
	// carriage return where a script expected a URL.
	//
	// Only when somebody is watching, and only when nothing else is writing
	// there. A log line lands on top of a spinner, so a run with its logger
	// on gets the lines instead — they say more than a spinner does, and they
	// are what the operator asked for.
	//
	// It waits on Ready, and it has to be Ready. Nothing has dialed yet — the
	// tunnel is lazy and asking for this is what trips it — so a spinner
	// waiting on anything the dial produces, the counter's Established
	// included, would be waiting for a dial that its own waiting prevents.
	//
	// A second call, not the one the receive below holds. Ready hands out a
	// channel per call, each delivering once and then closing, so both see
	// the tunnel; sharing one would let the spinner take the value and leave
	// the run reading a closed channel, which is how a tunnel that came up
	// reports that it never did.
	//
	// Blocking is what keeps the line clean: an address cannot be printed
	// over a frame that has not stopped turning yet. And it ends either way,
	// because Ready closes when a tunnel fails as surely as it delivers when
	// one comes up — including on the signal that cancelled the run.
	if display.IsInteractive(cmd) && b.logLevel == "" {
		console.Loading(stderr, tun.Ready(), "Creating tunnel...")
	}

	// Ready delivers the tunnel once the edge connection is up
	// and the hostname resolves publicly — reachable end to end,
	// which is the moment an address is worth handing to anybody.
	// The channel closing without delivering is the tunnel saying
	// it never came up at all, and Err is why.
	//
	// This is the wait that used to be URL()'s alone. URL blocks
	// on the same readiness and answers nil for the same failure,
	// so the two are interchangeable as a signal; asking Ready
	// says which of the two questions is being asked.
	up, ok := <-tun.Ready()
	if !ok {
		cause := cmp.Or(tun.Err(), ctx.Err(), v1.ErrNotReady)

		// The edge refused the credential this spec carries. That is
		// the whole of the class now: a reservation that lapsed is
		// adopted on whatever hostname the provider minted in its
		// place, silently and without an error, so the only way a
		// replay still fails here is the one where the provider was
		// never reached — the spec is served as given, and the edge
		// is where a dead one is finally found out.
		//
		// Which makes the file the thing at fault, and leaving it in
		// place the real cost: every later run replays it, is served
		// it again, and dies at the same edge. Drop it, then mint
		// once, which is a tunnel rather than an explanation if the
		// provider is reachable by now and the same error either way
		// if it is not.
		//
		// Only this class. ErrRejected is a mint the provider
		// refused outright or a request that could not be built at
		// all — configuration, and nothing the stored spec had a
		// part in. Discarding on it would throw away a good
		// credential over an unroutable provider or a bad header.
		if cached == "" || !errors.Is(cause, libtunnel.ErrCredentialRejected) {
			return cause
		}
		if spec != nil {
			spec.Discard(origins, log)
		}
		log.Warn("the cached tunnel is gone; minting a new one", "error", cause)

		tun = start("")
		if up, ok = <-tun.Ready(); !ok {
			return cmp.Or(tun.Err(), ctx.Err(), v1.ErrNotReady)
		}
	}
	public := up.URL()

	// A bound container is served before the tunnel exists —
	// the binding is what the tunnel is handed to proxy to — so
	// this is the first moment anything down there can be told
	// where it answers from outside. Each origin gets its own
	// address rather than the bare one, because with several of
	// them it is the routing parameter that reaches this one.
	addresses := make([]string, origins.Len())
	for i := range addresses {
		addresses[i] = publicURL(public, i, origins.Len())
	}
	bound.Announce(addresses)

	// The panel's address when there is a panel, "" when there is
	// not: the browser answers the question, and everything below
	// reads the answer.
	view := b.display.URL(b.multiview, public, origins)

	// The report: write the human-readable map to stderr, a line
	// per public address with the origins it reaches indented
	// beneath it. With a panel that is one address and every
	// origin; without, one address per origin.
	//
	// Nothing goes to stdout. It used to carry one bare URL per
	// origin as a machine interface, which meant every address
	// printed twice wherever the two streams landed together — a
	// terminal, a container's logs — and the de-duplication that
	// hid it could only see the case where one file descriptor was
	// literally the other. Under Docker they are two pipes that
	// merge downstream, so it never fired where it was needed most.
	// The banner is already on stderr by the time this runs: it was
	// printed above, before minting, so it survives a mint that
	// fails.
	//
	// Wait for the public URL to answer before any of it is
	// printed.
	//
	// Ready, which the tunnel above was waited on for, means the
	// connection is up and the hostname resolves. It does not
	// mean the edge has registered the route: for a
	// moment after that it answers 530. An address printed inside
	// that moment is one a script can read and cannot yet use, and
	// a browser opened into it shows an error page for a tunnel
	// that is about to work. Both readers are served by the same
	// wait, so it sits above the report rather than beside the
	// display.
	//
	// The counter answers whether the edge is up; how long that
	// is worth waiting for is this caller's policy, so the bound
	// is a context rather than something the counter carries.
	// Everything below is behind this wait, the cache save
	// included, so it cannot be unbounded.
	if !b.counter.IsEstablished() {
		log.Info("waiting for the public address to answer")
		<-b.counter.Established(context.WithTimeout(ctx, b.establishDeadline))
	}

	// Every public address gets a line, with what it reaches
	// indented beneath. A panel is the case where one address
	// reaches them all; otherwise each origin has an address of its
	// own. One shape either way, and no column to keep aligned as
	// hostnames change length.
	if view != "" {
		fmt.Fprintf(stdout, "%s\n", view)
		for _, origin := range origins.URLs() {
			fmt.Fprintf(stderr, "  -> %s\n", origin)
		}
	} else {
		for i, origin := range origins.URLs() {
			fmt.Fprintf(stdout, "%s\n", publicURL(public, i, origins.Len()))
			fmt.Fprintf(stderr, "  -> %s\n", origin)
		}
	}

	// Putting the tunnel in front of a person is the browser package's, both
	// ways it can be done: a tab, or the console this was started from. What
	// is reported here is only what it cannot see for itself.
	//
	// Whether there is a console at all is the console package's answer, not
	// this function's: it is handed the bound origins and this command's own
	// streams and says nil when there is nothing to draw or nowhere to draw
	// it. One page, never a fan of tabs — the panel when there is one, since
	// it reaches every origin, and otherwise the default origin itself.
	//
	// Reported after the addresses, so what a person came for is on the
	// screen before a frame takes it, and left behind when that frame ends: a
	// detach gives the console back and the tunnel goes on without it.
	// OPEN=false is the hammer, and it is spelled as the two facts Open
	// already takes rather than as a gate around the call: there is no console
	// to draw on, and the caller has decided against a tab. How a run is shown
	// stays one switch in display, and the hint below still fires — from here
	// this is a run with no screen, which is exactly what it is.
	screen, open := b.console.For(bound, cmd), b.open
	if os.Getenv(openEnv) == "false" {
		shown := false
		screen, open = nil, &shown
	}
	b.display.Open(ctx, log,
		display.WithAddr(cmp.Or(view, publicURL(public, 0, origins.Len()))),
		display.WithForced(open),
		display.WithStderr(stderr),
		display.WithInteractive(display.IsInteractive(cmd)),
		display.WithScreen(screen),
	)
	if screen == nil {
		// Nothing is going to be drawn here. The addresses are up, the run
		// blocks from now on, and the signal is the only thing left on this
		// side of it.
		fmt.Fprintln(stderr, stopHint)
	}

	// After the URL is live, so what gets cached is a tunnel that
	// came up rather than one that was merely asked for.
	if spec != nil {
		spec.Save(origins, b.tracking(origins), log)
	}

	// A viewer asking to end the run is the third way this stops, beside a
	// signal and the tunnel failing. Nothing is wrong when it happens, so it
	// reads as a clean exit — the deferred teardown below takes the origins,
	// the programs they started and the tunnel with it.
	select {
	case <-ctx.Done():
	case <-tun.Done():
	case <-bound.Done():
		log.Info("a viewer asked this run to end; stopping")
		return nil
	}

	// Cancelling ctx ends the tunnel too, so a reap makes both arms
	// above ready at once and the race would otherwise decide which
	// error an operator is shown. The cause is the verdict either
	// way: it outranks whatever the teardown it triggered has to
	// say for itself.
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if ctx.Err() != nil {
		return nil // signaled after the tunnel came up: clean shutdown
	}
	return tun.Err()
}

// applyEnv copies environment values onto the flags the command line did not
// set, which is what makes the precedence flag > env > default.
//
// A value that the flag refuses is an error wrapping v1.ErrInvalidEnv, naming
// the variable and the offending value: env beats code, so a typo'd override
// that silently fell back would be indistinguishable from one that worked.
//
// Idempotent, because two paths reach it: PersistentPreRunE for a command
// being executed, and Run for a caller who never executes one. A flag it has
// already set is marked Changed, and a Changed flag is skipped, so the second
// call does nothing — and a flag the command line set is skipped by the same
// test, which is the precedence rule itself.
func (b *BuilderImpl) applyEnv(cmd *cobra.Command) error {
	env := b.environment()
	var err error
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if err != nil || f.Changed || !env.IsSet(f.Name) {
			return
		}
		value := env.GetString(f.Name)

		// A repeatable flag takes the whole list at once. Replace, not
		// Append: the flag's default may be a seeded value, and the
		// environment overrides a seed rather than extending it — the same
		// rule pflag's stringArray applies to the command line.
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			err = slice.Replace(splitList(value))
		} else {
			err = f.Value.Set(value)
		}
		if err != nil {
			err = fmt.Errorf("%s=%q: %w: %w", flagEnv[f.Name], value, v1.ErrInvalidEnv, err)
			return
		}
		// Marking it changed is what stops cobra from reporting a required
		// flag as missing when its variable supplied it.
		f.Changed = true
	})
	return err
}

// environment is this builder's environment binding, built on first use: every
// flag paired with the variable that mirrors it, plus the origins key that has
// no flag to hang off. Origins reads it as well as Command, and can be called
// on a builder whose command was never assembled, which is why it is built
// here rather than during assembly.
func (b *BuilderImpl) environment() *viper.Viper {
	b.envOnce.Do(func() {
		b.env = viper.New()
		for flag, envVar := range flagEnv {
			// BindEnv only errors when given no name at all, which the
			// registry above cannot produce.
			_ = b.env.BindEnv(flag, envVar)
		}
		// Origins are arguments rather than a flag, so they have no entry in
		// flagEnv and no flag for PersistentPreRunE to copy a value onto. They
		// get a key of their own, which Origins reads them back by.
		_ = b.env.BindEnv(originsKey, v1.OriginsEnv)
	})
	return b.env
}

// logger resolves the tunnel's log sink from the level the command settled on
// — the --log-level flag, or v1.LogEnv bound onto it by PersistentPreRunE. An
// unrecognized level is an error: somebody typed it, and a silent downgrade to
// info would hide the typo. Unset is silence, since a library that logs
// uninvited pollutes its importer's output.
//
// The sink is the built command's own stderr, so logs never pollute the
// machine-readable URLs on stdout and an embedding program that called SetErr
// sees them where it is looking. Never os.Stderr directly — that is only where
// cobra falls back to when nothing was set.
//
// A logger comes back either way. The error says the level was refused, which
// is RunE's to report; a caller that only wants somewhere to warn (Origins)
// takes the silent one and leaves the report to the run.
func (b *BuilderImpl) logger() (*slog.Logger, error) {
	if b.logLevel == "" {
		return slog.New(slog.DiscardHandler), nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(b.logLevel)); err != nil {
		return slog.New(slog.DiscardHandler), fmt.Errorf("%w: %q, want debug, info, warn or error (--log-level or $%s)", v1.ErrInvalidLogLevel, b.logLevel, v1.LogEnv)
	}
	// Through the ring, so the same lines stderr shows are the ones a terminal
	// can show. A builder assembled as a bare struct rather than through New
	// has none, and logs the way it always did.
	handler := slog.Handler(slog.NewTextHandler(b.Command().ErrOrStderr(), &slog.HandlerOptions{Level: level}))
	if b.recent != nil {
		handler = b.recent.Wrap(handler)
	}
	return slog.New(handler), nil
}

// Origins reports the local origins this command exposes, in order: the first
// is the default and each later one answers on a bare ?n routing parameter.
//
// Three layers settle argv > environment > seed, which is the precedence a
// flag has over its variable over its default. Each layer replaces the one
// under it rather than extending it: a caller that names origins means those
// origins, not those plus whatever the program was built with. Argv is read
// off the built command, so before it runs this reports what the environment
// or the seed would expose, and from inside the run it reports what did.
//
// Two shorthands are filled in, both of them what people actually type: a
// value with no scheme implies http, and a value with no host implies
// localhost, so ":8000", "localhost:8000" and "http://localhost:8000" are one
// origin written three ways.
//
// A bare word this machine can run is a program rather than a hostname, so it
// is rewritten under v1.ExecScheme before the http default claims it: `tunneld
// htop` exposes htop's terminal, not the unresolvable host "htop".
//
// Anything else is dropped with a warning rather than failing the run — an
// unparsable URL, a scheme that is none of http, https, v1.AttachScheme or
// v1.ExecScheme, a served value carrying more than a reference, a URL with no
// host at all. One typo used to take every other origin down with it, and the
// ones that work are what somebody is waiting on; the warning names the value
// that did not, which is what a person needs to fix it. A run left with no
// origins at all is still an error, reported by the command rather than here.
//
// A second origin claiming the websockets loses only its marker, not itself: a
// handshake carries nothing to tell two claimants apart, so the first one
// keeps the designation and the later one is exposed as the plain origin it
// otherwise is.
//
// The warnings go to the same sink and level as the tunnel's own logs, so an
// unset --log-level (or v1.LogEnv) means a dropped origin is dropped silently
// — the same silence everything else in a default run keeps.
// tracking is what this run settled on, keyed by the variable that names each
// knob, for the cache to write beside the spec.
//
// Settled rather than read back out of the environment, because most of these
// never arrive that way: `tunneld :3000 --log-level debug` sets neither
// TUNNELD_ORIGINS nor TUNNELD_LOG, and a file recording only what the
// environment happened to hold would be empty for the common run.
//
// It is written and never read — Load takes the spec and nothing else. A cache
// that fed these back into the next run would pin a choice made once into
// every run afterwards, which is the rule the saved allowlist already keeps.
//
// PWD is here rather than on the origins because the cache has no business
// asking what directory a key was computed from, and in any run that is this
// process's own: both come from os.Getwd. An embedder that overrode the
// origins' directory is the one case where the two differ, and this line is
// then what the process was doing rather than what the key was made of.
//
// No credential belongs in this map. The mint token is the account's where a
// spec is one tunnel's, and nothing that names one is a knob a run settled.
func (b *BuilderImpl) tracking(origins Origins) map[string]string {
	shown := make([]string, 0, origins.Len())
	for _, u := range origins.URLs() {
		shown = append(shown, u.String())
	}

	out := map[string]string{
		v1.OriginsEnv:           strings.Join(shown, ","),
		v1.ProviderEnv:          b.provider,
		v1.LogEnv:               b.logLevel,
		v1.MultiviewEnv:         strconv.FormatBool(b.multiview),
		v1.ShellFallbackEnv:     strconv.FormatBool(b.shellFallback),
		v1.NoCacheEnv:           strconv.FormatBool(b.noCache),
		v1.IdentityProvidersEnv: strings.Join(b.identityProviders, ","),
	}
	// Facts about the run rather than knobs, so they carry no v1 constant and
	// nothing reads them back: which build wrote the file, and where it was
	// standing. A spec that stops replaying after an upgrade is a question
	// about the build that minted it, and the file is the only place left
	// holding the answer.
	out["TUNNELD_VERSION"] = Version()
	if wd, err := os.Getwd(); err == nil {
		out["PWD"] = wd
	}
	return out
}

// cached is the origins when this run would cache them, and nil when it would
// not — what the build banner names, so it never reports a key for a file
// nothing will write.
func (b *BuilderImpl) cached() Origins {
	if b.noCache || b.cache == nil {
		return nil
	}
	return b.Origins()
}

func (b *BuilderImpl) Origins() Origins {
	// A refused --log-level is the run's error to report, not this one's; here
	// it just means the warnings below go nowhere.
	log, _ := b.logger()

	settled := b.origins
	if env := b.environment(); env.IsSet(originsKey) {
		settled = splitList(env.GetString(originsKey))
	}
	if args := b.Command().Flags().Args(); len(args) > 0 {
		settled = args
	}

	// Nothing to expose is still a question with an answer: the shell of the
	// person who typed it. It is the one origin every machine has, it needs no
	// port to be listening, and tunneld already knows how to serve a program.
	//
	// Behind a knob, because the answer is only right when a person typed it.
	// An embedding program mounting tunneld under its own verb inherits this,
	// and somebody who meant to name an origin and did not should be told so
	// rather than handed a public terminal onto their own machine —
	// WithShellFallback(false), --shell-fallback=false, or ShellFallbackEnv.
	// It is also what lets a caller ask what a run will do without reading a
	// variable the run reads behind its back.
	//
	// Resolved here rather than left for the loop below, because the loop's
	// fallback for a word it cannot resolve is to read it as an address —
	// which turns a $SHELL naming a program that is not there into a proxy to
	// http://localhost/bin/nope, a tunnel to nothing that reports no problem.
	// Dropping it instead leaves the count at zero, and zero has a message
	// that names the lever.
	if len(settled) == 0 && b.shellFallback {
		if sh := os.Getenv("SHELL"); sh != "" {
			if path, ok := shell.Resolve(sh); ok {
				log.Info("no origin given; exposing this machine's shell", "shell", path)
				settled = append(settled, path)
			} else {
				log.Warn("not exposing a shell", "shell", sh, "reason", "$SHELL names no program that can be run")
			}
		}
	}

	urls := make([]*url.URL, 0, len(settled))
	// The first origin seen carrying a +ws marker, kept to strip a second.
	wsOrigin := ""
	for _, s := range settled {
		s = strings.TrimSpace(s)
		if s == "" {
			log.Warn("dropping an origin", "origin", s, "reason", "empty, pass a local service URL (e.g. http://localhost:3000)")
			continue
		}
		// Short circuit for executables: a path to a binary is not a URL, and the binder runs it
		if path, ok := shell.Resolve(s); ok {
			// Built rather than parsed, and done with: the reference resolved
			// a line ago, so there is nothing for the checks below to add.
			//
			// The resolved path rather than the word typed, because the origin
			// is what everything downstream shows and what somebody pastes
			// back — "top" names a program only on the machine that looked it
			// up. It goes in Path and not Host because that is the only shape
			// an absolute path survives: url.URL escapes the separators of a
			// host, so exec:///usr/bin/top round-trips and exec://%2Fusr%2Fbin
			// is what the other spelling produces. The empty authority is this
			// machine, which is the whole of what exec:// with no provider says.
			urls = append(urls, &url.URL{Scheme: v1.ExecScheme, Path: path})
			continue
		}
		if !strings.Contains(s, "://") {
			s = "http://" + s
		}
		u, err := url.Parse(s)
		if err != nil {
			log.Warn("dropping an origin", "origin", s, "reason", "not a URL", "err", err)
			continue
		}
		// A served origin is not proxied at all: it names a thing the binder
		// stands a loopback server up for — a container, a program — rather
		// than an address to reach. Everything the shorthands below fill in — a
		// default scheme, a default host, a preserved path — is meaningless
		// here, so the value is taken exactly as typed and anything extra is
		// dropped rather than silently ignored.
		if what, served := servedSchemes[u.Scheme]; served {
			// An authority with nothing after it is not a provider being asked
			// for something — there is nothing to ask for — so where the
			// scheme allows it, the word is looked up as a program first. It
			// is rewritten to the resolved path for the same reason the bare
			// argument above is: the word names a program only on the machine
			// that looked it up.
			if what.resolve != nil && u.Host != "" && u.Path == "" {
				if path, ok := what.resolve(u.Host); ok {
					urls = append(urls, &url.URL{Scheme: u.Scheme, Path: path})
					continue
				}
			}
			// The reference is the path. The authority beside it names the
			// provider that resolves the reference, and an empty one is this
			// machine — which is why the binder, not this loop, decides how
			// much of the path is the reference: with a provider the leading
			// separator is the URL's, and without one the path is absolute and
			// keeps every byte. Here only its emptiness matters.
			if strings.TrimPrefix(u.Path, "/") == "" {
				log.Warn("dropping an origin", "origin", s, "reason", "names no "+what.noun+", pass e.g. "+what.example)
				continue
			}
			// A reference and nothing else: a query, a fragment or a userinfo
			// is not part of the name of anything served.
			if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
				log.Warn("dropping an origin", "origin", s, "reason", "carries more than a "+what.noun+" reference, pass "+u.Scheme+"://"+u.Host+u.Path)
				continue
			}
			urls = append(urls, u)
			continue
		}
		// A +ws / +wss suffix declares that this origin owns WebSockets, so a
		// handshake the page did not build with a routing index goes here
		// rather than following the sticky cookie. The marker is stripped and
		// consumed by the tunnel engine; everything below treats the origin by
		// its base scheme, which is also how it is dialed.
		//
		// The two spellings (+ws and +wss) mean the same thing: the suffix
		// induces the designation rather than describing a transport — the
		// origin is dialed by its base scheme either way — so accepting both
		// spares an operator from reasoning about which one their service
		// "is", which is not a question the marker asks.
		base, marked := u.Scheme, false
		if s, ok := strings.CutSuffix(u.Scheme, "+wss"); ok {
			base, marked = s, true
		} else if s, ok := strings.CutSuffix(u.Scheme, "+ws"); ok {
			base, marked = s, true
		}
		if base != "http" && base != "https" {
			log.Warn("dropping an origin", "origin", s, "reason", "scheme is not http, https, "+v1.AttachScheme+" or "+v1.ExecScheme, "scheme", u.Scheme)
			continue
		}
		if marked {
			if wsOrigin != "" {
				// Two origins cannot both own the websockets. The marker goes
				// and the origin stays: it is a perfectly good origin that
				// asked for something already taken, and the tunnel engine
				// would refuse the pair outright.
				log.Warn("dropping a websockets marker", "origin", s, "reason", "already claimed by "+wsOrigin+", mark only one", "scheme", base)
				u.Scheme = base
			} else {
				wsOrigin = s
			}
		}
		// A port with no host in front of it — ":8000", or the "http://:8000"
		// the scheme default above makes of it — means the local machine, the
		// way every dev server reads that shorthand. The test is Hostname, not
		// Host: url.Parse keeps the colon, so ":8000" arrives as a non-empty
		// Host with nothing before the port, and a bare Host check waves it
		// through as the unresolvable origin "http://:8000".
		if u.Hostname() == "" {
			if u.Port() == "" {
				log.Warn("dropping an origin", "origin", s, "reason", "no host, pass e.g. http://localhost:3000")
				continue
			}
			u.Host = net.JoinHostPort("localhost", u.Port())
		}
		urls = append(urls, u)
	}
	return origins.New(origins.WithURL(urls...))
}

// openEnv is the hammer, and the one thing about how a run is shown that is
// typed rather than derived: OPEN=false shows this run nothing. No frame takes
// the console and no tab is launched, so the log lines keep the stderr they
// would otherwise have been muted for — which is the whole of why it exists,
// since the frame is drawn over exactly the output somebody reaching for it is
// trying to read.
//
// No flag, no TUNNELD_ mirror, and no row in the README. Every other variable
// this reads is one half of a knob an operator is meant to find; this is a way
// out of the console for the case the console is in the way, and a knob that
// turns the product off is not a feature of it. The bare word is the cost of
// being quick to type, and only the exact value "false" swings it: an OPEN
// that some other program left in the environment is overwhelmingly unlikely
// to be spelled that way, and anything this does not recognise leaves the
// derived decision alone.
const openEnv = "OPEN"

// stopHint is what a console with nothing left to draw is told. The addresses
// are printed, the tunnel is up, and from here the run is a block on a signal
// — which is worth saying out loud, because a terminal sitting at no prompt
// with no cursor looks the same whether it is waiting or wedged.
const stopHint = "Press Ctrl+C to stop the tunnel..."

// publicURL is the address origin i answers on, out of n origins: the tunnel's
// URL with a bare ?i routing parameter. Bare is load-bearing — a valued
// parameter ("?1=x") is application data the proxy forwards, while the bare
// form is the routing directive it consumes and strips before the request
// reaches the origin.
//
// The default origin is explicit too, as ?0, whenever there is more than one.
// A bare URL routes by the referring page and then by the sticky cookie, so
// once a browser has visited ?1 a plain address no longer reaches origin 0 —
// only an explicit index clears a previous choice. An address that stops
// working after someone clicks around is worse than a longer one.
//
// A lone origin has nothing to route between, so n of 1 gives the plain URL
// and no parameter at all.
func publicURL(public *url.URL, i, n int) string {
	if n <= 1 {
		return public.String()
	}
	routed := *public
	routed.RawQuery = strconv.Itoa(i)
	return routed.String()
}
