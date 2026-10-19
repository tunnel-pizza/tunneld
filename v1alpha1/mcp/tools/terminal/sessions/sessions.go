// Package sessions is the terminal_sessions tool: the machine the
// run is on, and the run's origins of one scheme.
package sessions

import (
	"context"
	"strconv"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Tool is terminal_sessions.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_sessions",
	Title:       "Terminal sessions",
	Description: "This machine, and its origins of one scheme, each with its index and ref.",
	// Answered from the run itself: the same each time, reaching nothing.
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_sessions takes.
type In struct {
	Scheme v1.Scheme `json:"scheme" jsonschema:"which of this machine's origins: exec, attach or http"`
}

// Machine is the run's own process, as its .env holds it (HOSTNAME,
// USERNAME, PID and PWD); a line the file does not hold is left out.
type Machine struct {
	Hostname string `json:"hostname,omitempty" jsonschema:"this machine's hostname"`
	Username string `json:"username,omitempty" jsonschema:"the user tunneld runs as on this machine"`
	PID      int    `json:"pid,omitempty" jsonschema:"tunneld's process id on this machine"`
	PWD      string `json:"pwd,omitempty" jsonschema:"the directory tunneld was started in on this machine"`
}

// Out is terminal_sessions' answer.
type Out struct {
	Machine  Machine     `json:"machine"`
	Sessions []v1.Origin `json:"sessions"`
}

func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
	start := time.Now()
	switch in.Scheme {
	case v1.ExecScheme, v1.AttachScheme, v1.HttpScheme:
	default:
		c.Logged(ctx, "terminal_sessions", "refused", start)
		return tool.Failed("scheme must be exec, attach or http."), Out{}, nil
	}
	// Listing needs no scope beyond what let the call in: acting on an
	// origin is what a tool asks a scope for. Each by its index among all
	// the origins, which is what ?n and the other tools name it by.
	out := Out{Machine: machine(c), Sessions: []v1.Origin{}}
	for _, o := range c.All() {
		if o.Scheme == in.Scheme {
			out.Sessions = append(out.Sessions, o)
		}
	}
	c.Logged(ctx, "terminal_sessions", "ok", start)
	return nil, out, nil
}

// machine is the run's own process as its .env holds it, read from the
// cache rather than worked out again; nothing without a cache.
func machine(c tool.Context) Machine {
	if c.Cache == nil {
		return Machine{}
	}
	pid, _ := strconv.Atoi(c.Cache.Get("PID"))
	return Machine{
		Hostname: c.Cache.Get("HOSTNAME"),
		Username: c.Cache.Get("USERNAME"),
		PID:      pid,
		PWD:      c.Cache.Get("PWD"),
	}
}
