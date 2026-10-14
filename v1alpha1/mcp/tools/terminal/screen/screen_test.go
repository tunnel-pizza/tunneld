package screen

import (
	"strings"
	"testing"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
)

// TestTerminalScreenListing pins how terminal_screen is listed: it reads,
// and changes nothing.
func TestTerminalScreenListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if a := listed.Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("annotations = %+v, want read-only", a)
	}
}

// TestTerminalScreen pins the call: the screen of the terminal url names,
// and what Context.Terminal refuses is a tool error.
func TestTerminalScreen(t *testing.T) {
	term := &tooltest.Terminal{Shows: attach.Screen{Width: 80, Height: 24, Rows: []string{"$ ls", "go.mod"}, Cursor: attach.Cursor{Row: 2}, Title: "sh"}}
	c := tool.Context{
		Origins: tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh"),
		Bound:   tooltest.Bound{Terminals: map[int]attach.Terminal{1: term}},
	}
	exec := &auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}}
	for name, tc := range map[string]struct {
		access *auth.Access
		url    string
		says   string
		failed bool
	}{
		"tunnel:exec":         {exec, "https://0tabc.tunneled.pizza/?1", `"structuredContent":{"alt_screen":false,"cursor":{"col":0,"row":2},"height":24,"rows":["$ ls","go.mod"],"title":"sh","width":80}`, false},
		"the secret":          {nil, "https://0tabc.tunneled.pizza/?1", `"title":"sh"`, false},
		"without tunnel:exec": {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, "https://0tabc.tunneled.pizza/?1", "needs the tunnel:exec scope", true},
		"not a terminal":      {exec, "https://0tabc.tunneled.pizza/", "is not a terminal", true},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := tooltest.Call(t, Tool, c, tc.access, `{"url":"`+tc.url+`"}`)
			if status != 200 || !strings.Contains(body, tc.says) || strings.Contains(body, `"isError":true`) != tc.failed {
				t.Errorf("terminal_screen = %d %s, want %q, a tool error: %v", status, body, tc.says, tc.failed)
			}
		})
	}
}
