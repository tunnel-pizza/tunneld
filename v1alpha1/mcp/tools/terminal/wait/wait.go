// Package wait is the terminal_wait tool: what one of the run's terminals
// prints, read on from where this sign-in stopped or from a cursor, waiting
// until it prints what was asked for, falls quiet, or the wait runs out.
package wait

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/output"
)

// Tool is terminal_wait.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_wait",
	Title:       "Wait for a terminal's output",
	Description: "terminal_read, once until matches, the program falls quiet for idle_ms, or timeout_ms passes. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_wait takes.
type In struct {
	URL    string `json:"url" jsonschema:"the terminal's address: https://hostname/, or https://hostname/?n for the origin at index n"`
	Cursor string `json:"cursor,omitempty" jsonschema:"where to read on from: a cursor a terminal tool answered; left out, from where this sign-in last stopped"`
	output.WaitFor
}

// run waits on the output of the terminal url names, for the caller.
func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, output.Reply, error) {
	start := time.Now()
	if _, err := in.Pattern(); err != nil {
		c.Logged(ctx, "terminal_wait", "refused", start)
		return tool.Failed("until is not a regular expression: " + err.Error() + "."), output.Reply{}, nil
	}
	term, refusal := c.Terminal(ctx, "terminal_wait", in.URL, start)
	if refusal != nil {
		return refusal, output.Reply{}, nil
	}
	reply, refusal := output.Wait(c, ctx, "terminal_wait", term, c.Caller(ctx), in.Cursor, in.WaitFor, start)
	if refusal != nil {
		return refusal, output.Reply{}, nil
	}
	c.Logged(ctx, "terminal_wait", "read", start)
	return nil, reply, nil
}
