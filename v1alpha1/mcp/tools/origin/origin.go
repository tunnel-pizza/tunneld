// Package origin is the origin_ tools: changing what a running tunnel
// serves.
package origin

import (
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/origin/add"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/origin/remove"
)

// Tools is every origin_ tool, each in a package of its own here.
var Tools = []tool.Tool{add.Tool, remove.Tool}
