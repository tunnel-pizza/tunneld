// Package cachedir is the --cache-dir list: what an entry means — a path,
// or true and false as instructions — how entries are resolved and
// deduplicated, and the pflag value that binds the flag onto the list.
package cachedir

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Option configures a ValueImpl at construction.
type Option = v1.Option[*ValueImpl]

// ValueImpl is the default CacheDirs implementation: a pflag.Value and
// pflag.SliceValue that binds --cache-dir directly onto the list, so the
// flag, its environment mirror, and Add resolve entries by one rule instead
// of three. pflag's own stringArray would store the raw strings and leave
// "." and "true" for whoever read them next to make sense of.
type ValueImpl struct {
	// dirs distinguishes nil from empty, and that is the whole of the cache
	// switch's state. Nil is unset, and Command fills it with the working
	// directory. Empty but not nil is a list a false entry emptied on
	// purpose, which Command leaves alone and Add will not add to. A later
	// source starts over by setting the field back to nil.
	dirs []string
	// changed marks the first value the command line supplied. Until then the
	// slice still holds whatever Add seeded, and the first --cache-dir clears
	// it: a command line replaces a seeded set rather than merging into one,
	// which is the rule origins follow as arguments.
	changed bool
}

// New returns an empty ValueImpl, configured by opts.
func New(opts ...Option) *ValueImpl {
	return v1.Apply(&ValueImpl{}, opts...)
}

// Add adds directories to cache tunnel specs in, in order, appending across
// calls.
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
func (v *ValueImpl) Add(dirs ...string) {
	// A list that exists and is empty is one a false entry emptied, and
	// nothing refills it: off holds until a later source sets the field
	// back to nil and starts over.
	if v.dirs != nil && len(v.dirs) == 0 {
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
				v.dirs = []string{}
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
			// which Add reads as nothing to cache — the same as any
			// other unusable entry.
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
		if slices.Contains(v.dirs, abs) {
			continue
		}
		v.dirs = append(v.dirs, abs)
	}
}

func (v *ValueImpl) Type() string   { return "stringArray" }
func (v *ValueImpl) String() string { return "[" + strings.Join(v.dirs, ",") + "]" }

func (v *ValueImpl) Set(s string) error {
	if !v.changed {
		v.dirs, v.changed = nil, true
	}
	v.Add(s)
	return nil
}

// Append, GetSlice and Replace are pflag.SliceValue, which is how the
// environment binding in Command hands a whole comma-separated variable over
// at once. Replace clears first, for the same reason Set does on its first
// call.
func (v *ValueImpl) Append(s string) error { return v.Set(s) }

// GetSlice returns dirs as is, so nil (unset) and empty (turned off) stay
// distinguishable to a caller.
func (v *ValueImpl) GetSlice() []string { return v.dirs }

func (v *ValueImpl) Replace(dirs []string) error {
	v.dirs, v.changed = nil, true
	v.Add(dirs...)
	return nil
}

var _ pflag.Value = (*ValueImpl)(nil)
var _ pflag.SliceValue = (*ValueImpl)(nil)
