// Package mcp is the MCP server tunneld serves to agents: one per run, over
// streamable HTTP on the control path, knowing every origin of the run by
// the index the routing parameter and the stderr map use. It offers no tools
// yet: the agent surface is being redesigned.
//
// The types a caller hands in are declared here and aliased by v0exp1,
// which is the only package allowed to import this one; a spawner is
// attach.Spawner, the one interface every origin's provider implements.
package mcp

import (
	"io"
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// Kind is what an origin is.
type Kind string

const (
	KindExec   Kind = "exec"   // exec://: a program on this machine
	KindAttach Kind = "attach" // attach://: a container
	KindHTTP   Kind = "http"   // an address the tunnel dials
)

// Origin is what the server knows about one origin of the run.
type Origin struct {
	Name    string         // the origin as shown, e.g. exec:///bin/sh
	Kind    Kind           // what it is
	Spawner attach.Spawner // nil when the origin cannot start a process
}

// Mcp serves one run's origins to agents over streamable HTTP, named by the
// index the routing parameter uses. It offers no tools yet: the agent
// surface is being redesigned.
type Mcp interface {
	// Handler answers the MCP endpoint, to mount on the control path.
	Handler() http.Handler
	// Close ends whatever the server started. Callable more than once.
	io.Closer
}

// Option configures a McpImpl.
type Option = v1.Option[*McpImpl]

// McpImpl is the Mcp contract: one run's MCP server over its origins, a
// handler to mount on the control path and a Close for the run's end. It offers no tools yet: the surface is being redesigned, and the
// origins are kept for what comes back.
type McpImpl struct {
	origins []Origin
	log     v1.Logger

	server  *sdk.Server
	handler http.Handler
}

// New returns a McpImpl configured by opts: no origins, and a log that
// discards.
//
// Stateless, and a request's end cancels the call it carried. Localhost
// protection is off because it would refuse exactly the shape every request
// here has: the router listens on 127.0.0.1 and the tunnel forwards the
// public hostname as Host. The secret in front of this handler is the guard,
// not the address.
func New(opts ...Option) *McpImpl {
	m := v1.Apply(&McpImpl{log: slog.New(slog.DiscardHandler)}, opts...)
	m.server = sdk.NewServer(&sdk.Implementation{Name: "tunneld", Version: "0"}, nil)
	m.handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return m.server }, &sdk.StreamableHTTPOptions{
		Stateless:                    true,
		DisableLocalhostProtection:   true,
		PropagateRequestCancellation: true,
	})
	return m
}

// WithOrigins sets the run's origins, index n being origin n: the index the
// routing parameter and the stderr map use.
func WithOrigins(origins []Origin) Option {
	return func(m *McpImpl) { m.origins = origins }
}

// WithLog sets where the server logs. Nil keeps the one it has.
func WithLog(log v1.Logger) Option {
	return func(m *McpImpl) {
		if log != nil {
			m.log = log
		}
	}
}

// Handler answers the MCP endpoint.
func (m *McpImpl) Handler() http.Handler { return m.handler }

// Close ends whatever the server started, which is nothing while there are
// no tools. Callable more than once.
func (m *McpImpl) Close() error { return nil }
