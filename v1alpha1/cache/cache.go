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
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	ltv1 "github.com/cnuss/libtunnel/v1"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
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

// Cache persists a tunnel's spec between runs, filed under the name the
// origins give it.
//
// Save takes what the run settled on as well, keyed by the variable that names
// each knob. It is written beside the spec and never read back: a file whose
// name is a hash otherwise says nothing about the run that wrote it, and a
// cache that fed configuration back into the next run would pin a choice made
// once into every run afterwards.
//
// Both take the run's facts as the options they are given, as Route and Open
// do: the origins whose key names the file, and for Save the spec and the
// tracking to write, and the run's logger. String is the file as the cache
// last saved it, "" before then. Handlers is what the cache answers under a
// path, by ServeMux pattern — the router hands it its control path — and the
// default cache's .env there is String, served as a remote copy. Secret is the running tunnel's secret the run last saved with, nil before
// then: what the router authorizes its control path against. Key is the key
// of the run the cache is for, "" before it knows: what the router names the
// run by on every answer from its control path, refusals included. Spec is
// every spec the cache takes, the run's own saves among them: a new one while
// the run waits is a new tunnel. Grant is whether a bearer token is a live,
// unused grant the cache issued on GET .env, for the router to let that one
// PATCH through. Mutable is what the file said about a variable a PATCH may
// change, set says whether it said anything at all (an empty line is a
// choice); SetMutable records what the builder settled, for the file.
type Cache interface {
	Load(opts ...Option) string
	Save(opts ...Option)
	String() string
	Handlers(path string) map[string]func(http.ResponseWriter, *http.Request)
	Secret() []byte
	Key() string
	Spec() <-chan string
	Grant(bearer string) bool
	Mutable(name string) (string, bool)
	SetMutable(name, value string)
}

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

	// specs is every spec the cache takes, as Spec hands them on: each
	// WithSpec, and each LIBTUNNEL_SPEC a PATCH sends. Room for one waiting,
	// and nobody blocks on it: WithSpec replaces one still waiting with its
	// own, the latest being the one that matters, and a PATCH that finds one
	// waiting is refused rather than dropped.
	specs chan string
	// applying is a PATCHed spec handed to the run and not yet settled by a
	// save (WithSpec after a respec, WithSavedSpec after a messages-only
	// update, or a re-sent spec the run already has). Another spec PATCH
	// meanwhile is refused like one finding a spec waiting: the settling
	// save's echo would replace it, after it was answered 200.
	applying bool

	// mutable is what a PATCH, Load or SetMutable set, apart from tracking so
	// that a respec's save, which replaces tracking wholesale, cannot erase it.
	// A name present with "" is a choice of empty (public), written as a bare
	// line; absent is never chosen.
	mutable map[string]string
	// mutables is how each variable is checked and applied, by name.
	mutables map[string]mutableVar
	// grants is every outstanding grant, by SHA-256 of the token; grantOrder
	// is issue order, for forgetting the oldest.
	grants     map[[32]byte]grant
	grantOrder [][32]byte
	now        func() time.Time
}

// mutableVar is one variable a PATCH may change: a pure check, then a commit
// that cannot fail once the check has passed.
type mutableVar struct {
	validate func(string) error
	apply    func(string)
}

// grant is what a single-use grant may do, and until when.
type grant struct {
	vars []string
	exp  time.Time
}

// A grant lasts a minute (long enough to hash a password and send it) and at
// most 32 are outstanding, since every GET issues one and nobody may use it.
const (
	grantLife = time.Minute
	grantsMax = 32
)

// New returns a CacheImpl configured by opts, pointed at the user's cache
// directory.
//
// Never the working directory, which is what an entry in the old list could
// name. A spec is credentials, the working directory is usually a repository,
// and no filename avoids being committed there: measured against GitHub's 239
// gitignore templates and 752 real ones, the best a name managed was 13% and
// 26%.
func New(opts ...Option) *CacheImpl {
	c := &CacheImpl{
		log:      discard,
		specs:    make(chan string, 1),
		mutable:  map[string]string{},
		mutables: map[string]mutableVar{},
		grants:   map[[32]byte]grant{},
		now:      time.Now,
	}
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
// serializes once it is up. It is handed on to Spec as well, in place of
// one still waiting there.
func WithSpec(spec string) Option {
	return func(c *CacheImpl) {
		c.spec, c.applying = spec, false
		select {
		case <-c.specs:
		default:
		}
		select {
		case c.specs <- spec:
		default:
		}
	}
}

// WithSavedSpec sets the spec Save writes without handing it on to Spec: for
// a spec the run already has, and has taken without a reconnect (a
// messages-only update). Unlike WithSpec it leaves a spec waiting on Spec in
// place, since a PATCH that put one there was answered as taken.
func WithSavedSpec(spec string) Option {
	return func(c *CacheImpl) { c.spec, c.applying = spec, false }
}

// WithClock sets what the cache reads the time from, for a grant's minute.
// Nil keeps the one it has (time.Now unless set).
func WithClock(now func() time.Time) Option {
	return func(c *CacheImpl) {
		if now != nil {
			c.now = now
		}
	}
}

// WithTracking sets what Save writes beside the spec: what the run settled
// on, keyed by the variable that names each knob.
func WithTracking(tracking map[string]string) Option {
	return func(c *CacheImpl) { c.tracking = tracking }
}

// WithSecret sets the running tunnel's secret. The cache keeps it in memory
// and never writes it.
func WithSecret(secret []byte) Option {
	return func(c *CacheImpl) {
		// A new secret is a new tunnel: every grant the old one issued goes.
		if !bytes.Equal(c.secret, secret) {
			c.grants, c.grantOrder = map[[32]byte]grant{}, nil
		}
		c.secret = secret
	}
}

// WithMutable registers how name, one of MUTABLE_VARS other than the spec,
// is checked and applied: validate is pure; apply commits and cannot fail
// once validate has passed. A PATCH naming a variable with nothing
// registered is refused rather than stored unapplied.
func WithMutable(name string, validate func(string) error, apply func(string)) Option {
	return func(c *CacheImpl) { c.mutables[name] = mutableVar{validate, apply} }
}

// Mutable is name's value and whether one was set at all, "" included.
func (c *CacheImpl) Mutable(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.mutable[name]
	return v, ok
}

// SetMutable records name's value for the file, applying nothing: the
// builder applies what it settled itself.
func (c *CacheImpl) SetMutable(name, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mutable[name] = value
}

// Grant reports whether bearer is a live, unused grant. It does not use it:
// the PATCH it authorizes does, once applied.
func (c *CacheImpl) Grant(bearer string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.liveGrant(bearer)
	return ok
}

// liveGrant finds bearer's grant, dropping it if expired; callers hold c.mu.
func (c *CacheImpl) liveGrant(bearer string) (grant, bool) {
	id := sha256.Sum256([]byte(bearer))
	g, ok := c.grants[id]
	if ok && c.now().After(g.exp) {
		delete(c.grants, id)
		return grant{}, false
	}
	return g, ok
}

// issueGrant mints one; callers hold c.mu. Only its hash is kept, so a dump
// of the map holds nothing usable.
func (c *CacheImpl) issueGrant() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	var vars []string
	for name := range MUTABLE_VARS {
		if name != ltv1.SpecEnv {
			vars = append(vars, name)
		}
	}
	slices.Sort(vars)
	id := sha256.Sum256([]byte(token))
	c.grants[id] = grant{vars: vars, exp: c.now().Add(grantLife)}
	c.grantOrder = append(c.grantOrder, id)
	for len(c.grantOrder) > grantsMax {
		delete(c.grants, c.grantOrder[0])
		c.grantOrder = c.grantOrder[1:]
	}
	return token
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

// Origins and Tracking read back what the options set, for a caller standing
// in for a cache that wants to see what it was handed without touching a disk.
func (c *CacheImpl) Origins() v1.Origins         { return c.origins }
func (c *CacheImpl) Tracking() map[string]string { return c.tracking }

// Spec is every spec the cache takes, as it takes them: each WithSpec — so
// each Save given one — and each LIBTUNNEL_SPEC a PATCH to .env sends. It
// holds one at a time, and nothing is waiting on it until something reads.
func (c *CacheImpl) Spec() <-chan string { return c.specs }

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
	// The mutables the file holds, present-and-empty included: a bare line is
	// a choice of public, distinct from a file that never named the variable.
	for name := range MUTABLE_VARS {
		if name == ltv1.SpecEnv {
			continue
		}
		if key := strings.ToLower(name); v.InConfig(key) {
			c.mutable[name] = v.GetString(key)
		}
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
	c.save()
}

// save is Save for a caller already holding c.mu: a PATCH, which validates,
// applies and saves under one hold of the lock.
func (c *CacheImpl) save() {
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

// Handlers is what the cache answers under path — the router's control path,
// which the router owns — by the ServeMux pattern each is registered under
// there. The router guards them and names the run on every answer; what each
// says is the cache's.
//
//	path+".env"   the cache file (dotenvHandler)
func (c *CacheImpl) Handlers(path string) map[string]func(http.ResponseWriter, *http.Request) {
	return map[string]func(http.ResponseWriter, *http.Request){
		path + ".env": c.dotenvHandler(),
	}
}

// logger is the one Load or Save was last given, read under the lock they set
// it under: a request asks from its own goroutine.
func (c *CacheImpl) logger() v1.Logger {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.log
}

// dotenvHandler answers GET with the cache file as the run last saved it,
// 404 when nothing is saved yet, and a single-use grant (v1.GrantHeader) the
// caller may hand the owner's browser. PATCH reads its body as variables
// (parseDotenv), refusing one that does not parse, 400, or is too big, 413,
// and keeps only MUTABLE_VARS. Then, under the cache's lock, it is
// all-or-nothing: a grant naming a variable outside its own is a 403, a
// variable nobody registered or that fails its check a 400, and a spec while
// one is still waiting a 429 asking to be tried again in a second; only then
// is every variable applied and saved and the spec handed on to Spec. With
// the secret the answer is a 200 carrying the file, as GET would; with a
// grant, which is used up once something was applied, a 204 and never the
// file, which holds the spec (the router says the gate on it). GET answers an ETag naming the file, and a PATCH with If-Match
// that names another is a 412 that applies nothing. Any other method is a
// 405 naming GET.
//
// No answer is stored anywhere on the way: the file changes with every save,
// and a stale copy of a credential is one more place it lives.
func (c *CacheImpl) dotenvHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// no-transform: a hop that compresses would also weaken the ETag,
		// and If-Match compares strongly.
		w.Header().Set("Cache-Control", "no-store, no-transform")
		switch r.Method {
		case http.MethodGet:
			// Every read by the secret's holder carries a grant: what that
			// holder may hand the owner's browser so it can PATCH this file
			// once, without ever holding the secret itself.
			c.mu.Lock()
			saved := c.served()
			var token string
			if saved != "" {
				token = c.issueGrant()
			}
			c.mu.Unlock()
			if saved == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set(v1.GrantHeader, token)
			w.Header().Set("ETag", etag(saved))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, saved)
		case http.MethodPatch:
			// Every patch says on the log what became of it: it arrives from
			// outside, and a change to the run nobody can see happen is one
			// nobody can account for. Names only, never values: the spec is
			// the hostname's credential.
			log := c.logger()
			vars, err := parseDotenv(http.MaxBytesReader(w, r.Body, maxDotenv))
			if err != nil {
				if tooBig := new(http.MaxBytesError); errors.As(err, &tooBig) {
					log.Info("refused a patch to .env", "reason", "the body is too big", "limit", maxDotenv)
					http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
					return
				}
				log.Info("refused a patch to .env", "reason", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// From here to the answer under one hold of the lock, so two
			// PATCHes cannot interleave: everything is validated before
			// anything is applied, and the spec slot checked here is the slot
			// written below, since WithSpec only runs inside Save, under it.
			c.mu.Lock()
			defer c.mu.Unlock()
			// A read-then-write caller (tunnel.pizza's messages sync) names the
			// file it read; if a respec replaced it in between, writing back
			// what was read would send the old credential as a new spec.
			// Every field line: a list may come split over several.
			if want := strings.Join(r.Header.Values("If-Match"), ","); want != "" && !ifMatch(want, c.served()) {
				log.Info("refused a patch to .env", "reason", "the file changed since it was read")
				http.Error(w, "the file changed since it was read", http.StatusPreconditionFailed)
				return
			}
			bearer, granted := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			var g grant
			if granted {
				var ok bool
				if g, ok = c.liveGrant(bearer); !ok {
					// Expired or used between authorize and here.
					auth.Unauthorized(w, "")
					return
				}
				for _, name := range slices.Sorted(maps.Keys(vars)) {
					if !slices.Contains(g.vars, name) {
						log.Info("refused a patch to .env", "reason", "outside the grant", "name", name)
						http.Error(w, name+" is not this grant's to change", http.StatusForbidden)
						return
					}
				}
			}
			// Only what a caller may change: the rest of the file is the
			// run's to say.
			var ignored []string
			maps.DeleteFunc(vars, func(name, _ string) bool {
				if !MUTABLE_VARS[name] {
					ignored = append(ignored, name)
				}
				return !MUTABLE_VARS[name]
			})
			slices.Sort(ignored)
			spec, hasSpec := vars[ltv1.SpecEnv]
			for _, name := range slices.Sorted(maps.Keys(vars)) {
				if name == ltv1.SpecEnv {
					continue
				}
				m, ok := c.mutables[name]
				if !ok {
					log.Info("refused a patch to .env", "reason", "not applied by this run", "name", name)
					http.Error(w, name+": this run does not apply it", http.StatusBadRequest)
					return
				}
				if err := m.validate(vars[name]); err != nil {
					log.Info("refused a patch to .env", "reason", "invalid", "name", name)
					http.Error(w, name+": "+err.Error(), http.StatusBadRequest)
					return
				}
			}
			if hasSpec && (len(c.specs) == cap(c.specs) || c.applying) {
				log.Info("refused a patch to .env", "reason", "a patch is already being applied")
				w.Header().Set("Retry-After", "1")
				http.Error(w, "a patch is already being applied", http.StatusTooManyRequests)
				return
			}
			// Apply: nothing below can fail.
			applied := false
			for name, value := range vars {
				if name == ltv1.SpecEnv {
					continue
				}
				c.mutables[name].apply(value)
				c.mutable[name] = value
				applied = true
			}
			if applied {
				c.save()
			}
			if hasSpec {
				c.specs <- spec
				c.applying = true
			}
			switch {
			case hasSpec:
				log.Info("a patch to .env brought a spec; handing it to the run", "names", slices.Sorted(maps.Keys(vars)), "ignored", ignored)
			case applied:
				log.Info("a patch to .env applied", "names", slices.Sorted(maps.Keys(vars)), "ignored", ignored)
			default:
				log.Info("a patch to .env changed nothing", "ignored", ignored)
			}
			if granted {
				if applied {
					delete(c.grants, sha256.Sum256([]byte(bearer)))
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, c.served())
		default:
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// etag names a file by its content, for If-Match.
func etag(file string) string {
	sum := sha256.Sum256([]byte(file))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`
}

// ifMatch reports whether an If-Match header names file: "*" for any file
// there is (RFC 9110), or a list holding its tag. A tag that comes back
// weakened (W/) still matches: a hop that compressed the response weakens
// it, and it is still this server's own name for the same bytes.
func ifMatch(header, file string) bool {
	if strings.TrimSpace(header) == "*" {
		return file != ""
	}
	want := etag(file)
	for tag := range strings.SplitSeq(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(tag), "W/") == want {
			return true
		}
	}
	return false
}

// maxDotenv is the most a PATCH body may be: a cache file is one spec and a
// handful of knobs, a few kilobytes, so this is room to spare and no more.
const maxDotenv = 64 << 10

// MUTABLE_VARS is every variable a PATCH may change: the handler keeps these
// from what parseDotenv read and drops the rest.
var MUTABLE_VARS = map[string]bool{ltv1.SpecEnv: true, v1.WWWAuthenticateEnv: true}

// dotenvName is what a variable may be called: a letter or an underscore, then
// letters, digits and underscores — a name a shell can export.
var dotenvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseDotenv reads body as a file of variables, one NAME=value a line, the
// way the cache writes them: blank lines and # comments are skipped, a line
// may end \r\n (bufio.ScanLines drops the \r), and one pair of matching quotes
// around a value, ' or ", is taken off — so what GET answers is a body PATCH
// takes, but for the password: served with its salt and hash redacted, its
// pw is no PHC string, and its validation refuses it.
//
// Sanitized rather than trusted, since it arrives over the network: a line
// with no '=', a name a shell could not export, a name given twice, and a value
// that is not UTF-8 or carries a control character are each refused, naming
// the line. Nothing past the first refusal is read.
func parseDotenv(body io.Reader) (map[string]string, error) {
	vars := map[string]string{}
	sc := bufio.NewScanner(body)
	// A line one byte longer than the body may be, so a body too big is
	// always refused as one, and never as a line too long.
	sc.Buffer(nil, maxDotenv+1)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: no '=' in it", n)
		}
		if !dotenvName.MatchString(name) {
			return nil, fmt.Errorf("line %d: %q is not a variable name", n, name)
		}
		if _, twice := vars[name]; twice {
			return nil, fmt.Errorf("line %d: %s is set twice", n, name)
		}
		if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("line %d: %s is not UTF-8", n, name)
		}
		if strings.ContainsFunc(value, unicode.IsControl) {
			return nil, fmt.Errorf("line %d: %s holds a control character", n, name)
		}
		vars[name] = value
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return vars, nil
}

// render is String for a caller already holding mu: Save.
func (c *CacheImpl) render() string { return c.renderFor(false) }

// served is the file as .env hands it out: the password redacted.
// Callers hold mu.
func (c *CacheImpl) served() string { return c.renderFor(true) }

// renderFor writes the file, as saved or as served.
func (c *CacheImpl) renderFor(served bool) string {
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
		if _, mut := c.mutable[name]; mut {
			continue
		}
		lines = append(lines, assign(name, c.tracking[name]))
	}
	// What a PATCH or the operator set, after the run's knobs: an empty one
	// as a bare line, since present-and-empty is a choice (public) that a
	// file without the line could not tell from never having chosen.
	for _, name := range slices.Sorted(maps.Keys(c.mutable)) {
		value := c.mutable[name]
		// Served, the password says its challenges with pw's salt and hash
		// redacted: those stay in the file on disk, so a restart is
		// protected, and never go to whoever reads .env, tunnel.pizza
		// included. A value that does not parse says only that it is set.
		if served && name == v1.WWWAuthenticateEnv && value != "" {
			public := "(set)"
			if cs, err := auth.Parse(value); err == nil {
				out := make([]string, len(cs))
				for i, ch := range cs {
					out[i] = ch.Redacted()
				}
				public = strings.Join(out, ", ")
			}
			value = public
		}
		if value == "" {
			lines = append(lines, name+"=")
		} else {
			lines = append(lines, assign(name, value))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
