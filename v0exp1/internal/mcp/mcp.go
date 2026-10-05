// Package mcp is the MCP server tunneld serves to agents: one per run, over
// streamable HTTP on the control path, knowing every origin of the run by
// the index the routing parameter and the stderr map use. The tools spawn
// private processes through each origin's Spawner, so an agent's command
// runs beside the terminal a person is watching rather than in it.
//
// The types a caller hands in are declared here and aliased by v0exp1,
// which is the only package allowed to import this one.
package mcp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Kind is what an origin is, which decides what the tools can do with it.
type Kind string

const (
	KindProgram   Kind = "program"   // exec://: a program on this machine
	KindContainer Kind = "container" // attach://: a container
	KindHTTP      Kind = "http"      // an address the tunnel dials
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

// server is one run's tools over its origins.
type server struct {
	origins  []Origin
	log      *slog.Logger
	sessions *sessions
}

// Handler answers the MCP endpoint for these origins, index n being origin
// n, logging each call on log. Closing stops every session and process it
// started.
//
// Stateless: the SDK's own session id is not used — tool sessions have ids
// of their own — and a request's end cancels the call it carried, so a
// client that drops kills the process it was waiting on. Localhost
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
		// A put_file of maxFile arrives base64-encoded, four thirds the
		// size, inside a JSON envelope.
		MaxRequestBodyBytes: maxFile*4/3 + 1<<20,
	})
	return h, closer
}

// newServer builds the SDK server with every tool registered. The closer
// ends the sessions.
func newServer(origins []Origin, log *slog.Logger) (*sdk.Server, io.Closer) {
	s := &server{origins: origins, log: log, sessions: newSessions(origins, log)}
	srv := sdk.NewServer(&sdk.Implementation{Name: "tunneld", Version: "0"}, &sdk.ServerOptions{
		Instructions: "Each tool names an origin by index n, as the origins tool lists them. " +
			"exec runs one command privately and returns its exit code; a session keeps a process between calls.",
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "origins", Description: "List the run's origins: index, origin, kind."}, s.listOrigins)
	sdk.AddTool(srv, &sdk.Tool{Name: "exec", Description: "Run argv once on origin n over pipes, no terminal; stdout, stderr and the exit code come back."}, s.exec)
	sdk.AddTool(srv, &sdk.Tool{Name: "put_file", Description: "Write bytes to an absolute path on origin n."}, s.putFile)
	sdk.AddTool(srv, &sdk.Tool{Name: "get_file", Description: "Read an absolute path on origin n."}, s.getFile)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_open", Description: "Start a private process on origin n that outlives this call; argv defaults to the origin's own program."}, s.sessions.open)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_write", Description: "Send stdin to a session."}, s.sessions.write)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_read", Description: "Read what a session has printed since the last read, waiting up to timeout_ms for something."}, s.sessions.read)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_close", Description: "End a session and its process."}, s.sessions.close)
	return srv, s.sessions
}

// origin answers origin n, or the tool error for an index off the list or an
// origin that cannot run a program.
func (s *server) origin(n int) (Origin, error) {
	return spawnable(s.origins, n)
}

// spawnable is origin n of origins when it can start a process, else the
// one-line reason it cannot, worded for the agent that asked.
func spawnable(origins []Origin, n int) (Origin, error) {
	if n < 0 || n >= len(origins) {
		return Origin{}, fmt.Errorf("origin %d: there are %d origins, 0 to %d", n, len(origins), len(origins)-1)
	}
	o := origins[n]
	if o.Spawner == nil {
		return Origin{}, fmt.Errorf("origin %d cannot run a program: it is %s %s origin", n, article(o.Kind), o.Kind)
	}
	return o, nil
}

// article is "an" before http, "a" before the rest.
func article(k Kind) string {
	if k == KindHTTP {
		return "an"
	}
	return "a"
}
