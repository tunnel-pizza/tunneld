// Package screen is the terminal_screen tool: what one of the run's
// terminals shows now.
package screen

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Tool is terminal_screen.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_screen",
	Title:       "Read a terminal's screen",
	Description: "What the terminal at url shows now, as the person sees it: its width and height (theirs; an agent never sets them), the visible rows, the cursor, the title, and whether a full-screen program is up. Not the history above it: terminal_read has that. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	// Reads the screen and changes nothing, but a program is drawing it.
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_screen takes.
type In struct {
	URL string `json:"url" jsonschema:"The terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none."`
}

// run reads the screen of the terminal url names.
func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, attach.Screen, error) {
	start := time.Now()
	term, refusal := c.Terminal(ctx, "terminal_screen", in.URL, start)
	if refusal != nil {
		return refusal, attach.Screen{}, nil
	}
	c.Logged(ctx, "terminal_screen", "read", start)
	return nil, term.Screen(), nil
}
