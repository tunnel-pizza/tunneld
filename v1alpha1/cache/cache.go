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
	"slices"
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
//
// Undotted, like every neighbour it sits among: a cache directory is not a
// place anybody browses by accident, so hiding one inside it hides nothing.
const dirName = "tunneld"

// Option configures a CacheImpl at construction.
type Option = v1.Option[*CacheImpl]

// assign is one line of the file: NAME='value'.
//
// Single quotes: the spec is a JSON envelope, so it carries double quotes of
// its own and no shell-style expansion should touch it.
func assign(name, value string) string { return name + "='" + value + "'" }

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

// Save writes the running tunnel's spec under this run's name, so the next run
// of the same thing resumes this hostname instead of minting a new one.
//
// One file, where this used to write one per directory in a list. What a run
// is and where its spec goes are now the same question, so there is nothing
// left to spread across directories and nothing to choose between on the way
// back in.
//
// spec is what the tunnel serializes once it is up, and it has to be asked
// for then rather than being the one that was replayed. Nothing has to fail
// for the two to differ: a reclaim can hold the hostname and replace the
// tunnel behind it, and a reservation that lapsed entirely is adopted on
// whatever hostname was minted in its place — a new name, no error, and a
// stored spec that now points at nothing.
//
// It is the one line that is read back. The file's key for it is the name
// libtunnel gives the same value in an environment, so a file is a thing an
// operator can source as well as a thing Load parses — but this package no
// longer reads that environment itself: what a run has is the tunnel, and the
// tunnel is asked.
//
// Nothing here fails a tunnel either. The tunnel is up and serving whether or
// not the next run gets a head start.
func (c *CacheImpl) Save(origins v1.Origins, spec string, tracking map[string]string, log v1.Logger) {
	if spec == "" {
		log.Debug("nothing to cache: the tunnel has no spec to give")
		return
	}
	lines := []string{assign(ltv1.SpecEnv, spec)}

	// Everything the run settled on, after the one line that does something.
	// Sorted, so the same run twice writes the same file and a diff between
	// two of them is about the runs rather than about map iteration.
	names := make([]string, 0, len(tracking))
	for name, value := range tracking {
		// A knob nobody set says nothing about the run, and a file of empty
		// variables is one nobody reads twice.
		if value != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		lines = append(lines, assign(name, tracking[name]))
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
