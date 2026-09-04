// Package env carries a tunnel's identity between runs.
//
// libtunnel mints a fresh hostname on every start unless it is handed the spec
// of a tunnel it already has, and that handoff channel is an environment
// variable. So persisting one is all a cache has to be: a file of variables,
// written when a tunnel comes up and read back into the environment before the
// next one is built.
//
// The library used to keep this file itself and no longer does
// (cnuss/libtunnel#167). Where it lands is a deployment's decision rather than
// a library's — a volume in a container, a working directory on a laptop —
// which is why the directories arrive from --cache-dir rather than being
// derived here.
package env

import (
	"os"
	"path/filepath"
	"strings"

	ltv1 "github.com/cnuss/libtunnel/v1"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// File is the name looked for in each cache directory, and written to.
const File = "TUNNEL.env"

// saved is what a run persists: the spec libtunnel adopts on the next start,
// and the hostname as a plain-text mirror for anyone reading the file. Only
// the spec is load-bearing — libtunnel never adopts the hostname — but a cache
// file nobody can read is a cache file nobody trusts.
//
// An allowlist rather than every LIBTUNNEL_ variable in the environment: the
// rest are an operator's configuration, and a cache that captured them would
// pin choices they made once into every run afterwards.
var saved = []string{ltv1.SpecEnv, ltv1.HostnameEnv}

// Cached returns the spec envelope from the first TUNNEL.env found in dirs,
// and stops there. Later directories are fallbacks, not layers: two files
// would raise the question of which tunnel is being resumed, and there is no
// useful answer.
//
// It returns the envelope rather than setting LIBTUNNEL_SPEC, which is what
// this used to do. That variable is the parent-to-child handoff channel, where
// the parent's tunnel is live by construction, so libtunnel adopts it pinned
// and asks the provider nothing. A cached spec is the opposite case — the
// tunnel it names may have been reaped hours ago — and it has to go through
// libtunnel.From, where the spec's identity rides the mint request and the
// provider says whether it still exists.
//
// Nothing here fails a tunnel. An unreadable or malformed file costs the
// hostname continuity it would have provided, and a fresh mint is the correct
// behaviour without it.
func Cached(cacheDirs []string, log v1.Logger) string {
	for _, dir := range cacheDirs {
		path := filepath.Join(dir, File)
		if _, err := os.Stat(path); err != nil {
			continue
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
	return ""
}

// Discard removes the cache from every directory holding one, so the next run
// mints instead of replaying.
//
// The caller decides when: a spec the provider refuses outright is dead and
// keeping it would fail every run the same way, while a hostname somebody else
// now holds is not this spec's fault and throwing it away would not win the
// name back.
func Discard(cacheDirs []string, log v1.Logger) {
	for _, dir := range cacheDirs {
		path := filepath.Join(dir, File)
		if err := os.Remove(path); err == nil {
			log.Info("discarded a dead tunnel cache", "path", path)
		} else if !os.IsNotExist(err) {
			log.Warn("could not discard the tunnel cache", "path", path, "error", err)
		}
	}
}

// Save writes the running tunnel's spec to TUNNEL.env in every one of dirs it
// can write to, so the next run resumes this hostname instead of minting a new
// one.
//
// Every directory rather than the first, where Load reads the first and stops.
// The asymmetry is the point: which directories exist is a property of where
// the process is running — a volume that may or may not be mounted, a working
// directory that may or may not be the same one — and writing to all of them
// means the next run resumes from whichever it turns out to have. A directory
// that cannot be written is skipped rather than fatal, for the same reason.
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
func Save(cacheDirs []string, log v1.Logger) {
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

	var written []string
	for _, dir := range cacheDirs {
		// The default cache directory does not exist until the first save, so
		// creating it is part of saving rather than something the caller was
		// asked to arrange. 0700: it holds credentials, and a directory
		// somebody else can list is a directory that has already leaked which
		// projects are on this machine.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Debug("could not make a cache directory", "dir", dir, "error", err)
			continue
		}
		path := filepath.Join(dir, File)
		// 0600: a spec is the credential for a public hostname.
		if err := os.WriteFile(path, body, 0o600); err != nil {
			log.Debug("could not cache the tunnel here", "path", path, "error", err)
			continue
		}
		written = append(written, path)
	}
	if len(written) == 0 {
		log.Warn("could not cache the tunnel in any directory", "dirs", cacheDirs)
		return
	}
	log.Info("cached the tunnel", "paths", written)
}
