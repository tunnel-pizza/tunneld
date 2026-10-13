// Package exec is the terminal_exec tool: a command run in one of
// the run's terminals, as the person typing it would. A stub for now.
package exec

import (
	"context"
	"strconv"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Tool is terminal_exec.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_exec",
	Title:       "Run a command in a terminal",
	Description: "Runs command in the terminal at url, as the person typing it would. url is the terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none. Needs the tunnel:exec scope. Not implemented yet: it answers that it is not, once the caller may use it.",
	Annotations: &sdk.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(false)},
}, run)

// In is what terminal_exec takes.
type In struct {
	URL     string `json:"url" jsonschema:"The terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none."`
	Command string `json:"command" jsonschema:"the command line to run, as typed at the prompt"`
}

// scope is what a provider token needs to act on an exec origin.
const scope = "tunnel:exec"

// run is a stub: the scope is checked as the real tool will check it, then
// the url must name one of the run's terminals, and a caller past both is
// told it is not implemented.
func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
	start := time.Now()
	if refusal := c.Refused(ctx, "terminal_exec", scope, start); refusal != nil {
		return refusal, nil, nil
	}
	if c.Origins == nil {
		c.Logged(ctx, "terminal_exec", "refused", start)
		return tool.Failed("This run serves no terminal."), nil, nil
	}
	origin, err := c.Origins.From(in.URL)
	if err != nil {
		c.Logged(ctx, "terminal_exec", "refused", start)
		return tool.Failed(err.Error() + ". terminal_sessions lists the terminals."), nil, nil
	}
	if origin.Scheme != v1.ExecScheme {
		c.Logged(ctx, "terminal_exec", "refused", start)
		return tool.Failed(in.URL + " is not a terminal: origin " + strconv.Itoa(origin.Index) + " is " + string(origin.Scheme) + ". terminal_sessions lists the terminals."), nil, nil
	}
	c.Logged(ctx, "terminal_exec", "not implemented", start)
	return tool.Failed("terminal_exec is not implemented yet."), nil, nil
}
