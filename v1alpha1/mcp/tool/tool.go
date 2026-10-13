// Package tool is what every MCP tool shares: its shape, an sdk.Tool and
// the handler that runs it, the run it reads, a tool error, a scope refusal
// and the one log line a call leaves. Each tool is its own package under
// tools/, and the server adds them all.
package tool

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
)

// Context is the run a tool reads: its origins, nil when it was given none,
// its cache, nil when it has none, and where a call is logged.
type Context struct {
	Origins v1.Origins
	Cache   cache.Cache
	Log     v1.Logger
}

// Tool is one MCP tool: how tools/list shows it, and how it is added to a
// server.
type Tool struct {
	sdk.Tool
	add func(*sdk.Server, Context)
}

// New is the tool def, run by run with the run's Context. In and Out are
// what the SDK infers the input and output schemas from.
func New[In, Out any](def sdk.Tool, run func(Context, context.Context, *sdk.CallToolRequest, In) (*sdk.CallToolResult, Out, error)) Tool {
	return Tool{Tool: def, add: func(s *sdk.Server, c Context) {
		listed := def
		sdk.AddTool(s, &listed, func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
			return run(c, ctx, req, in)
		})
	}}
}

// Add adds the tool to s, reading c.
func (t Tool) Add(s *sdk.Server, c Context) { t.add(s, c) }

// All is the run's origins, none when it was given none.
func (c Context) All() []v1.Origin {
	if c.Origins == nil {
		return nil
	}
	return c.Origins.All()
}

// Failed is a tool error carrying text, for the model to read: an error
// result, not a JSON-RPC error.
func Failed(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

// Refused is the tool error for a provider token without scope, logged; nil
// when the call may go on: the token holds it, or the tunnel's secret let the
// call in, which opens everything.
func (c Context) Refused(ctx context.Context, tool, scope string, start time.Time) *sdk.CallToolResult {
	access, byToken := auth.AccessOf(ctx)
	if !byToken || access.Has(scope) {
		return nil
	}
	c.Logged(ctx, tool, "refused", start)
	return Failed("This sign-in can't use the terminal: " + tool + " needs the " + scope + " scope.")
}

// Logged is a call's one log line: the tool, who, the client acting for
// them, how it ended and how long it took. Never its arguments or its
// answer.
func (c Context) Logged(ctx context.Context, tool, ended string, start time.Time) {
	access, _ := auth.AccessOf(ctx)
	c.Log.Info("mcp: call", "tool", tool, "sub", access.Sub, "actor", access.Actor, "ended", ended, "took", time.Since(start).Round(time.Millisecond))
}
