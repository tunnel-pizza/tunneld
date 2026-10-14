// Package mcp is the MCP server tunneld serves to agents: one per run, over
// streamable HTTP on the control path, knowing every origin of the run by
// the index the routing parameter and the stderr map use. It offers
// the terminal tools: terminal_sessions, terminal_screen, terminal_read,
// terminal_wait and terminal_exec.
package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Mcp serves one run's origins to agents over streamable HTTP, named by the
// index the routing parameter uses.
type Mcp interface {
	// Handler answers the MCP endpoint, to mount on the control path.
	Handler() http.Handler
	// Close ends whatever the server started. Callable more than once.
	io.Closer
}

// Option configures a McpImpl.
type Option = v1.Option[*McpImpl]

// McpImpl is the Mcp contract: one run's MCP server over its origins, a
// handler to mount on the control path and a Close for the run's end.
type McpImpl struct {
	origins v1.Origins
	bound   attach.Bound
	log     v1.Logger
	cache   cache.Cache
	ctx     context.Context

	server  *sdk.Server
	handler http.Handler
	stop    func() bool
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
	c := tool.Context{Origins: m.origins, Bound: m.bound, Cache: m.cache, Log: m.log}
	for _, t := range tools {
		t.Add(m.server, c)
	}
	m.handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return m.server }, &sdk.StreamableHTTPOptions{
		Stateless:                    true,
		DisableLocalhostProtection:   true,
		PropagateRequestCancellation: true,
	})
	if m.ctx != nil {
		m.stop = context.AfterFunc(m.ctx, func() { _ = m.Close() })
	}
	return m
}

// WithOrigins sets the run's origins, index n being origin n: the index the
// routing parameter and the stderr map use.
func WithOrigins(origins v1.Origins) Option {
	return func(m *McpImpl) { m.origins = origins }
}

// WithBound sets the servers standing in for the run's served origins, which
// the terminal tools type into and read.
func WithBound(bound attach.Bound) Option {
	return func(m *McpImpl) { m.bound = bound }
}

// WithCache sets the run's cache, which terminal_sessions reads the
// machine from: the lines the run's .env holds.
func WithCache(c cache.Cache) Option {
	return func(m *McpImpl) { m.cache = c }
}

// WithContext closes the server once ctx is done, so the caller has no
// Close to defer.
func WithContext(ctx context.Context) Option {
	return func(m *McpImpl) { m.ctx = ctx }
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

// Close ends whatever the server started, which is nothing: the one tool
// keeps no state. Callable more than once.
func (m *McpImpl) Close() error { return nil }
