// Package screen is the terminal_screen tool: what one of the run's
// terminals shows now.
package screen

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// Tool is terminal_screen.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_screen",
	Title:       "Read a terminal's screen",
	Description: "What a terminal on this machine, at url, shows now: its size, visible rows, cursor and title, and whether a full-screen program is up. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	// Reads the screen and changes nothing, but a program is drawing it.
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
}, run())

// In is what terminal_screen takes.
type In struct {
	URL string `json:"url" jsonschema:"the address of a terminal on this machine: https://hostname/, or https://hostname/?n for the origin at index n"`
}

// run reads the screen of the terminal url names.
func run() tool.RunFunc[In, types.Screen] {
	return func(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, types.Screen, error) {
		start := time.Now()
		term, err := c.Terminal(ctx, "terminal_screen", in.URL, start)
		if err != nil {
			return errors.ErrScreen(err)
		}
		c.Logged(ctx, "terminal_screen", "read", start)
		return nil, term.Screen(), nil
	}
}
