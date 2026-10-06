// Package mcp is the MCP server tunneld serves to agents: one per run, over
// streamable HTTP on the control path, knowing every origin of the run by
// the index the routing parameter and the stderr map use. It offers no tools
// yet: the agent surface is being redesigned.
//
// The types a caller hands in are declared here and aliased by v0exp1,
// which is the only package allowed to import this one.
package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Kind is what an origin is.
type Kind string

const (
	KindExec   Kind = "exec"   // exec://: a program on this machine
	KindAttach Kind = "attach" // attach://: a container
	KindHTTP   Kind = "http"   // an address the tunnel dials
)

// Spawner starts one private process on an origin, over pipes, and reports
// how it ended: the exit code, -1 when a signal ended it. err is a failure
// to start, never a non-zero exit. The process ends with ctx. The same shape
// as attach.Spawner, declared here so this package imports nothing of
// v1alpha1's.
type Spawner interface {
	Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exit int, err error)
}

// Origin is what the server knows about one origin of the run.
type Origin struct {
	Name    string  // the origin as shown, e.g. exec:///bin/sh
	Kind    Kind    // what it is
	Spawner Spawner // nil when the origin cannot start a process
}

// server is one run's MCP server over its origins. It offers no tools yet:
// the surface is being redesigned, and the origins are kept for what comes
// back.
type server struct {
	origins []Origin
	log     *slog.Logger
}

// Close has nothing to end while there are no tools. Callable more than once.
func (s *server) Close() error { return nil }

// Handler answers the MCP endpoint for these origins, index n being origin
// n, logging on log. The closer ends whatever the server started.
//
// Stateless, and a request's end cancels the call it carried. Localhost
// protection is off because it would refuse exactly the shape every request
// here has: the router listens on 127.0.0.1 and the tunnel forwards the
// public hostname as Host. The secret in front of this handler is the guard,
// not the address.
func Handler(origins []Origin, log *slog.Logger) (http.Handler, io.Closer) {
	srv, closer := newServer(origins, log)
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{
		Stateless:                    true,
		DisableLocalhostProtection:   true,
		PropagateRequestCancellation: true,
	})
	return h, closer
}

// newServer builds the SDK server, with no tools registered.
func newServer(origins []Origin, log *slog.Logger) (*sdk.Server, io.Closer) {
	s := &server{origins: origins, log: log}
	return sdk.NewServer(&sdk.Implementation{Name: "tunneld", Version: "0"}, nil), s
}
