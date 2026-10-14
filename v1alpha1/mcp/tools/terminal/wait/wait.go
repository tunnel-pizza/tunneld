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
	Description: "What the program in the terminal at url prints, read on from where this sign-in last stopped (or from cursor when given), waiting until until (a regular expression) matches it, the program prints nothing for idle_ms (500 by default, 0 never), or timeout_ms passes (10000 by default, at most 25000). matched and timed_out say which; neither means it fell quiet. While a full-screen program (an editor, k9s, htop) is up it leaves no text: the answer carries screen, as terminal_screen gives it, until matches the screen's rows, and idle_ms counts from the screen's last change. A line wider than the screen comes back split at its width, a row each. Use it to keep reading a long reply after terminal_exec. The answer's cursor is where it stopped: when other sessions may share the terminal under the same sign-in, pass it back on the next read so their reads do not move yours. dropped says output was no longer kept, so some was missed. Text past 64 KiB keeps its end and says truncated. url is the terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none. Needs the scope of the origin's scheme: tunnel:exec for a program.",
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
