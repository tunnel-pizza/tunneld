// Package mcp is the MCP server tunneld serves to agents: one per run, over
// streamable HTTP on the control path, knowing every origin of the run by
// the index the routing parameter and the stderr map use. It offers
// terminal_sessions, the first of the terminal tools.
//
// The types a caller hands in are declared here and aliased by v0exp1,
// which is the only package allowed to import this one; a spawner is
// attach.Spawner, the one interface every origin's provider implements.
package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
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
	origins []Origin
	log     v1.Logger
	cache   cache.Cache

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
	sdk.AddTool(m.server, &sdk.Tool{
		Name:        "terminal_sessions",
		Title:       "Terminal sessions",
		Description: "The machine the run is on (its hostname, the user it runs as, its process id and working directory) and the run's origins, each with its n, its kind and its name: exec is a program in a terminal, named by its path, attach a container, named by the container, http an address, named by host and port. A tunnel with a single origin needs no parameter in a URL. With several, a bare ?1 (not ?n=1) picks one, and ?0 goes back to the first. Use to find which origin a terminal tool should act on.",
		// Answered from the run itself: the same each time, reaching nothing.
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)},
	}, m.terminalSessions)
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

// WithCache sets the run's cache, which terminal_sessions reads the
// machine from: the lines the run's .env holds.
func WithCache(c cache.Cache) Option {
	return func(m *McpImpl) { m.cache = c }
}

// WithLog sets where the server logs. Nil keeps the one it has.
func WithLog(log v1.Logger) Option {
	return func(m *McpImpl) {
		if log != nil {
			m.log = log
		}
	}
}

// Session is one origin as terminal_sessions answers it.
type Session struct {
	N    int    `json:"n" jsonschema:"the origin's index, 0 the first: in a URL a bare ?1 (not ?n=1) picks origin 1, and a single origin needs none"`
	Kind Kind   `json:"kind" jsonschema:"what the origin is: exec, attach or http"`
	Name string `json:"name" jsonschema:"what kind leaves out: an exec origin's program path (never its arguments), an http origin's host and port, an attach origin's container"`
}

// Machine is the run's own process, as its .env holds it (HOSTNAME,
// USERNAME, PID and PWD); a line the file does not hold is left out.
type Machine struct {
	Hostname string `json:"hostname,omitempty" jsonschema:"the machine's hostname"`
	Username string `json:"username,omitempty" jsonschema:"the user tunneld runs as"`
	PID      int    `json:"pid,omitempty" jsonschema:"tunneld's process id"`
	PWD      string `json:"pwd,omitempty" jsonschema:"the directory tunneld was started in"`
}

// Sessions is terminal_sessions' answer.
type Sessions struct {
	Machine  Machine   `json:"machine"`
	Sessions []Session `json:"sessions"`
}

// attachScope is what a provider token needs for the read-only terminal
// tools.
const attachScope = "tunnel:attach"

func (m *McpImpl) terminalSessions(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, Sessions, error) {
	start := time.Now()
	if refusal := m.refused(ctx, "terminal_sessions", attachScope, start); refusal != nil {
		return refusal, Sessions{}, nil
	}
	out := Sessions{Machine: m.machine(), Sessions: make([]Session, len(m.origins))}
	for i, o := range m.origins {
		out.Sessions[i] = Session{N: i, Kind: o.Kind, Name: nameOf(o.Name)}
	}
	m.logged(ctx, "terminal_sessions", "ok", start)
	return nil, out, nil
}

// refused is the tool error for a provider token without scope, logged; nil
// when the call may go on: the token holds it, or the tunnel's secret let the
// call in, which opens everything.
func (m *McpImpl) refused(ctx context.Context, tool, scope string, start time.Time) *sdk.CallToolResult {
	access, byToken := auth.AccessOf(ctx)
	if !byToken || access.Has(scope) {
		return nil
	}
	m.logged(ctx, tool, "refused", start)
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: "This sign-in can't use the terminal: " + tool + " needs the " + scope + " scope."}},
	}
}

// logged is a call's one log line: the tool, who, the client acting for
// them, how it ended and how long it took. Never its arguments or its
// answer.
func (m *McpImpl) logged(ctx context.Context, tool, ended string, start time.Time) {
	access, _ := auth.AccessOf(ctx)
	m.log.Info("mcp: call", "tool", tool, "sub", access.Sub, "actor", access.Actor, "ended", ended, "took", time.Since(start).Round(time.Millisecond))
}

// Handler answers the MCP endpoint.
func (m *McpImpl) Handler() http.Handler { return m.handler }

// Close ends whatever the server started, which is nothing: the one tool
// keeps no state. Callable more than once.
func (m *McpImpl) Close() error { return nil }

// machine is the run's own process as its .env holds it, read from the
// cache rather than worked out again; nothing without a cache.
func (m *McpImpl) machine() Machine {
	if m.cache == nil {
		return Machine{}
	}
	pid, _ := strconv.Atoi(m.cache.Get("PID"))
	return Machine{
		Hostname: m.cache.Get("HOSTNAME"),
		Username: m.cache.Get("USERNAME"),
		PID:      pid,
		PWD:      m.cache.Get("PWD"),
	}
}

// nameOf is an origin as named beside its kind: an exec origin's program
// path, without the arguments its query carries, which can hold a secret;
// an http origin's host and port, and its path but for a bare "/", without
// credentials or query; an attach origin's container. One that does not
// parse is named as given.
func nameOf(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return origin
	}
	switch u.Scheme {
	case string(KindExec):
		return u.Path
	case string(KindAttach):
		return strings.TrimPrefix(u.Path, "/")
	default:
		return u.Host + strings.TrimSuffix(u.Path, "/")
	}
}
