// Package read is the terminal_read tool: what one of the run's terminals
// has printed, read on from where this sign-in stopped or from a cursor,
// without waiting.
package read

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/output"
)

// Tool is terminal_read.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_read",
	Title:       "Read a terminal's output",
	Description: "What the program in the terminal at url has printed, read on from where this sign-in last stopped (or from cursor when given; the first read starts from everything kept, the last 1 MiB at least), as text with control sequences dropped. Answers at once; terminal_wait waits for more. The answer's cursor is where it stopped: when other sessions may share the terminal under the same sign-in, pass it back on the next read so their reads do not move yours. dropped says output was no longer kept, so some was missed. Text past 64 KiB keeps its end and says truncated. url is the terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_read takes.
type In struct {
	URL    string `json:"url" jsonschema:"The terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none."`
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
		return output.Unreadable(c, ctx, "terminal_read", err, start), Out{}, nil
	}
	out := Out{Output: got}
	out.Text, out.Truncated = tool.Capped(out.Text)
	c.Logged(ctx, "terminal_read", "read", start)
	return nil, out, nil
}
