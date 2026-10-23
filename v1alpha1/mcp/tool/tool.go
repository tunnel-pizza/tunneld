// Package tool is what every MCP tool shares: its shape, an sdk.Tool and
// the handler that runs it, the run it reads, a tool error, a scope refusal
// and the one log line a call leaves. Each tool is its own package under
// tools/, and the server adds them all.
package tool

import (
	"context"
	"strconv"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
)

// Context is the run a tool reads: its origins, nil when it was given none,
// the servers standing in for the ones it serves, nil when none, its cache,
// nil when it has none, and where a call is logged.
type Context struct {
	Origins  v1.Origins
	Attached attach.Bound
	Cache    cache.Cache
	Log      v1.Logger
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

// Terminal is the terminal at url, or the tool error saying why not,
// logged: url names an origin of the run, the run serves it with a
// terminal, and the caller holds the scope of its scheme (tunnel:exec for a
// program).
func (c Context) Terminal(ctx context.Context, tool, url string, start time.Time) (attach.Terminal, *sdk.CallToolResult) {
	if c.Origins == nil || c.Attached == nil {
		c.Logged(ctx, tool, "refused", start)
		return nil, Failed("This run serves no terminal.")
	}
	origin, err := c.Origins.From(url)
	if err != nil {
		c.Logged(ctx, tool, "refused", start)
		return nil, Failed(err.Error() + ". terminal_sessions lists the terminals.")
	}
	term, ok := c.Attached.Terminal(origin.Index)
	if !ok {
		c.Logged(ctx, tool, "refused", start)
		return nil, Failed(url + " is not a terminal: origin " + strconv.Itoa(origin.Index) + " is " + string(origin.Scheme) + ". terminal_sessions lists the terminals.")
	}
	if refusal := c.Refused(ctx, tool, auth.Scope(origin.Scheme), start); refusal != nil {
		return nil, refusal
	}
	return term, nil
}

// Caller is whose read position a call moves in a terminal's output: the
// sign-in's grant (code_jti), one per connected app install; a token with
// no grant, its person and the client acting for them; the tunnel's
// secret, a caller of its own. Parallel agents on one sign-in share it, and
// keep apart by the cursors the terminal tools answer.
func (c Context) Caller(ctx context.Context) string {
	access, byToken := auth.AccessOf(ctx)
	switch {
	case !byToken:
		return "secret"
	case access.Grant != "":
		return "grant " + access.Grant
	default:
		return "sub " + access.Sub + " act " + access.Actor
	}
}

// MaxText is the most output text a tool hands an agent, so a log dump
// cannot flood its context.
const MaxText = 64 << 10

// Capped is text cut to its last MaxText bytes, from a whole character, and
// whether it was cut.
func Capped(text string) (string, bool) {
	if len(text) <= MaxText {
		return text, false
	}
	text = text[len(text)-MaxText:]
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return text, true
}

// Logged is a call's one log line: the tool, who, the client acting for
// them, how it ended and how long it took. Never its arguments or its
// answer.
func (c Context) Logged(ctx context.Context, tool, ended string, start time.Time) {
	access, _ := auth.AccessOf(ctx)
	c.Log.Info("mcp: call", "tool", tool, "sub", access.Sub, "actor", access.Actor, "ended", ended, "took", time.Since(start).Round(time.Millisecond))
}
