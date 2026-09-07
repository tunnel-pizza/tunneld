package v1alpha1

import (
	"cmp"
	"fmt"
	"io"
	"strings"

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
			RunE: func(cmd *cobra.Command, _ []string) error {
				return b.run(cmd.Context(), cmd.ErrOrStderr())
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
