// Package v0exp1 is tunneld's experimental surface: what works, ships in the
// binary, and has not earned a place in v1alpha1 yet. Everything here may
// change or disappear in any release.
//
// The implementations live under v0exp1/internal, so Go's internal rule keeps
// every other package from reaching them directly: the only way in is
// Experimental, which makes each use of an experiment visible at its call:
//
//	origin, err := v0exp1.Experimental().Builtin().Origin()
//
// Importing this package links the built-in shell, whose init turns a process
// started with its argument into that shell before main runs.
package v0exp1

import "github.com/tunnel-pizza/tunneld/v0exp1/internal/shell/builtin"

// Experiments is every experiment, one method each. A method returning nil is
// that experiment turned off, and every caller checks for it — so switching
// one off is a change here and nowhere else.
type Experiments interface {
	// Builtin is the shell built into tunneld, for a machine with none; nil
	// when it is turned off.
	Builtin() Builtin
}

// Builtin is the shell built into tunneld: Elvish, with u-root's commands on
// its $PATH.
type Builtin interface {
	// Origin is the built-in shell as an ordinary program origin: this
	// executable, run with the argument that makes it the shell. Not on
	// Windows, where it returns ErrBuiltinNoTerminal.
	Origin() (string, error)
	// Run is the built-in shell itself, interactive on this process's own
	// terminal until it exits; the result is its exit status.
	Run() int
}

// ErrBuiltinNoTerminal is Builtin.Origin's answer on a platform with no
// pseudo-terminals.
var ErrBuiltinNoTerminal = builtin.ErrNoTerminal

// Experimental is the way in to every experiment.
func Experimental() Experiments { return ExperimentsImpl{} }

// ExperimentsImpl is Experiments.
type ExperimentsImpl struct{}

// Builtin is the shell built into tunneld.
func (ExperimentsImpl) Builtin() Builtin { return BuiltinImpl{} }

// BuiltinImpl is Builtin, over v0exp1/internal/shell/builtin.
type BuiltinImpl struct{}

// Origin is builtin.Origin.
func (BuiltinImpl) Origin() (string, error) { return builtin.Origin() }

// Run is builtin.Run.
func (BuiltinImpl) Run() int { return builtin.Run() }
