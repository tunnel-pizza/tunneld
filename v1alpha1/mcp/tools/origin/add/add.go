// Package add is the origin_add tool: a new origin for the running tunnel,
// read the way the command line reads one.
package add

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors/refusals"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// Tool is origin_add.
var Tool = tool.New(sdk.Tool{
	Name:        "origin_add",
	Title:       "Add an origin",
	Description: "Adds an origin to this machine's tunnel, as tunneld's command line takes one: :3000, sh, exec:///bin/bash, attach://dockerd/web. Answers the machine and the new origin's session. Only the client that started this tunneld, over stdio, or a sign-in holding tunnel:origins, may call it. Not implemented yet: it refuses every call.",
	// Starts a program or attaches a container, and takes nothing away.
	Annotations: &sdk.ToolAnnotations{DestructiveHint: new(false), OpenWorldHint: new(false)},
}, run())

// In is what origin_add takes.
type In struct {
	Origin string `json:"origin" jsonschema:"the origin, as tunneld's command line takes one: a port (:3000), a program on this machine (sh), or a URL (http://localhost:3000, exec:///bin/bash, attach://dockerd/web)"`
}

// run refuses: adding an origin to a running tunnel is not implemented yet.
func run() tool.RunFunc[In, types.Origin] {
	return func(c tool.Context, ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, types.Origin, error) {
		start := time.Now()
		if err := c.ChangesOrigins(ctx, req, "origin_add", start); err != nil {
			return errors.ErrOriginAdd(err)
		}
		c.Logged(ctx, "origin_add", "refused", start)
		return errors.ErrOriginAdd(refusals.NotImplemented("origin_add"))
	}
}
