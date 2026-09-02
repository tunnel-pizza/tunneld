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

// Load reads the first TUNNEL.env found in dirs into this process's
// environment, and stops there. Later directories are fallbacks, not layers:
// two files would raise the question of which tunnel is being resumed, and
// there is no useful answer.
//
// A variable already set is left alone. The file is a cache and the
// environment is an instruction, so an operator who exported LIBTUNNEL_SPEC
// deliberately keeps it.
//
// Nothing here fails a tunnel. An unreadable or malformed file costs the
// hostname continuity it would have provided, and a fresh mint is the correct
// behaviour without it.
func Load(cacheDirs []string, log v1.Logger) {
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
			return
		}

		for _, key := range v.AllKeys() {
			// viper lower-cases the keys it parses; an environment variable
			// is upper by convention and LIBTUNNEL_SPEC is by contract.
			name := strings.ToUpper(key)
			if _, ok := os.LookupEnv(name); ok {
				continue
			}
			if err := os.Setenv(name, v.GetString(key)); err != nil {
				log.Warn("could not apply the tunnel cache", "var", name, "error", err)
			}
		}
		log.Info("resumed a tunnel from cache", "path", path)
		return
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
// The spec is read from the environment rather than from the tunnel. libtunnel
// exports a freshly minted one there itself, and it is the same envelope
// Serialize returns — while asking the backend's provider for it again would
// mint a second tunnel, because a spec this process exported reads as absent
// to the adopter that would otherwise replay it.
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
