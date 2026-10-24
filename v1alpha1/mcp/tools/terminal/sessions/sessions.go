// Package sessions is the terminal_sessions tool: the machine the
// run is on, and the run's origins of one scheme.
package sessions

import (
	"context"
	"strconv"
	"time"

	ltv1 "github.com/cnuss/libtunnel/v1"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Tool is terminal_sessions.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_sessions",
	Title:       "Terminal sessions",
	Description: "This machine and its tunnel's public hostname, and its origins of one scheme, each with its index and ref.",
	// Answered from the run itself: the same each time, reaching nothing.
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_sessions takes.
type In struct {
	Scheme v1.Scheme `json:"scheme" jsonschema:"which of this machine's origins: exec, attach or http"`
}

// Machine is the run's own process, as its .env holds it (HOSTNAME,
// USERNAME, PID, PWD and the tunnel's LIBTUNNEL_HOSTNAME); a line the file
// does not hold is left out.
type Machine struct {
	Tunnel   string `json:"tunnel,omitempty" jsonschema:"the public hostname of this machine's tunnel: its terminals are at https://<tunnel>/, or https://<tunnel>/?n for the origin at index n"`
	Hostname string `json:"hostname,omitempty" jsonschema:"this machine's hostname"`
	Username string `json:"username,omitempty" jsonschema:"the user tunneld runs as on this machine"`
	PID      int    `json:"pid,omitempty" jsonschema:"tunneld's process id on this machine"`
	PWD      string `json:"pwd,omitempty" jsonschema:"the directory tunneld was started in on this machine"`
}

// Session is one origin, with the address the other terminal tools take it
// at.
type Session struct {
	v1.Origin
	Address string `json:"address,omitempty" jsonschema:"the origin's public address, the url the other terminal tools take: https://<tunnel>/, or https://<tunnel>/?n when the run has more than one origin"`
}

// Out is terminal_sessions' answer.
type Out struct {
	Machine  Machine   `json:"machine"`
	Sessions []Session `json:"sessions"`
}

func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
	start := time.Now()
	switch in.Scheme {
	case v1.ExecScheme, v1.AttachScheme, v1.HttpScheme:
	default:
		c.Logged(ctx, "terminal_sessions", "refused", start)
		return errors.UnknownScheme(), Out{}, nil
	}
	// Listing needs no scope beyond what let the call in: acting on an
	// origin is what a tool asks a scope for. Each by its index among all
	// the origins, which is what ?n and the other tools name it by.
	out := Out{Machine: machine(c), Sessions: []Session{}}
	all := c.All()
	for _, o := range all {
		if o.Scheme == in.Scheme {
			out.Sessions = append(out.Sessions, Session{Origin: o, Address: address(out.Machine.Tunnel, o.Index, len(all))})
		}
	}
	c.Logged(ctx, "terminal_sessions", "ok", start)
	return nil, out, nil
}

// address is origin i's public address on tunnel, a run of n origins: the
// bare address when there is one, ?i when there are more, as the run prints
// them. Empty while the tunnel is not known.
func address(tunnel string, i, n int) string {
	if tunnel == "" {
		return ""
	}
	if n <= 1 {
		return "https://" + tunnel + "/"
	}
	return "https://" + tunnel + "/?" + strconv.Itoa(i)
}

// machine is the run's own process as its .env holds it, read from the
// cache rather than worked out again; nothing without a cache.
func machine(c tool.Context) Machine {
	if c.Cache == nil {
		return Machine{}
	}
	pid, _ := strconv.Atoi(c.Cache.Get("PID"))
	return Machine{
		Tunnel:   c.Cache.Get(ltv1.HostnameEnv),
		Hostname: c.Cache.Get("HOSTNAME"),
		Username: c.Cache.Get("USERNAME"),
		PID:      pid,
		PWD:      c.Cache.Get("PWD"),
	}
}
