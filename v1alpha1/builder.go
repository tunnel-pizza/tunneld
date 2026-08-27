package v1alpha1

import (
	"cmp"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// WithName sets the built command's name.
func (b *BuilderImpl) WithName(name string) v1.Builder {
	b.name = name
	return b
}

// WithURL seeds the local origins to expose, appending across calls.
func (b *BuilderImpl) WithURL(urls ...string) v1.Builder {
	b.urls = append(b.urls, urls...)
	return b
}

// WithProvider sets the quick-tunnel provider host to mint against.
func (b *BuilderImpl) WithProvider(host string) v1.Builder {
	b.provider = host
	return b
}

// WithLogLevel sets the tunnel's stderr log level.
func (b *BuilderImpl) WithLogLevel(level string) v1.Builder {
	b.logLevel = level
	return b
}

// WithStdout redirects the public URLs.
func (b *BuilderImpl) WithStdout(w io.Writer) v1.Builder {
	b.stdout = w
	return b
}

// WithStderr redirects the banner, the origin map, and the tunnel's logs.
func (b *BuilderImpl) WithStderr(w io.Writer) v1.Builder {
	b.stderr = w
	return b
}

// Name returns the configured command name, defaulting to v1.CommandName.
func (b *BuilderImpl) Name() string {
	if b.name == "" {
		return v1.CommandName
	}
	return b.name
}

// Build assembles the configured command. It is the terminal step; the command
// is built once and cached, so repeated calls return the same *cobra.Command
// rather than a second one with a second set of flags bound to these fields.
func (b *BuilderImpl) Build() *cobra.Command {
	b.builtOnce.Do(func() { b.built = b.command() })
	return b.built
}

// command is the one-shot assembly behind Build.
func (b *BuilderImpl) command() *cobra.Command {
	name := b.Name()

	cmd := &cobra.Command{
		Use:   name + " --url <local-url> [--url <local-url> ...]",
		Short: "Expose local origins to the public internet through a quick tunnel",
		Long: name + ` exposes already-running local services to the public internet
through an in-process quick tunnel — no cloudflared binary, no account, no DNS.

Repeat --url per origin. They share one hostname: the first is the default,
each later one answers on a bare ?n parameter (n is that flag's position).

  ` + name + ` --url http://localhost:3000 --url http://localhost:4000

    https://<host>/     -> http://localhost:3000
    https://<host>/?1   -> http://localhost:4000

Public URLs go to stdout, one line per origin; logs go to stderr.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true, // usage answers a flag error, not a tunnel failure
		SilenceErrors: true, // the caller prints the error, prefixed, exactly once
		RunE: func(cmd *cobra.Command, _ []string) error {
			return b.run(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr())
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
	cmd.Flags().StringArrayVarP(&b.urls, "url", "u", b.urls,
		"local origin to expose, e.g. http://localhost:3000 (repeat for more; a bare host:port implies http)")
	cmd.Flags().StringVar(&b.provider, "provider", cmp.Or(b.provider, v1.DefaultProvider),
		"quick-tunnel provider host to mint against")
	cmd.Flags().StringVar(&b.logLevel, "log-level", b.logLevel,
		"tunnel log level on stderr: debug, info, warn, error (default: silent, or $"+v1.LogEnv+")")
	// Required only when nothing was seeded: an embedder that supplied an
	// origin wants --url optional, not forbidden.
	if len(b.urls) == 0 {
		_ = cmd.MarkFlagRequired("url")
	}

	cmd.AddCommand(versionCommand(name))
	return cmd
}

// versionCommand prints the build banner and exits — the build id of the
// binary plus the tunnel library it links against, since that library is what
// actually speaks to the edge and a bug report needs both numbers.
//
// The banner is written to OutOrStdout explicitly. cmd.Print and friends route
// through OutOrStderr, which falls back to os.Stderr, and a version a script
// cannot read off stdout is a version nobody can pipe.
func versionCommand(name string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the " + name + " build identifier and exit",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), VersionLine())
		},
	}
}
