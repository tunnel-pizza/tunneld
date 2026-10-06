// Package v0exp1 is tunneld's experimental surface: what works, ships in the
// binary, and has not earned a place in v1alpha1 yet. Everything here may
// change or disappear in any release.
//
// The implementations live under v0exp1/internal, so Go's internal rule keeps
// every other package from reaching them directly: the only way in is
// Experimental, which makes each use of an experiment visible at its call:
//
//	origin, err := v0exp1.Experimental().Builtin().Origin()
//	server := v0exp1.Experimental().Mcp(v0exp1.McpWithOrigins(origins))
//
// Importing this package links the built-in shell, whose init turns a process
// started with its argument into that shell before main runs.
package v0exp1

import (
	"log/slog"

	"github.com/tunnel-pizza/tunneld/v0exp1/internal/mcp"
	"github.com/tunnel-pizza/tunneld/v0exp1/internal/shell/builtin"
)

// Experiments is every experiment, one method each. A method returning nil is
// that experiment turned off, and every caller checks for it — so switching
// one off is a change here and nowhere else.
type Experiments interface {
	// Builtin is the shell built into tunneld, for a machine with none; nil
	// when it is turned off.
	Builtin() Builtin
	// Mcp is the MCP server tunneld serves to agents on its control path,
	// configured by opts; nil when it is turned off.
	Mcp(opts ...McpOption) Mcp
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

// Mcp is mcp.Mcp: the MCP server, a handler to mount on the control path and
// a closer for the run's end.
type Mcp = mcp.Mcp

// McpOption configures the MCP server. Prefixed, as every Mcp name here is,
// because this package is every experiment's and Option alone would claim
// the word for one of them.
type McpOption = mcp.Option

// McpWithOrigins sets the run's origins, index n being origin n.
func McpWithOrigins(origins []McpOrigin) McpOption { return mcp.WithOrigins(origins) }

// McpWithLog sets where the server logs. Nil keeps the one it has.
func McpWithLog(log *slog.Logger) McpOption { return mcp.WithLog(log) }

// McpOrigin is what the server is told about one origin: how it is shown,
// what it is, and what can spawn a process on it, nil for nothing.
type McpOrigin = mcp.Origin

// McpKind is what an origin is: McpExec, McpAttach or McpHTTP.
type McpKind = mcp.Kind

// The kinds of origin.
const (
	McpExec   = mcp.KindExec
	McpAttach = mcp.KindAttach
	McpHTTP   = mcp.KindHTTP
)

// ErrBuiltinNoTerminal is Builtin.Origin's answer on a platform with no
// pseudo-terminals.
var ErrBuiltinNoTerminal = builtin.ErrNoTerminal

// Experimental is the way in to every experiment.
func Experimental() Experiments { return ExperimentsImpl{} }

// ExperimentsImpl is Experiments.
type ExperimentsImpl struct{}

// Builtin is the shell built into tunneld.
func (ExperimentsImpl) Builtin() Builtin { return BuiltinImpl{} }

// Mcp is the MCP server, configured by opts.
func (ExperimentsImpl) Mcp(opts ...McpOption) Mcp { return mcp.New(opts...) }

// McpImpl is Mcp: v0exp1/internal/mcp's.
type McpImpl = mcp.McpImpl

// BuiltinImpl is Builtin, over v0exp1/internal/shell/builtin.
type BuiltinImpl struct{}

// Origin is builtin.Origin.
func (BuiltinImpl) Origin() (string, error) { return builtin.Origin() }

// Run is builtin.Run.
func (BuiltinImpl) Run() int { return builtin.Run() }
