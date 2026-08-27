package v1alpha1

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	v1 "github.com/cnuss/golib/v1"
)

// EnvBool reads an env-fixed boolean knob. The three results carry the whole
// contract: value is the parsed override, fixed reports whether name is set at
// all (a set value overrides code — env beats code), and err carries an
// unparsable value for the caller to surface where the knob takes effect.
//
// Splitting "is it set" from "what is it" is what makes an explicit false
// override distinguishable from an absent one, which a bare (bool, error)
// cannot express. Returning err rather than swallowing it is the other half:
// an override the operator typo'd must fail loudly, because silently using the
// code value looks identical to the override having worked.
//
// An empty value reads as unset. err wraps v1.ErrInvalidEnv, so a caller can
// match the class with errors.Is while the message still names the variable
// and the offending value.
func EnvBool(name string) (value, fixed bool, err error) {
	env, ok := os.LookupEnv(name)
	if !ok || env == "" {
		return false, false, nil
	}
	v, err := strconv.ParseBool(env)
	if err != nil {
		return false, true, fmt.Errorf("%s=%q: %w: %w", name, env, v1.ErrInvalidEnv, err)
	}
	return v, true, nil
}

// EnvDuration reads an env-fixed duration knob, the sibling of EnvBool and
// bound by the same contract: fixed reports whether name is set, and err
// carries an unparsable value instead of swallowing it. The value uses
// time.ParseDuration syntax (e.g. "1s", "500ms").
func EnvDuration(name string) (value time.Duration, fixed bool, err error) {
	env, ok := os.LookupEnv(name)
	if !ok || env == "" {
		return 0, false, nil
	}
	v, err := time.ParseDuration(env)
	if err != nil {
		return 0, true, fmt.Errorf("%s=%q: %w: %w", name, env, v1.ErrInvalidEnv, err)
	}
	return v, true, nil
}

// Logger returns the default logger: silent unless v1.LogEnv (GOLIB_LOG) names
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
