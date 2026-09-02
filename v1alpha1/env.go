package v1alpha1

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	v1 "github.com/tunnel-pizza/tunneld/v1"
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

// newEnv returns the environment binding for one command's flags.
//
// The instance is per-builder, never viper's package global: two commands in
// one process — a host program's and an embedded tunneld's — would otherwise
// share one key space, and so would two tests in one binary.
func newEnv() *viper.Viper {
	v := viper.New()
	for flag, env := range flagEnv {
		// BindEnv only errors when given no name at all, which the registry
		// above cannot produce.
		_ = v.BindEnv(flag, env)
	}
	return v
}

// applyEnv copies environment values onto the flags the command line did not
// set, which is what makes the precedence flag > env > default. It runs from
// PersistentPreRunE, ahead of cobra's required-flag validation, so a flag
// satisfied by its variable counts as supplied.
//
// A value that the flag refuses is an error wrapping v1.ErrInvalidEnv, naming
// the variable and the offending value: env beats code, so a typo'd override
// that silently fell back would be indistinguishable from one that worked.
func applyEnv(cmd *cobra.Command, v *viper.Viper) error {
	var err error
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if err != nil || f.Changed || !v.IsSet(f.Name) {
			return
		}
		value := v.GetString(f.Name)

		// A repeatable flag takes the whole list at once. Replace, not
		// Append: the flag's default may be a seeded value, and the
		// environment overrides a seed rather than extending it — the same
		// rule pflag's stringArray applies to the command line.
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			err = slice.Replace(splitEnvList(value))
		} else {
			err = f.Value.Set(value)
		}
		if err != nil {
			err = fmt.Errorf("%s=%q: %w: %w", flagEnv[f.Name], value, v1.ErrInvalidEnv, err)
			return
		}
		// Marking it changed is what stops cobra from reporting a required
		// flag as missing when its variable supplied it.
		f.Changed = true
	})
	return err
}

// splitEnvList parses a list-valued variable: comma-separated, surrounding
// space trimmed, empty entries dropped so a trailing comma is not an origin.
func splitEnvList(value string) []string {
	items := make([]string, 0, strings.Count(value, ",")+1)
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}
