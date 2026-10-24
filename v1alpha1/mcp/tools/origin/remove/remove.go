// Package remove is the origin_remove tool: an origin taken off the running
// tunnel, its index left a hole.
package remove

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors/refusals"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// Tool is origin_remove.
var Tool = tool.New(sdk.Tool{
	Name:        "origin_remove",
	Title:       "Remove an origin",
	Description: "Removes an origin from this machine's tunnel, named by its index or its public address; its index is never given to another. Answers the machine and the removed origin's session. Only the client that started this tunneld, over stdio, or a sign-in holding tunnel:origins, may call it. Not implemented yet: it refuses every call.",
	// Ends the program or the container attach the origin's terminal holds.
	Annotations: &sdk.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(false)},
}, run())

// In is what origin_remove takes.
type In struct {
	Origin string `json:"origin" jsonschema:"the origin to remove: its index (1), or its public address (https://hostname/?1)"`
}

// run refuses: removing an origin from a running tunnel is not implemented
// yet.
func run() tool.RunFunc[In, types.Origin] {
	return func(c tool.Context, ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, types.Origin, error) {
		start := time.Now()
		if err := c.ChangesOrigins(ctx, req, "origin_remove", start); err != nil {
			return errors.ErrOriginRemove(err)
		}
		c.Logged(ctx, "origin_remove", "refused", start)
		return errors.ErrOriginRemove(refusals.NotImplemented("origin_remove"))
	}
}
