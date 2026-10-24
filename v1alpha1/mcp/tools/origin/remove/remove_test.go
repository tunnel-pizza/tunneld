package remove

import (
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
)

// TestOriginRemoveListing pins how origin_remove is listed: it changes the run, ends what the origin holds, and
// says it is not implemented yet.
func TestOriginRemoveListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if a := listed.Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint != true {
		t.Errorf("annotations = %+v, want a change that is destructive: true", a)
	}
	if !strings.Contains(listed.Description, "Not implemented yet") {
		t.Errorf("description %q does not say it is not implemented yet", listed.Description)
	}
}

// TestOriginRemove pins the call: over the tunnel, the secret and a token without
// tunnel:origins refused, saying why; a token with it, and the stdio client,
// get as far as the tool, which is not implemented yet.
func TestOriginRemove(t *testing.T) {
	c := tool.Context{Origins: tooltest.Origins(t, "exec:///bin/sh"), Log: slog.New(slog.DiscardHandler)}
	for name, tc := range map[string]struct {
		access *auth.Access
		says   string
	}{
		"the tunnel's secret":         {nil, "The tunnel's secret can't call origin_remove"},
		"a token without the scope":   {&auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}}, "origin_remove needs the tunnel:origins scope"},
		"a token with tunnel:origins": {&auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:origins"}}, "origin_remove is not implemented yet"},
	} {
		status, body := tooltest.Call(t, Tool, c, tc.access, `{"origin":"1"}`)
		if status != 200 || !strings.Contains(body, tc.says) || !strings.Contains(body, `"isError":true`) {
			t.Errorf("origin_remove over the tunnel, %s = %d %s, want a tool error saying %q", name, status, body, tc.says)
		}
	}
	r, _, err := run()(c, t.Context(), &sdk.CallToolRequest{}, In{Origin: "1"})
	if err != nil || r == nil || !r.IsError || r.Content[0].(*sdk.TextContent).Text != "origin_remove is not implemented yet" {
		t.Errorf("origin_remove over stdio = %+v, %v; want the tool error saying it is not implemented yet", r, err)
	}
}
