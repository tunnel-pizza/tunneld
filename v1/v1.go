// Package v1 is the stable public surface for golib. The Builder interface and
// Result type here are the contract callers depend on across releases; the
// implementation lives in v1alpha1 and may change between alpha revisions.
//
// Two other things live at this layer because they are equally part of the
// contract: the Err* sentinels callers match with errors.Is, and the *Env
// constants naming every environment knob.
package v1

import "errors"

// The sentinel errors, centralized: declared here with errors.New, wrapped by
// the implementation in v1alpha1, and matched by callers with errors.Is. Two
// disciplines govern this block.
//
// First, an error's doc names the likely cause and the lever — what a caller
// can actually do about it. "Invalid value" tells a caller nothing; naming the
// variable to fix tells them everything.
//
// Second, a sentinel is never deleted. When the condition it reported stops
// existing, the variable stays (marked Deprecated, with the history in its
// doc) so callers matching on it keep compiling. Deleting one turns a
// no-longer-reachable branch in caller code into a build failure — all cost,
// no benefit, since an unreachable errors.Is is simply never true.

// ErrInvalidEnv reports an environment override that is set but unparsable —
// see EnvBool and EnvDuration in v1alpha1. Because env beats code, a typo'd
// value would otherwise fall back to the code value silently and the operator
// would never learn the knob did nothing; this surfaces it instead. The lever
// is the variable named in the wrapped message: correct the value or unset it
// to fall back to the code value deliberately.
var ErrInvalidEnv = errors.New("invalid environment value")

// ErrUnnamed was the Build error of a builder configured without WithName,
// back when a name was mandatory. Names are optional now — an unset name
// builds a Result with an empty Name and no error — so nothing returns this
// any more. It remains so callers matching on it keep compiling.
//
// Deprecated: Build no longer fails on a missing name; nothing returns this.
var ErrUnnamed = errors.New("builder has no name")

// The environment variables, centralized: every code knob with an
// env-expressible value has a mirror here, and env beats code — an operator
// reconfigures a deployed binary without a rebuild. Each variable is read
// lazily, where its knob takes effect, rather than once at init, so a value
// set after construction still lands.
//
// Naming: GOLIB_<KNOB> for a core knob, GOLIB__<IMPL>_<KNOB> (double
// underscore) for an implementation-scoped one. The doubled separator
// namespaces the implementation, so two implementations can each expose a
// TIMEOUT knob (GOLIB__FOO_TIMEOUT, GOLIB__BAR_TIMEOUT) without colliding
// with each other or with a core GOLIB_TIMEOUT.
const (
	// LogEnv names the level (debug|info|warn|error) of the logger
	// v1alpha1.Logger returns: set, it writes to stderr at that level;
	// unset, that logger is silent. An unrecognized value reads as info and
	// logs a warning saying so — a misspelled level should not silence the
	// logs the operator was trying to turn on.
	LogEnv = "GOLIB_LOG"
)

// Builder assembles a value of type T from optional configuration. Configure it
// with the With* methods (each returns the Builder for chaining), then call the
// terminal Build to produce a Result. Obtain one from golib.New.
type Builder[T any] interface {
	// WithName sets a display name carried into the Result. Unset, the name is
	// empty.
	WithName(name string) Builder[T]
	// WithValue sets the payload the builder produces. Unset, Build returns the
	// zero value of T.
	WithValue(v T) Builder[T]
	// Build assembles the configured value and returns it. It is the terminal
	// step; calling it more than once returns the same Result.
	Build() Result[T]
	// Name returns the configured name (empty if WithName was never called).
	Name() string
}

// Result is the structured output of Builder.Build. The json tags make it drop
// straight into encoding/json and compatible marshalers.
type Result[T any] struct {
	Name  string `json:"name,omitempty"`
	Value T      `json:"value"`
}
