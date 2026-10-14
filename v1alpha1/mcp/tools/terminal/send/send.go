// Package send is the terminal_send tool: typing into one of the run's
// terminals, as the person at it would.
package send

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Tool is terminal_send.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_send",
	Title:       "Type into a terminal",
	Description: "Types into the terminal at url, as the person at it would: input is a list of steps sent in order, each exactly one of text or key, so a key can come before text ([{\"key\":\"Esc\"},{\"text\":\":wq\"},{\"key\":\"Enter\"}]). The person watching sees it land. Answers once it is sent, not once the program has answered, with next: the output's offset as the input went in. Read what the program printed in reply with terminal_wait from next, or look with terminal_screen. url is the terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(false)},
}, run)

// In is what terminal_send takes.
type In struct {
	URL   string         `json:"url" jsonschema:"The terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none."`
	Input []attach.Input `json:"input" jsonschema:"what to send, in order: each step exactly one of text or key"`
}

// Out is terminal_send's answer.
type Out struct {
	Next int64 `json:"next" jsonschema:"the output's offset as the input went in: terminal_wait or terminal_read from here gets the reply"`
}

// run types into the terminal url names.
func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
	start := time.Now()
	if len(in.Input) == 0 {
		c.Logged(ctx, "terminal_send", "refused", start)
		return tool.Failed("Nothing to send: input needs at least one step."), Out{}, nil
	}
	term, refusal := c.Terminal(ctx, "terminal_send", in.URL, start)
	if refusal != nil {
		return refusal, Out{}, nil
	}
	next, err := term.Send(in.Input...)
	if err != nil {
		c.Logged(ctx, "terminal_send", "refused", start)
		return tool.Failed(err.Error() + "."), Out{}, nil
	}
	c.Logged(ctx, "terminal_send", "sent", start)
	return nil, Out{Next: next}, nil
}
