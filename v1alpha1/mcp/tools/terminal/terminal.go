// Package terminal is the terminal_ tools: what a run's terminals are, and
// acting on one.
package terminal

import (
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/exec"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/read"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/screen"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/sessions"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tools/terminal/wait"
)

// Tools is every terminal_ tool, each in a package of its own here.
var Tools = []tool.Tool{sessions.Tool, screen.Tool, read.Tool, wait.Tool, exec.Tool}
