// Package cache carries a tunnel's identity between runs.
//
// libtunnel mints a fresh hostname on every start unless it is told which
// tunnel this is, and what it takes is the spec of one it already had. So
// persisting one is all a cache has to be: a file of variables, written when a
// tunnel comes up and read back on the next start.
//
// The library used to keep this file itself and no longer does
// (cnuss/libtunnel#167). Where it lands is the user's cache directory, and
// what it is called is the run it belongs to: two runs from one project
// serving different things are two tunnels, and before the name carried that
// they shared one file and replayed each other's hostname.
package cache

import (
	"os"
	"path/filepath"
	"strings"

	ltv1 "github.com/cnuss/libtunnel/v1"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// ext is what a cache file is called after its key. The contents are a file of
// environment variables, so the extension says what it is to whoever opens the
// directory.
const ext = ".env"

// dirName is the one directory every cached spec lives in, under the user's
// own cache directory. Flat: what identifies a tunnel is in the filename, so
// nesting would only add a level saying the same thing twice.
const dirName = ".tunneld"

// saved is what a run persists: the spec libtunnel adopts on the next start,
// and the hostname as a plain-text mirror for anyone reading the file. Only
// the spec is load-bearing — libtunnel never adopts the hostname — but a cache
// file nobody can read is a cache file nobody trusts.
//
// An allowlist rather than every LIBTUNNEL_ variable in the environment: the
// rest are an operator's configuration, and a cache that captured them would
// pin choices they made once into every run afterwards.
var saved = []string{ltv1.SpecEnv, ltv1.HostnameEnv}

// Option configures a CacheImpl at construction.
type Option = v1.Option[*CacheImpl]

// CacheImpl is the default cache: one file per tunnel, named for it, under the
// user's cache directory.
type CacheImpl struct {
	// dir is where files go. Empty means this machine has no cache directory
	// and nothing is cached, which is the same answer an unwritable one gives.
	dir string
}

// New returns a CacheImpl configured by opts, pointed at the user's cache
// directory.
//
// Never the working directory, which is what an entry in the old list could
// name. A spec is credentials, the working directory is usually a repository,
// and no filename avoids being committed there: measured against GitHub's 239
// gitignore templates and 752 real ones, the best a name managed was 13% and
// 26%.
func New(opts ...Option) *CacheImpl {
	c := &CacheImpl{}
	if base, err := os.UserCacheDir(); err == nil {
		c.dir = filepath.Join(base, dirName)
	}
	return v1.Apply(c, opts...)
}

// WithDir replaces the directory specs are cached in — a mounted volume in a
// container, a temporary directory in a test. An empty directory turns caching
// off, which is what a machine with no cache directory already gets.
func WithDir(dir string) Option {
	return func(c *CacheImpl) { c.dir = dir }
}

// path is where this run's spec lives: the key, which names the tunnel, under
// the directory, which names nothing.
//
// This package hashes nothing and decides nothing about identity. Whether two
// runs are the same tunnel is the origins' answer; this joins it to a
// directory and adds an extension.
func (c *CacheImpl) path(origins v1.Origins) string {
	if c.dir == "" || origins == nil {
		return ""
	}
	return filepath.Join(c.dir, origins.Key()+ext)
}

// Load returns the spec envelope this run cached, and "" when it has not:
// a run whose origins name no file here has never had a tunnel, whatever other
// runs in the same directory have.
//
// It returns the envelope rather than setting LIBTUNNEL_SPEC, which is what
// this used to do. Both reach libtunnel's one credential chain, where a spec is
// a hint that rides the mint request, but the variable outranks a spec handed
// to From — so writing it here would put a cached tunnel above the live parent
// of a handoff, which is the one case where the caller knows better than the
// cache does.
//
// Nothing here fails a tunnel. An unreadable or malformed file costs the
// hostname continuity it would have provided, and a fresh mint is the correct
// behaviour without it.
func (c *CacheImpl) Load(origins v1.Origins, log v1.Logger) string {
	path := c.path(origins)
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}

	v := viper.New()
	v.SetConfigType("env")
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		log.Warn("could not read the tunnel cache", "path", path, "error", err)
		return ""
	}

	// viper lower-cases the keys it parses.
	spec := v.GetString(strings.ToLower(ltv1.SpecEnv))
	if spec == "" {
		log.Warn("the tunnel cache names no spec", "path", path)
		return ""
	}
	log.Info("resuming a tunnel from cache", "path", path)
	return spec
}

// Discard removes this run's cache, so the next run mints instead of
// replaying.
//
// The caller decides when: a spec the provider refuses outright is dead and
// keeping it would fail every run the same way, while a hostname somebody else
// now holds is not this spec's fault and throwing it away would not win the
// name back.
func (c *CacheImpl) Discard(origins v1.Origins, log v1.Logger) {
	path := c.path(origins)
	if path == "" {
		return
	}
	if err := os.Remove(path); err == nil {
		log.Info("discarded a dead tunnel cache", "path", path)
	} else if !os.IsNotExist(err) {
		log.Warn("could not discard the tunnel cache", "path", path, "error", err)
	}
}

// Save writes the running tunnel's spec under this run's name, so the next run
// of the same thing resumes this hostname instead of minting a new one.
//
// One file, where this used to write one per directory in a list. What a run
// is and where its spec goes are now the same question, so there is nothing
// left to spread across directories and nothing to choose between on the way
// back in.
//
// The spec is read from the environment rather than from the tunnel, and it
// has to be read after the tunnel is up rather than being the one that was
// replayed. Nothing has to fail for the two to differ: a reclaim can hold the
// hostname and replace the tunnel behind it, and a reservation that lapsed
// entirely is adopted on whatever hostname was minted in its place — a new
// name, no error, and a stored spec that now points at nothing. libtunnel
// exports whatever the chain resolved, so the environment is the current one.
//
// Asking the backend's provider for it instead would mint a second tunnel,
// because a spec this process exported reads as absent to the adopter that
// would otherwise replay it.
//
// Nothing here fails a tunnel either. The tunnel is up and serving whether or
// not the next run gets a head start.
func (c *CacheImpl) Save(origins v1.Origins, log v1.Logger) {
	var lines []string
	for _, name := range saved {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			// Single quotes: the spec is a JSON envelope, so it carries double
			// quotes of its own and no shell-style expansion should touch it.
			lines = append(lines, name+"='"+value+"'")
		}
	}
	if len(lines) == 0 {
		log.Debug("nothing to cache: no tunnel spec in the environment")
		return
	}
	body := []byte(strings.Join(lines, "\n") + "\n")

	path := c.path(origins)
	if path == "" {
		log.Debug("nothing to cache into: this machine has no cache directory")
		return
	}
	// The cache directory does not exist until the first save, so creating it
	// is part of saving rather than something the caller was asked to arrange.
	// 0700: it holds credentials, and a directory somebody else can list is a
	// directory that has already leaked which projects are on this machine.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Warn("could not make the cache directory", "dir", filepath.Dir(path), "error", err)
		return
	}
	// 0600: a spec is the credential for a public hostname.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		log.Warn("could not cache the tunnel", "path", path, "error", err)
		return
	}
	log.Info("cached the tunnel", "path", path)
}
