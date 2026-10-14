// Package exec is the terminal_exec tool: running something in one of the
// run's terminals, typed as the person at it would, and reading the reply.
package exec

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/output"
)

// Tool is terminal_exec.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_exec",
	Title:       "Run in a terminal",
	Description: "Types into the terminal at url, as the person at it would (they see it land), then waits for the reply as terminal_wait does. input is steps in order, each exactly one of text or key: [{\"key\":\"Esc\"},{\"text\":\":wq\"},{\"key\":\"Enter\"}]. An Esc with steps after it goes alone, 150 ms ahead of them, so it is not read as Alt: send an Esc with what follows it, in one call. Text comes as the terminal shows it, a line at a time (one wider than the screen kept whole), capped at its last 64 KiB (truncated). A full-screen program (an editor, k9s, htop) leaves no text: the answer carries screen, as terminal_screen gives it. Pass the answer's cursor to the next read when other sessions may share this sign-in, so their reads do not move yours. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(false)},
}, run)

// In is what terminal_exec takes.
type In struct {
	URL   string         `json:"url" jsonschema:"The terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none."`
	Input []attach.Input `json:"input" jsonschema:"what to send, in order: each step exactly one of text or key"`
	output.WaitFor
}

// run types into the terminal url names, then waits for the reply from
// where the input went in, unless told to answer at once.
func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, output.Reply, error) {
	start := time.Now()
	if len(in.Input) == 0 {
		c.Logged(ctx, "terminal_exec", "refused", start)
		return tool.Failed("Nothing to send: input needs at least one step."), output.Reply{}, nil
	}
	// Checked before anything is typed: a pattern refused after would leave
	// the input sent and its reply unread.
	if _, err := in.Pattern(); err != nil {
		c.Logged(ctx, "terminal_exec", "refused", start)
		return tool.Failed("until is not a regular expression: " + err.Error() + "."), output.Reply{}, nil
	}
	term, refusal := c.Terminal(ctx, "terminal_exec", in.URL, start)
	if refusal != nil {
		return refusal, output.Reply{}, nil
	}
	reader := c.Caller(ctx)
	sent, err := term.Send(reader, in.Input...)
	if err != nil {
		c.Logged(ctx, "terminal_exec", "refused", start)
		return tool.Failed(err.Error() + "."), output.Reply{}, nil
	}
	if in.Until == "" && in.IdleMs != nil && *in.IdleMs == 0 {
		c.Logged(ctx, "terminal_exec", "sent", start)
		return nil, output.Reply{Output: attach.Output{Cursor: sent}}, nil
	}
	reply, refusal := output.Wait(c, ctx, "terminal_exec", term, reader, sent, in.WaitFor, start)
	if refusal != nil {
		return refusal, output.Reply{}, nil
	}
	c.Logged(ctx, "terminal_exec", "sent", start)
	return nil, reply, nil
}
