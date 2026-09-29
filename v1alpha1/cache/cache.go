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
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

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

// Option configures a CacheImpl, at construction or when Load or Save is
// called.
type Option = v1.Option[*CacheImpl]

// discard is where a cache with no logger writes: nowhere.
var discard = slog.New(slog.DiscardHandler)

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

	// What follows is one run's, set by the options below — given to New, or
	// to Load and Save, which apply them to the cache itself: what a call
	// sets stays, so a later call that names no origins — the router's,
	// serving the run's spec — reads the same run's file.
	//
	// origins is the run whose file this is: its key names the file.
	origins v1.Origins
	// spec is what Save writes: the envelope the running tunnel serializes.
	spec string
	// tracking is what Save writes beside the spec: the knobs the run
	// settled on, keyed by the variable that names each.
	tracking map[string]string
	// secret is the running tunnel's secret: whoever holds it can run the
	// tunnel. Kept in memory only, never written: the file's spec already
	// carries it, inside its envelope.
	secret []byte
	log    v1.Logger

	// mu is held by Load and Save for the whole call: the router loads from
	// its own goroutine while the run saves from another.
	mu sync.Mutex
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
	c := &CacheImpl{log: discard}
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

// WithOrigins sets the run whose file Load reads and Save writes: its key
// names the file.
func WithOrigins(origins v1.Origins) Option {
	return func(c *CacheImpl) { c.origins = origins }
}

// WithSpec sets the spec Save writes: the envelope the running tunnel
// serializes once it is up.
func WithSpec(spec string) Option {
	return func(c *CacheImpl) { c.spec = spec }
}

// WithTracking sets what Save writes beside the spec: what the run settled
// on, keyed by the variable that names each knob.
func WithTracking(tracking map[string]string) Option {
	return func(c *CacheImpl) { c.tracking = tracking }
}

// WithSecret sets the running tunnel's secret. The cache keeps it in memory
// and never writes it.
func WithSecret(secret []byte) Option {
	return func(c *CacheImpl) { c.secret = secret }
}

// Key is the key of the run the cache would read or write for now — the name
// of its file, without the extension — or "" before it has been given
// origins. Held under the cache's lock, since Save may be changing them.
func (c *CacheImpl) Key() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.origins == nil {
		return ""
	}
	return c.origins.Key()
}

// Secret is the tunnel secret the cache was last given, nil before then.
// Held under the cache's lock, since a request may ask while the run saves.
func (c *CacheImpl) Secret() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.secret
}

// WithLog sets where the cache says what it did. Nil keeps the one it has.
func WithLog(log v1.Logger) Option {
	return func(c *CacheImpl) {
		if log != nil {
			c.log = log
		}
	}
}

// Origins, Spec and Tracking read back what the options set, for a caller
// standing in for a cache that wants to see what it was handed without
// touching a disk.
func (c *CacheImpl) Origins() v1.Origins         { return c.origins }
func (c *CacheImpl) Spec() string                { return c.spec }
func (c *CacheImpl) Tracking() map[string]string { return c.tracking }

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
// opts are applied to the cache itself, as Display's Open applies its own:
// what they set stays for the next call.
//
// Nothing here fails a tunnel. An unreadable or malformed file costs the
// hostname continuity it would have provided, and a fresh mint is the correct
// behaviour without it.
func (c *CacheImpl) Load(opts ...Option) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	v1.Apply(c, opts...)
	log := c.log
	path := c.path(c.origins)
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
// The spec, set by WithSpec, is what the tunnel serializes once it is up, and
// it has to be asked for then rather than being the one that was replayed. Nothing has to fail
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
// opts are applied to the cache itself, as Load's are.
//
// Nothing here fails a tunnel either. The tunnel is up and serving whether or
// not the next run gets a head start.
func (c *CacheImpl) Save(opts ...Option) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v1.Apply(c, opts...)
	log := c.log
	body := c.render()
	if body == "" {
		log.Debug("nothing to cache: the tunnel has no spec to give")
		return
	}

	path := c.path(c.origins)
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
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		log.Warn("could not cache the tunnel", "path", path, "error", err)
		return
	}
	log.Info("cached the tunnel", "path", path)
}

// String is the cache file for what the options set: the spec's line, then
// what the run settled on, one NAME='value' per line. After a Save, that is
// the file Save wrote, since Save's options stay on the cache — which is what
// the router serves as a remote copy of it. Empty when there is no spec: a
// file without the one line that does something is not a cache file.
func (c *CacheImpl) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.render()
}

// render is String for a caller already holding mu: Save.
func (c *CacheImpl) render() string {
	if c.spec == "" {
		return ""
	}
	lines := []string{assign(ltv1.SpecEnv, c.spec)}

	// Everything the run settled on, after the one line that does something.
	// Sorted, so the same run twice writes the same file and a diff between
	// two of them is about the runs rather than about map iteration.
	names := make([]string, 0, len(c.tracking))
	for name, value := range c.tracking {
		// A knob nobody set says nothing about the run, and a file of empty
		// variables is one nobody reads twice.
		if value != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		lines = append(lines, assign(name, c.tracking[name]))
	}
	return strings.Join(lines, "\n") + "\n"
}
