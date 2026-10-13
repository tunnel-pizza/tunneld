// Package terminal is the terminal_ tools: what a run's terminals are, and
// acting on one.
package terminal

import (
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/exec"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/sessions"
)

// Tools is every terminal_ tool, each in a package of its own here.
var Tools = []tool.Tool{sessions.Tool, exec.Tool}
