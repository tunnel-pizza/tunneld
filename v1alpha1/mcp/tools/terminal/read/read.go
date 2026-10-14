// Package read is the terminal_read tool: what one of the run's terminals
// has printed, from an offset, without waiting.
package read

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Tool is terminal_read.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_read",
	Title:       "Read a terminal's output",
	Description: "What the program in the terminal at url has printed after since, as text with control sequences dropped, and next, the offset to read from next. Answers at once; terminal_wait waits for more. Leave since out for everything kept, the last 1 MiB at least; dropped says output after since is no longer kept. Text past 64 KiB keeps its end and says truncated. url is the terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_read takes.
type In struct {
	URL   string `json:"url" jsonschema:"The terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none."`
	Since int64  `json:"since,omitempty" jsonschema:"the offset to read after, from terminal_send or an earlier read: left out, everything kept"`
}

// Out is terminal_read's answer.
type Out struct {
	attach.Output
	Truncated bool `json:"truncated" jsonschema:"whether text was cut to its last 64 KiB"`
}

// run reads the output of the terminal url names.
func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
	start := time.Now()
	term, refusal := c.Terminal(ctx, "terminal_read", in.URL, start)
	if refusal != nil {
		return refusal, Out{}, nil
	}
	out := Out{Output: term.Read(in.Since)}
	out.Text, out.Truncated = tool.Capped(out.Text)
	c.Logged(ctx, "terminal_read", "read", start)
	return nil, out, nil
}
