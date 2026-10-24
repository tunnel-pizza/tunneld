// Package read is the terminal_read tool: what one of the run's terminals
// has printed, read on from where this sign-in stopped or from a cursor,
// without waiting.
package read

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Tool is terminal_read.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_read",
	Title:       "Read a terminal's output",
	Description: "What the program in a terminal on this machine, at url, has printed since this sign-in last read, or since cursor, answered at once. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_read takes.
type In struct {
	URL    string `json:"url" jsonschema:"the address of a terminal on this machine: https://hostname/, or https://hostname/?n for the origin at index n"`
	Cursor string `json:"cursor,omitempty" jsonschema:"where to read on from: a cursor a terminal tool answered; left out, from where this sign-in last stopped"`
}

// Out is terminal_read's answer.
type Out struct {
	attach.Output
	Truncated bool `json:"truncated" jsonschema:"whether text was cut to its last 64 KiB"`
}

// run reads the output of the terminal url names, for the caller.
func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
	start := time.Now()
	term, refusal := c.Terminal(ctx, "terminal_read", in.URL, start)
	if refusal != nil {
		return refusal, Out{}, nil
	}
	got, err := term.Read(c.Caller(ctx), in.Cursor)
	if err != nil {
		c.Logged(ctx, "terminal_read", "refused", start)
		return errors.Unreadable(err), Out{}, nil
	}
	out := Out{Output: got}
	out.Text, out.Truncated = tool.Capped(out.Text)
	c.Logged(ctx, "terminal_read", "read", start)
	return nil, out, nil
}
