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
	v1types "github.com/tunnel-pizza/tunneld/v1/types"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors/refusals"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// Tool is terminal_sessions.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_sessions",
	Title:       "Terminal sessions",
	Description: "This machine and its tunnel's public hostname, and its origins of one scheme, each with its index and ref.",
	// Answered from the run itself: the same each time, reaching nothing.
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)},
}, run())

// In is what terminal_sessions takes.
type In struct {
	Scheme v1.Scheme `json:"scheme" jsonschema:"which of this machine's origins to list: exec for its programs and attach for its containers, the terminals; or http for the web addresses its tunnel forwards to"`
}

// run lists the run's origins of one scheme, with the machine they are on.
func run() tool.RunFunc[In, types.Sessions] {
	return func(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, types.Sessions, error) {
		start := time.Now()
		switch in.Scheme {
		case v1.ExecScheme, v1.AttachScheme, v1.HttpScheme:
		default:
			c.Logged(ctx, "terminal_sessions", "refused", start)
			return errors.ErrSessions(refusals.UnknownScheme(in.Scheme))
		}
		// Listing needs no scope beyond what let the call in: acting on an
		// origin is what a tool asks a scope for. Each by its index among all
		// the origins, which is what ?n and the other tools name it by.
		out := types.Sessions{Machine: machine(c), Sessions: []v1types.Session{}}
		all := c.All()
		for _, o := range all {
			if o.Scheme == in.Scheme {
				out.Sessions = append(out.Sessions, v1types.Session{Origin: o, Address: address(out.Machine.Tunnel, o.Index, len(all))})
			}
		}
		c.Logged(ctx, "terminal_sessions", "ok", start)
		return nil, out, nil
	}
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
func machine(c tool.Context) v1types.Machine {
	if c.Cache == nil {
		return v1types.Machine{}
	}
	pid, _ := strconv.Atoi(c.Cache.Get("PID"))
	return v1types.Machine{
		Tunnel:   c.Cache.Get(ltv1.HostnameEnv),
		Hostname: c.Cache.Get("HOSTNAME"),
		Username: c.Cache.Get("USERNAME"),
		PID:      pid,
		PWD:      c.Cache.Get("PWD"),
	}
}
