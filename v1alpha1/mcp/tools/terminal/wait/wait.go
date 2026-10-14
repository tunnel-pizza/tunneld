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
	Description: "Waits for more of what the program in the terminal at url prints, read on from where this sign-in stopped (or from cursor), until until matches, it goes quiet for idle_ms, or timeout_ms passes; matched and timed_out say which. On a full-screen program, until and idle_ms watch the screen. Text comes as the terminal shows it, a line at a time (one wider than the screen kept whole), capped at its last 64 KiB (truncated). dropped says some output was no longer kept. A full-screen program (an editor, k9s, htop) leaves no text: the answer carries screen, as terminal_screen gives it. Pass the answer's cursor to the next read when other sessions may share this sign-in, so their reads do not move yours. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_wait takes.
type In struct {
	URL    string `json:"url" jsonschema:"The terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none."`
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
