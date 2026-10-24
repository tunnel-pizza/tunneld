package add

import (
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
)

// TestOriginAddListing pins how origin_add is listed: it changes the run, adds and takes nothing away, and
// says it is not implemented yet.
func TestOriginAddListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if a := listed.Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint != false {
		t.Errorf("annotations = %+v, want a change that is destructive: false", a)
	}
	if !strings.Contains(listed.Description, "Not implemented yet") {
		t.Errorf("description %q does not say it is not implemented yet", listed.Description)
	}
}

// TestOriginAdd pins the call: over the tunnel, the secret and a token without
// tunnel:origins refused, saying why; a token with it, and the stdio client,
// get as far as the tool, which is not implemented yet.
func TestOriginAdd(t *testing.T) {
	c := tool.Context{Origins: tooltest.Origins(t, "exec:///bin/sh"), Log: slog.New(slog.DiscardHandler)}
	for name, tc := range map[string]struct {
		access *auth.Access
		says   string
	}{
		"the tunnel's secret":         {nil, "The tunnel's secret can't call origin_add"},
		"a token without the scope":   {&auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}}, "origin_add needs the tunnel:origins scope"},
		"a token with tunnel:origins": {&auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:origins"}}, "origin_add is not implemented yet"},
	} {
		status, body := tooltest.Call(t, Tool, c, tc.access, `{"origin":":3000"}`)
		if status != 200 || !strings.Contains(body, tc.says) || !strings.Contains(body, `"isError":true`) {
			t.Errorf("origin_add over the tunnel, %s = %d %s, want a tool error saying %q", name, status, body, tc.says)
		}
	}
	r, _, err := run()(c, t.Context(), &sdk.CallToolRequest{}, In{Origin: ":3000"})
	if err != nil || r == nil || !r.IsError || r.Content[0].(*sdk.TextContent).Text != "origin_add is not implemented yet" {
		t.Errorf("origin_add over stdio = %+v, %v; want the tool error saying it is not implemented yet", r, err)
	}
}
