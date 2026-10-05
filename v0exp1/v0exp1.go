// Package v0exp1 is tunneld's experimental surface: what works, ships in the
// binary, and has not earned a place in v1alpha1 yet. Everything here may
// change or disappear in any release.
//
// The implementations live under v0exp1/internal, so Go's internal rule keeps
// every other package from reaching them directly: the only way in is
// Experimental, which makes each use of an experiment visible at its call:
//
//	origin, err := v0exp1.Experimental().Builtin().Origin()
//	handler, closer := v0exp1.Experimental().Mcp().Handler(origins, log)
//
// Importing this package links the built-in shell, whose init turns a process
// started with its argument into that shell before main runs.
package v0exp1

import (
	"io"
	"log/slog"
	"net/http"

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
	// Mcp is the MCP server tunneld serves to agents on its control path;
	// nil when it is turned off.
	Mcp() Mcp
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

// Mcp serves one run's origins to agents over streamable HTTP: tools that
// run a command, move a file or hold a process on any origin that can spawn
// one, named by the index the routing parameter uses.
type Mcp interface {
	// Handler answers the MCP endpoint for these origins, index n being
	// origin n, logging each call on log. Closing stops every session and
	// process it started.
	Handler(origins []McpOrigin, log *slog.Logger) (http.Handler, io.Closer)
}

// McpOrigin is what the server is told about one origin: how it is shown,
// what it is, and what can spawn a process on it, nil for nothing. Prefixed,
// as every Mcp name here is, because this package is every experiment's and
// Origin alone would claim the word for one of them.
type McpOrigin = mcp.Origin

// McpKind is what an origin is: McpProgram, McpContainer or McpHTTP.
type McpKind = mcp.Kind

// The kinds of origin, as the origins tool reports them.
const (
	McpProgram   = mcp.KindProgram
	McpContainer = mcp.KindContainer
	McpHTTP      = mcp.KindHTTP
)

// McpSpawner starts one private process on an origin. attach.Spawner has the
// same method, so a bound origin's spawner is one as it stands.
type McpSpawner = mcp.Spawner

// ErrBuiltinNoTerminal is Builtin.Origin's answer on a platform with no
// pseudo-terminals.
var ErrBuiltinNoTerminal = builtin.ErrNoTerminal

// Experimental is the way in to every experiment.
func Experimental() Experiments { return ExperimentsImpl{} }

// ExperimentsImpl is Experiments.
type ExperimentsImpl struct{}

// Builtin is the shell built into tunneld.
func (ExperimentsImpl) Builtin() Builtin { return BuiltinImpl{} }

// Mcp is the MCP server.
func (ExperimentsImpl) Mcp() Mcp { return McpImpl{} }

// McpImpl is Mcp, over v0exp1/internal/mcp.
type McpImpl struct{}

// Handler is mcp.Handler.
func (McpImpl) Handler(origins []McpOrigin, log *slog.Logger) (http.Handler, io.Closer) {
	return mcp.Handler(origins, log)
}

// BuiltinImpl is Builtin, over v0exp1/internal/shell/builtin.
type BuiltinImpl struct{}

// Origin is builtin.Origin.
func (BuiltinImpl) Origin() (string, error) { return builtin.Origin() }

// Run is builtin.Run.
func (BuiltinImpl) Run() int { return builtin.Run() }
