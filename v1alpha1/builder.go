package v1alpha1

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// appending across options.
//
// An entry that is a boolean is an instruction rather than a path: true names
// the default location, false names nothing at all. That is what lets one
// field take both a switch and a list — "on" is what an operator means by
// setting the variable to true, and the default cache directory computed
// below is the answer that needs no further configuration. An empty entry
// reads as true, since nothing else it could mean is useful.
//
// The false half matters as much as the true half. Without it, an operator
// turning the knob off would get a cache directory literally named "false",
// silently, in whatever directory they happened to start from.
//
// False is a verdict on the whole list rather than on its own place in it: it
// stops there, drops every directory collected so far, and holds — so
// "/a,false,/b" caches nowhere, and so does "false,/b". A switch that only
// cancelled the entries before it would make "off" depend on where in the list
// somebody wrote it, and the reading where off means off is the one an
// operator can be sure of. A later source still overrides: the flag replaces
// what the variable said, and the variable replaces a seed.
//
// Entries are resolved to absolute paths, so a later chdir cannot move a cache
// out from under the process, and that is also what makes deduplication mean
// anything: "." and the working directory's own path are two spellings of one
// directory, and a list that cached to it twice is not a list anybody wrote on
// purpose.
//
// A directory that cannot be resolved is dropped rather than reported. The
// only way that happens is os.Getwd failing, which is the same condition that
// already turns an empty entry into nothing, and neither is worth failing a
// tunnel over.
func WithCacheDir(dirs ...string) Option {
	return func(b *BuilderImpl) {
		// A list that exists and is empty is one a false entry emptied, and
		// nothing refills it: off holds until a later source sets the field
		// back to nil and starts over.
		if b.cacheDirs != nil && len(b.cacheDirs) == 0 {
			return
		}
		for _, dir := range dirs {
			// Whether dir is a boolean rather than a path, and which one:
			// the spellings are strconv.ParseBool's — 1/t/T/TRUE/true/True
			// and 0/f/F/FALSE/false/False — so both halves of the knob are
			// the ones an operator would guess. Anything else is a path,
			// including "yes" and "no", which ParseBool has never accepted
			// and this should not start accepting on its own.
			if on, err := strconv.ParseBool(dir); err == nil {
				if !on {
					b.cacheDirs = []string{}
					return
				}
				dir = ""
			}
			if dir == "" {
				// Where a spec goes when nothing says otherwise: a
				// per-project directory under the user's cache directory.
				//
				// Not the working directory, which is what this used to be.
				// A spec is credentials, the working directory is usually a
				// repository, and no filename avoids being committed there:
				// measured against GitHub's 239 gitignore templates and 752
				// real ones, the best a name managed was 13% and 26%.
				// Nothing written into somebody's checkout is safe by
				// default, so nothing is written there.
				//
				// The working directory still decides *which* cache,
				// because two projects on one machine are two tunnels. It is
				// fingerprinted rather than mirrored: a path cannot be a
				// single path element, and hashing it sidesteps every
				// question about separators, length and case. The base name
				// is kept as a prefix so the directory is recognisable to a
				// person looking at it, and the hash is what makes it
				// unique.
				//
				// An empty result means the user has no cache directory,
				// which WithCacheDir reads as nothing to cache — the same as
				// any other unusable entry.
				if base, err := os.UserCacheDir(); err == nil {
					if wd, err := os.Getwd(); err == nil {
						sum := sha256.Sum256([]byte(wd))
						name := hex.EncodeToString(sum[:])[:16]
						// A readable prefix, when there is one to read: "/"
						// and "." have no base worth showing, and the hash
						// alone is still correct.
						if label := filepath.Base(wd); label != "" && label != "." && label != string(filepath.Separator) {
							name = label + "-" + name
						}
						dir = filepath.Join(base, "tunneld", name)
					}
				}
			}
			if dir == "" {
				continue
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				continue
			}
			if slices.Contains(b.cacheDirs, abs) {
				continue
			}
			b.cacheDirs = append(b.cacheDirs, abs)
		}
	}
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

// WithStdout redirects the help text and the version banner. Build passes it
// to the command's SetOut, so calling SetOut on the built command overrides
// this. Unset, output goes to the process's stdout.
//
// A running tunnel writes nothing there: its addresses go to stderr with the
// rest of what a person reads.
func WithStdout(w io.Writer) Option {
	return func(b *BuilderImpl) { b.stdout = w }
}

// WithStderr redirects the banner, the origin map, and the tunnel's logs.
// Build passes it to the command's SetErr, so calling SetErr on the built
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

// Build assembles the configured command. It is the terminal step; the command
// is built once and cached, so repeated calls return the same *cobra.Command
// rather than a second one with a second set of flags bound to these fields.
//
// command is the one-shot assembly behind Build.
func (b *BuilderImpl) Build() *cobra.Command {
	b.builtOnce.Do(func() {
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
		if b.cacheDirs == nil {
			WithCacheDir("")(b)
		}
		cmd.Flags().Var(&cacheDirValue{b: b}, "cache-dir",
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

		b.built = cmd
	})
	return b.built
}

// cacheDirValue binds --cache-dir onto WithCacheDir, so the flag, its
// environment mirror and the Go setter resolve by one rule instead of three.
// pflag's own stringArray would store the raw strings and leave "." and "true"
// for whoever read them next to make sense of.
type cacheDirValue struct {
	b *BuilderImpl
	// changed marks the first value the command line supplied. Until then the
	// slice still holds whatever WithCacheDir seeded, and the first --cache-dir
	// clears it: a command line replaces a seeded set rather than merging into
	// one, which is the rule pflag's own stringArray applies to --url.
	changed bool
}

func (v *cacheDirValue) Type() string   { return "stringArray" }
func (v *cacheDirValue) String() string { return "[" + strings.Join(v.b.cacheDirs, ",") + "]" }

func (v *cacheDirValue) Set(s string) error {
	if !v.changed {
		v.b.cacheDirs, v.changed = nil, true
	}
	WithCacheDir(s)(v.b)
	return nil
}

// Append, GetSlice and Replace are pflag.SliceValue, which is how the
// environment binding in Build hands a whole comma-separated variable over
// at once. Replace clears first, for the same reason Set does on its first
// call.
func (v *cacheDirValue) Append(s string) error { return v.Set(s) }
func (v *cacheDirValue) GetSlice() []string    { return v.b.cacheDirs }

func (v *cacheDirValue) Replace(dirs []string) error {
	v.b.cacheDirs, v.changed = nil, true
	WithCacheDir(dirs...)(v.b)
	return nil
}
