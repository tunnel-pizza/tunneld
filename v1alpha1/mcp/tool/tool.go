// Package tool is what every MCP tool shares: its shape, an sdk.Tool and
// the handler that runs it, the run it reads, a tool error, a scope refusal
// and the one log line a call leaves. Each tool is its own package under
// tools/, and the server adds them all.
package tool

import (
	"context"
	"errors"
	"regexp"
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
	Origins v1.Origins
	Bound   attach.Bound
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

// Terminal is the terminal at url, or the tool error saying why not,
// logged: url names an origin of the run, the run serves it with a
// terminal, and the caller holds the scope of its scheme (tunnel:exec for a
// program).
func (c Context) Terminal(ctx context.Context, tool, url string, start time.Time) (attach.Terminal, *sdk.CallToolResult) {
	if c.Origins == nil || c.Bound == nil {
		c.Logged(ctx, tool, "refused", start)
		return nil, Failed("This run serves no terminal.")
	}
	origin, err := c.Origins.From(url)
	if err != nil {
		c.Logged(ctx, tool, "refused", start)
		return nil, Failed(err.Error() + ". terminal_sessions lists the terminals.")
	}
	term, ok := c.Bound.Terminal(origin.Index)
	if !ok {
		c.Logged(ctx, tool, "refused", start)
		return nil, Failed(url + " is not a terminal: origin " + strconv.Itoa(origin.Index) + " is " + string(origin.Scheme) + ". terminal_sessions lists the terminals.")
	}
	if refusal := c.Refused(ctx, tool, "tunnel:"+string(origin.Scheme), start); refusal != nil {
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

// Waits stay short: the relay holds one request per call, and the edge ends
// one that writes nothing for about 100 seconds.
const (
	defaultIdle    = 500 * time.Millisecond
	defaultTimeout = 10 * time.Second
	maxTimeout     = 25 * time.Second
)

// WaitFor is when a terminal tool that waits for output stops: until a
// pattern matches what it read, the program falls quiet, or the wait runs
// out.
type WaitFor struct {
	Until     string `json:"until,omitempty" jsonschema:"a regular expression (RE2) that ends the wait once the text read matches it"`
	IdleMs    *int   `json:"idle_ms,omitempty" jsonschema:"end once the program has printed nothing for this long, in milliseconds: 500 when left out, 0 never"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"end after this long, in milliseconds: 10000 when left out, at most 25000"`
}

// Pattern is Until compiled, nil when there is none.
func (w WaitFor) Pattern() (*regexp.Regexp, error) {
	if w.Until == "" {
		return nil, nil
	}
	return regexp.Compile(w.Until)
}

// Reply is what a terminal tool that waits answers: the output read, and
// how the wait ended.
type Reply struct {
	attach.Output
	Matched   bool `json:"matched" jsonschema:"whether until matched"`
	TimedOut  bool `json:"timed_out" jsonschema:"whether timeout_ms passed first; neither this nor matched means the program fell quiet"`
	Truncated bool `json:"truncated" jsonschema:"whether text was cut to its last 64 KiB"`
}

// Reply waits on term for reader from cursor, or reader's position when
// cursor is "", as w says, or answers the tool error why not, logged: a
// pattern that does not compile, or a cursor not the terminal's.
func (c Context) Reply(ctx context.Context, tool string, term attach.Terminal, reader, cursor string, w WaitFor, start time.Time) (Reply, *sdk.CallToolResult) {
	until, err := w.Pattern()
	if err != nil {
		c.Logged(ctx, tool, "refused", start)
		return Reply{}, Failed("until is not a regular expression: " + err.Error() + ".")
	}
	idle := defaultIdle
	if w.IdleMs != nil {
		idle = time.Duration(max(0, *w.IdleMs)) * time.Millisecond
	}
	timeout := defaultTimeout
	if w.TimeoutMs > 0 {
		timeout = min(time.Duration(w.TimeoutMs)*time.Millisecond, maxTimeout)
	}
	waiting, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	got, matched, err := term.Wait(waiting, reader, cursor, until, idle)
	if err != nil {
		return Reply{}, c.Unreadable(ctx, tool, err, start)
	}
	reply := Reply{Output: got, Matched: matched, TimedOut: !matched && errors.Is(waiting.Err(), context.DeadlineExceeded)}
	reply.Text, reply.Truncated = Capped(reply.Text)
	return reply, nil
}

// Unreadable is the tool error for a read the terminal refused, logged: a
// cursor not its own, from another terminal or before tunneld started
// again, or one no terminal gave.
func (c Context) Unreadable(ctx context.Context, tool string, err error, start time.Time) *sdk.CallToolResult {
	c.Logged(ctx, tool, "refused", start)
	if errors.Is(err, attach.ErrStaleCursor) {
		return Failed("That cursor is not this terminal's: it came from another terminal, or from before tunneld started again, so the output it points into is gone. Leave cursor out to read on from where this sign-in last stopped.")
	}
	if errors.Is(err, attach.ErrBadCursor) {
		return Failed("That is not a cursor: pass back one a terminal tool answered, or leave it out.")
	}
	return Failed(err.Error() + ".")
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
