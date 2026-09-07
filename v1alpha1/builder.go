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
)

// WithName sets the built command's name — the verb in usage strings and
// what cobra matches when the command is mounted under another root. Unset,
// the name is v1.CommandName.
func WithName(name string) Option {
	return func(b *BuilderImpl) { b.name = name }
}

// WithURL seeds the local origins to expose, in order: the first is the
// default origin and each later one answers on a bare ?n parameter. Repeated
// options append. A --url flag on the command line replaces the whole seeded
// set rather than adding to it.
//
// A missing scheme implies http and a missing host implies localhost, so
// ":8000", "localhost:8000" and "http://localhost:8000" name one origin.
func WithURL(urls ...string) Option {
	return func(b *BuilderImpl) { b.urls = append(b.urls, urls...) }
}

// WithProvider sets the quick-tunnel provider host to mint against. Unset,
// the provider is v1.DefaultProvider.
func WithProvider(host string) Option {
	return func(b *BuilderImpl) { b.provider = host }
}

// WithCacheDir adds directories to cache tunnel specs in, in order,
// appending across options. See package cachedir for what an entry means —
// a path, or true and false as instructions — and how entries are resolved
// and deduplicated.
//
// Like every option, it requires a builder from New: applied to a bare
// BuilderImpl{}, cacheDirs is nil and this dereferences it immediately, at
// option-apply time — before Command's wiring check ever runs.
func WithCacheDir(dirs ...string) Option {
	return func(b *BuilderImpl) { b.cacheDirs.Add(dirs...) }
}

// WithLogLevel sets the tunnel's log level (debug|info|warn|error) on
// stderr. Unset, the level comes from v1.LogEnv, and silence if that is
// unset too.
func WithLogLevel(level string) Option {
	return func(b *BuilderImpl) { b.logLevel = level }
}

// WithOpen sets whether a public URL is opened in a browser once the tunnel
// is live — the multiview panel when there is one, otherwise the default
// origin. Exactly one page is opened either way, since a fan of tabs is
// rarely what anyone wanted. Unset, the behaviour is v1.DefaultOpen.
//
// It seeds the default of --no-open, which reads inverted: WithOpen(false)
// makes --no-open default to true.
func WithOpen(open bool) Option {
	return func(b *BuilderImpl) { b.open = open }
}

// WithMultiview sets whether the tunnel's own address answers with a panel
// framing every origin. It does nothing with a single origin, which has
// nothing to sit beside and keeps the bare address for itself. Unset, the
// behaviour is v1.DefaultMultiview.
func WithMultiview(multiview bool) Option {
	return func(b *BuilderImpl) { b.multiview = multiview }
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
		// collaborator — Command reads cacheDirs directly a few lines down,
		// to seed and bind --cache-dir, so a check inside RunE would always
		// have been too late for that collaborator: it would run after
		// Command had already dereferenced a nil one. WithCacheDir needs one
		// sooner still, at option-apply time (see its doc) — like every
		// option, it assumes a builder from New, so this check is the first
		// place Command needs every collaborator, not the first place a nil
		// one can bite.
		//
		// A missing collaborator short-circuits with a minimal command whose
		// RunE returns the error and nothing else — no flag binding, no env
		// binding, so nothing downstream ever touches the nil field either.
		for _, c := range []struct {
			name    string
			missing bool
		}{
			{"cacheDirs", b.cacheDirs == nil},
			{"engine", b.engine == nil},
			{"cache", b.cache == nil},
			{"panel", b.panel == nil},
			{"opener", b.opener == nil},
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

		// The environment binding is per-builder, never viper's package
		// global: two commands in one process — a host program's and an
		// embedded tunneld's — would otherwise share one key space, and so
		// would two tests in one binary.
		env := viper.New()
		for flag, envVar := range flagEnv {
			// BindEnv only errors when given no name at all, which the
			// registry above cannot produce.
			_ = env.BindEnv(flag, envVar)
		}

		cmd := &cobra.Command{
			Use:   name + " --url <local-url> [--url <local-url> ...]",
			Short: "Expose local origins to the public internet through a quick tunnel",
			Long: name + ` exposes already-running local services to the public internet
through an in-process quick tunnel — no cloudflared binary, no account, no DNS.

Repeat --url per origin. They share one hostname: the first is the default,
each later one answers on a bare ?n parameter (n is that flag's position).

  ` + name + ` --url http://localhost:3000 --url http://localhost:4000

    https://<host>/?0   -> http://localhost:3000
    https://<host>/?1   -> http://localhost:4000

An origin can also be a running container, which is served as a terminal in
the browser rather than proxied:

  ` + name + ` --url dockerd://my-container

Mark one origin http+ws (or https+ws) when a service opens its own WebSocket —
a dev server's live reload, say. A handshake carries nothing that says which
origin it belongs to, so without the marker it goes to the first one:

  ` + name + ` --url :4000 --url http+ws://localhost:5173

The public URLs, the origin map and every log line go to stderr.`,
			Args:          cobra.NoArgs,
			SilenceUsage:  true, // usage answers a flag error, not a tunnel failure
			SilenceErrors: true, // the caller prints the error, prefixed, exactly once
			// Persistent, so it also covers a subcommand — and placed here rather
			// than in RunE because cobra runs this hook ahead of required-flag
			// validation, which is what lets TUNNELD_URL satisfy --url.
			PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
				// Environment values are copied onto the flags the command
				// line did not set, which is what makes the precedence
				// flag > env > default. This runs from PersistentPreRunE,
				// ahead of cobra's required-flag validation, so a flag
				// satisfied by its variable counts as supplied.
				//
				// A value that the flag refuses is an error wrapping
				// v1.ErrInvalidEnv, naming the variable and the offending
				// value: env beats code, so a typo'd override that silently
				// fell back would be indistinguishable from one that worked.
				var err error
				cmd.Flags().VisitAll(func(f *pflag.Flag) {
					if err != nil || f.Changed || !env.IsSet(f.Name) {
						return
					}
					value := env.GetString(f.Name)

					// A repeatable flag takes the whole list at once.
					// Replace, not Append: the flag's default may be a
					// seeded value, and the environment overrides a seed
					// rather than extending it — the same rule pflag's
					// stringArray applies to the command line.
					if slice, ok := f.Value.(pflag.SliceValue); ok {
						// A list-valued variable is comma-separated,
						// surrounding space trimmed, empty entries dropped
						// so a trailing comma is not an origin.
						items := make([]string, 0, strings.Count(value, ",")+1)
						for item := range strings.SplitSeq(value, ",") {
							if item = strings.TrimSpace(item); item != "" {
								items = append(items, item)
							}
						}
						err = slice.Replace(items)
					} else {
						err = f.Value.Set(value)
					}
					if err != nil {
						err = fmt.Errorf("%s=%q: %w: %w", flagEnv[f.Name], value, v1.ErrInvalidEnv, err)
						return
					}
					// Marking it changed is what stops cobra from reporting a
					// required flag as missing when its variable supplied it.
					f.Changed = true
				})
				return err
			},
			// RunE is the built command's body: it brings the tunnel up,
			// reports the public URLs, and blocks until ctx is canceled or
			// the tunnel fails. ctx is the shutdown handle — canceling it (a
			// signal, in the binary's case) tears the tunnel down during
			// startup as well as after, so this returns rather than hanging.
			// A tunnel that fails on its own returns the cause.
			//
			// stderr comes from the command's own ErrOrStderr, so cobra stays the
			// single owner of where output goes; it is never nil.
			//
			// The engine is github.com/cnuss/libtunnel driving Cloudflare's edge
			// in process — no cloudflared binary, no account, no DNS to
			// configure.
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				stderr := cmd.ErrOrStderr()

				// Origins: turn the settled origin values into URLs,
				// rejecting anything the tunnel could not proxy to.
				//
				// Two shorthands are filled in, both of them what people
				// actually type: a value with no scheme implies http, and a
				// value with no host implies localhost, so ":8000" and
				// "localhost:8000" and "http://localhost:8000" are one origin
				// written three ways. Everything else must carry an http or
				// https scheme and a host, so a typo surfaces here rather than
				// as a public hostname that answers only errors. Every failure
				// wraps a v1 sentinel and names the offending value.
				//
				// The messages name the value, not the flag, because by this
				// point a value may have arrived either way — through --url or
				// through v1.URLEnv bound onto it. Only the nothing-at-all case
				// names both, since that is the one an operator fixes by
				// choosing between them.
				origins := make([]*url.URL, 0, len(b.urls))
				// The first origin seen carrying a +ws marker, kept to reject a
				// second.
				wsOrigin := ""
				for _, s := range b.urls {
					s = strings.TrimSpace(s)
					if s == "" {
						return fmt.Errorf("%w: empty origin, pass a local service URL (e.g. http://localhost:3000)", v1.ErrNoOrigin)
					}
					if !strings.Contains(s, "://") {
						s = "http://" + s
					}
					u, err := url.Parse(s)
					if err != nil {
						return fmt.Errorf("%w: %q is not a URL: %w", v1.ErrInvalidOrigin, s, err)
					}
					// A container is not proxied at all: it is served, by a
					// loopback origin the binder stands up later. Everything the
					// shorthands below fill in — a default scheme, a default
					// host, a preserved path — is meaningless here, so the value
					// is taken exactly as typed and anything extra is an error
					// rather than a silent drop.
					if u.Scheme == v1.DockerScheme {
						if u.Host == "" {
							return fmt.Errorf("%w: %q names no container, pass e.g. dockerd://my-container", v1.ErrInvalidOrigin, s)
						}
						if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
							return fmt.Errorf("%w: %q carries more than a container reference; pass %s://%s", v1.ErrInvalidOrigin, s, v1.DockerScheme, u.Host)
						}
						origins = append(origins, u)
						continue
					}
					// A +ws / +wss suffix declares that this origin owns
					// WebSockets, so a handshake the page did not build with a
					// routing index goes here rather than following the sticky
					// cookie. The marker is stripped and consumed by the tunnel
					// engine; everything below treats the origin by its base
					// scheme, which is also how it is dialed.
					//
					// The two spellings (+ws and +wss) mean the same thing: the
					// suffix induces the designation rather than describing a
					// transport — the origin is dialed by its base scheme either
					// way — so accepting both spares an operator from reasoning
					// about which one their service "is", which is not a
					// question the marker asks.
					base, marked := u.Scheme, false
					if s, ok := strings.CutSuffix(u.Scheme, "+wss"); ok {
						base, marked = s, true
					} else if s, ok := strings.CutSuffix(u.Scheme, "+ws"); ok {
						base, marked = s, true
					}
					if base != "http" && base != "https" {
						return fmt.Errorf("%w: %q has scheme %q, want http, https or %s", v1.ErrInvalidOrigin, s, u.Scheme, v1.DockerScheme)
					}
					if marked {
						// Two origins cannot both own the WebSockets — a
						// handshake carries nothing to tell them apart, which is
						// the whole reason the marker exists. The engine rejects
						// this too; catching it here makes it a flag error
						// before the mint rather than a tunnel that cancels.
						if wsOrigin != "" {
							return fmt.Errorf("%w: %q and %q both claim the websockets, mark only one", v1.ErrInvalidOrigin, wsOrigin, s)
						}
						wsOrigin = s
					}
					// A port with no host in front of it — ":8000", or the
					// "http://:8000" the scheme default above makes of it —
					// means the local machine, the way every dev server reads
					// that shorthand. The test is Hostname, not Host: url.Parse
					// keeps the colon, so ":8000" arrives as a non-empty Host
					// with nothing before the port, and a bare Host check waves
					// it through as the unresolvable origin "http://:8000".
					if u.Hostname() == "" {
						if u.Port() == "" {
							return fmt.Errorf("%w: %q has no host, pass e.g. http://localhost:3000", v1.ErrInvalidOrigin, s)
						}
						u.Host = net.JoinHostPort("localhost", u.Port())
					}
					origins = append(origins, u)
				}
				if len(origins) == 0 {
					return fmt.Errorf("%w: pass --url (or $%s) with the local service URL (e.g. http://localhost:3000)", v1.ErrNoOrigin, v1.URLEnv)
				}

				// The logger: resolve the tunnel's log sink from the level the
				// command settled on — the --log-level flag, or v1.LogEnv bound
				// onto it by PersistentPreRunE. An unrecognized level is an
				// error either way: somebody typed it, and a silent downgrade
				// to info would hide the typo. Logger is the fallback when
				// neither was set, which is silence.
				//
				// The sink is stderr either way, so logs never pollute the
				// machine-readable URLs on stdout. WithLogger below shares this
				// logger between tunneld's own startup line and the tunnel's
				// internals, so both share one level.
				var log *slog.Logger
				if b.logLevel == "" {
					log = Logger()
				} else {
					var level slog.Level
					if err := level.UnmarshalText([]byte(b.logLevel)); err != nil {
						return fmt.Errorf("%w: %q, want debug, info, warn or error (--log-level or $%s)", v1.ErrInvalidLogLevel, b.logLevel, v1.LogEnv)
					}
					log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
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
				dialable, closeOrigins, err := b.binder.Bind(ctx, origins, log)
				if err != nil {
					return err
				}
				defer closeOrigins.Close()

				cached := ""
				if len(b.cacheDirs.GetSlice()) > 0 {
					cached = b.cache.Load(b.cacheDirs.GetSlice(), log)
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

					tun := b.engine.Tunnel(spec, b.provider).
						WithLogger(log).
						WithContext(ctx).
						WithEventListener(listen).
						WithLocalURL(dialable...)
					// Served in front of the origin proxy, so the panel needs no
					// port of its own and no origin ever sees the request.
					if b.panel.Wanted(b.multiview, origins) {
						for _, ic := range b.panel.Interceptors(origins, log) {
							tun.WithInterceptor(ic)
						}
					}
					return tun
				}
				tun := start(cached)

				view := ""

				log.Info("tunneld starting", "version", Version(), "libtunnel", libtunnel.Version(), "origins", len(origins))

				// The banner goes out before the tunnel is asked for a URL, not
				// after it answers. Minting is the slow part and the part that
				// fails, and tying the banner to success meant a start that
				// failed printed nothing at all — no version, no sign the
				// program had run. Everything above this line is configuration,
				// so a bad flag or an origin that cannot be reached still fails
				// without one.
				fmt.Fprintln(stderr, VersionLine())

				public := tun.URL()
				if public == nil {
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
					b.cache.Discard(b.cacheDirs.GetSlice(), log)
					log.Warn("the cached tunnel is gone; minting a new one", "error", cause)

					tun = start("")
					if public = tun.URL(); public == nil {
						return cmp.Or(tun.Err(), ctx.Err(), v1.ErrNotReady)
					}
				}
				if b.panel.Wanted(b.multiview, origins) {
					view = b.panel.URL(public)
				}

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
				// Every public address gets a line, with what it reaches
				// indented beneath. A panel is the case where one address
				// reaches them all; otherwise each origin has an address of its
				// own. One shape either way, and no column to keep aligned as
				// hostnames change length.
				if view != "" {
					fmt.Fprintf(stderr, "  %s\n", view)
					for _, origin := range origins {
						fmt.Fprintf(stderr, "    -> %s\n", origin)
					}
				} else {
					for i, origin := range origins {
						fmt.Fprintf(stderr, "  %s\n", PublicURL(public, i, len(origins)))
						fmt.Fprintf(stderr, "    -> %s\n", origin)
					}
				}
				if !b.noOpen {
					// One page, never a fan of tabs: the panel when there is
					// one, since it reaches every origin, and otherwise the
					// default origin itself.
					target := cmp.Or(view, PublicURL(public, 0, len(origins)))
					b.opener.Open(ctx, target, stderr, log)
				}

				// After the URL is live, so what gets cached is a tunnel that
				// came up rather than one that was merely asked for.
				if len(b.cacheDirs.GetSlice()) > 0 {
					b.cache.Save(b.cacheDirs.GetSlice(), log)
				}

				select {
				case <-ctx.Done():
				case <-tun.Done():
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
			},
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
		// default and an argv value overwrites it. StringArray, not StringSlice:
		// a repeated flag must collect values verbatim, and StringSlice splits on
		// commas, which would silently shred a URL carrying one in its query.
		// pflag's stringArray replaces the default on the first --url and appends
		// after that, so a command line never merges into a seeded set.
		// Each usage string names the flag's environment mirror, so --help doubles
		// as the reference for configuring a container. The registry behind those
		// names is flagEnv, in env.go.
		cmd.Flags().StringArrayVarP(&b.urls, "url", "u", b.urls,
			"local origin to expose, e.g. http://localhost:3000, dockerd://my-container, or http+ws://localhost:5173 for the one that owns websockets (repeat for more; :8000 and localhost:8000 also work) [$"+v1.URLEnv+", comma-separated]")
		// Unset, specs cache into the default cache directory. Seeded here
		// rather than in New so that an explicit WithCacheDir replaces the
		// default instead of appending to it: a caller naming a directory
		// means that directory, not that one and wherever the process
		// happened to start.
		//
		// Nil, not empty: an empty list is one a false entry emptied, and seeding
		// over it would re-enable what an operator turned off.
		if b.cacheDirs.GetSlice() == nil {
			b.cacheDirs.Add("")
		}
		cmd.Flags().Var(b.cacheDirs, "cache-dir",
			"directory to cache tunnel specs in (repeat for more; empty or true means the default, false disables it) [$"+v1.CacheDirEnv+", comma-separated]")
		cmd.Flags().StringVar(&b.provider, "provider", cmp.Or(b.provider, v1.DefaultProvider),
			"quick-tunnel provider host to mint against [$"+v1.ProviderEnv+"]")
		cmd.Flags().StringVar(&b.logLevel, "log-level", b.logLevel,
			"tunnel log level on stderr: debug, info, warn, error (default: silent) [$"+v1.LogEnv+"]")
		cmd.Flags().BoolVar(&b.noOpen, "no-open", !b.open,
			"do not open a public URL in a browser once the tunnel is live [$"+v1.NoOpenEnv+"]")
		cmd.Flags().BoolVar(&b.multiview, "multiview", b.multiview,
			"answer the tunnel's own URL with a panel framing every origin [$"+v1.MultiviewEnv+"]")
		// Required only when nothing was seeded: an embedder that supplied an
		// origin wants --url optional, not forbidden.
		if len(b.urls) == 0 {
			_ = cmd.MarkFlagRequired("url")
		}

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
				fmt.Fprintln(cmd.OutOrStdout(), VersionLine())
			},
		})

		b.command = cmd
	})
	return b.command
}

// PublicURL is the address origin i answers on, out of n origins: the tunnel's
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
func PublicURL(public *url.URL, i, n int) string {
	if n <= 1 {
		return public.String()
	}
	routed := *public
	routed.RawQuery = strconv.Itoa(i)
	return routed.String()
}
