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

// WithCacheDir adds directories to cache tunnel specs in, in order,
// appending across calls.
//
// An entry that is a boolean is an instruction rather than a path: true names
// the default location, false names nothing at all. That is what lets one
// field take both a switch and a list — "on" is what an operator means by
// setting the variable to true, and defaultCacheDir is the answer that needs
// no further configuration. An empty entry reads as true, since
// nothing else it could mean is useful.
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
func (b *BuilderImpl) WithCacheDir(dirs ...string) v1.Builder {
	// A list that exists and is empty is one a false entry emptied, and
	// nothing refills it: off holds until a later source sets the field back
	// to nil and starts over.
	if b.cacheDirs != nil && len(b.cacheDirs) == 0 {
		return b
	}
	for _, dir := range dirs {
		if on, ok := boolish(dir); ok {
			if !on {
				b.cacheDirs = []string{}
				return b
			}
			dir = ""
		}
		if dir == "" {
			dir = defaultCacheDir()
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

// WithOpen sets whether a public URL is opened in a browser once the tunnel is
// live. It seeds the default of --no-open, which reads inverted: WithOpen(false)
// makes --no-open default to true.
func (b *BuilderImpl) WithOpen(open bool) v1.Builder {
	b.open = open
	return b
}

// WithMultiview sets whether several origins are also served together as one
// panel of framed views.
func (b *BuilderImpl) WithMultiview(multiview bool) v1.Builder {
	b.multiview = multiview
	return b
}

// WithStdout redirects the help text and the version banner. A running
// tunnel writes nothing to stdout.
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
	env := newEnv()

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
			return applyEnv(cmd, env)
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
	// Unset, specs cache into defaultCacheDir. Seeded here rather than in New
	// so that an explicit WithCacheDir replaces the default instead of
	// appending to it: a caller naming a directory means that directory, not
	// that one and wherever the process happened to start.
	//
	// Nil, not empty: an empty list is one a false entry emptied, and seeding
	// over it would re-enable what an operator turned off.
	if b.cacheDirs == nil {
		b.WithCacheDir("")
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
	v.b.WithCacheDir(s)
	return nil
}

// Append, GetSlice and Replace are pflag.SliceValue, which is how applyEnv
// hands a whole comma-separated variable over at once. Replace clears first,
// for the same reason Set does on its first call.
func (v *cacheDirValue) Append(s string) error { return v.Set(s) }
func (v *cacheDirValue) GetSlice() []string    { return v.b.cacheDirs }

func (v *cacheDirValue) Replace(dirs []string) error {
	v.b.cacheDirs, v.changed = nil, true
	v.b.WithCacheDir(dirs...)
	return nil
}

// defaultCacheDir is where a spec goes when nothing says otherwise: a
// per-project directory under the user's cache directory.
//
// Not the working directory, which is what this used to be. A spec is
// credentials, the working directory is usually a repository, and no filename
// avoids being committed there: measured against GitHub's 239 gitignore
// templates and 752 real ones, the best a name managed was 13% and 26%. Nothing
// written into somebody's checkout is safe by default, so nothing is written
// there.
//
// The working directory still decides *which* cache, because two projects on
// one machine are two tunnels. It is fingerprinted rather than mirrored: a path
// cannot be a single path element, and hashing it sidesteps every question
// about separators, length and case. The base name is kept as a prefix so the
// directory is recognisable to a person looking at it, and the hash is what
// makes it unique.
//
// An empty result means the user has no cache directory, which WithCacheDir
// reads as nothing to cache — the same as any other unusable entry.
func defaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(wd))
	name := hex.EncodeToString(sum[:])[:16]
	// A readable prefix, when there is one to read: "/" and "." have no base
	// worth showing, and the hash alone is still correct.
	if label := filepath.Base(wd); label != "" && label != "." && label != string(filepath.Separator) {
		name = label + "-" + name
	}
	return filepath.Join(base, "tunneld", name)
}

// boolish reports whether s is a boolean rather than a path, and which one.
// The spellings are strconv.ParseBool's — 1/t/T/TRUE/true/True and
// 0/f/F/FALSE/false/False — so both halves of the knob are the ones an
// operator would guess. Anything else is a path, including "yes" and "no",
// which ParseBool has never accepted and this should not start accepting on
// its own.
func boolish(s string) (value, ok bool) {
	v, err := strconv.ParseBool(s)
	return v, err == nil
}
