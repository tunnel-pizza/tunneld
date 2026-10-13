package mcp

import (
	"slices"

	"github.com/tunnel-pizza/tunneld/v0exp1/internal/mcp/tools/terminal"
)

// tools is every tool the server offers: each group under tools/ hands over
// its own, a tool's name's parts being its directories (tools/terminal/exec
// is terminal_exec). tools/list names them in the SDK's order, by name.
var tools = slices.Concat(terminal.Tools)
