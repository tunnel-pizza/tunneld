package v1alpha1

import (
	"log/slog"
	"os"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Logger returns the default logger: silent unless v1.LogEnv (TUNNELD_LOG) names
// a level, in which case it writes to stderr at that level. Silence is the
// default because a library that logs uninvited pollutes its importer's
// output; the environment variable is the operator's way to turn it on without
// a rebuild.
//
// An unrecognized level reads as info and logs a warning naming the bad value —
// a misspelled level should not silence the logs the operator was trying to
// enable. Call it where the logger is used rather than caching it at init, so
// a level set after startup still takes effect.
func Logger() *slog.Logger {
	env, ok := os.LookupEnv(v1.LogEnv)
	if !ok || env == "" {
		return slog.New(slog.DiscardHandler)
	}

	var level slog.Level
	err := level.UnmarshalText([]byte(env))
	if err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err != nil {
		log.Warn("unknown log level, defaulting to info", "var", v1.LogEnv, "value", env)
	}
	return log
}

// flagEnv is the flag → environment variable registry: every flag with an
// env-expressible value, bound to the constant naming it in v1. Explicit
// rather than derived — viper's AutomaticEnv would mangle a name out of each
// flag, which puts the authority over the operator-facing strings in a key
// replacer instead of in v1, where the rest of this package's knobs are
// declared. It also keeps LogEnv spelled TUNNELD_LOG rather than the
// TUNNELD_LOG_LEVEL a derivation would produce.
var flagEnv = map[string]string{
	"url":       v1.URLEnv,
	"provider":  v1.ProviderEnv,
	"cache-dir": v1.CacheDirEnv,
	"log-level": v1.LogEnv,
	"no-open":   v1.NoOpenEnv,
	"multiview": v1.MultiviewEnv,
}
