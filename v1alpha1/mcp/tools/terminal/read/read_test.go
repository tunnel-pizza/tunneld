package read

import (
	"strings"
	"testing"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
)

// TestTerminalReadListing pins how terminal_read is listed: it reads, and
// changes nothing.
func TestTerminalReadListing(t *testing.T) {
	if a := tooltest.Listed(t, Tool).Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("annotations = %+v, want read-only", a)
	}
}

// TestTerminalRead pins the call: the output of the terminal url names
// after since, everything kept when since is left out, capped, and what
// Context.Terminal refuses is a tool error.
func TestTerminalRead(t *testing.T) {
	exec := &auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}}
	long := strings.Repeat("x", tool.MaxText) + "end"
	for name, tc := range map[string]struct {
		access *auth.Access
		prints string
		args   string
		says   string
		failed bool
	}{
		"since":               {exec, "$ ls\ngo.mod\n", `{"url":"https://0tabc.tunneled.pizza/?1","since":5}`, `"structuredContent":{"dropped":false,"next":12,"text":"go.mod\n","truncated":false}`, false},
		"everything kept":     {nil, "$ ls\ngo.mod\n", `{"url":"https://0tabc.tunneled.pizza/?1"}`, `"text":"$ ls\ngo.mod\n"`, false},
		"more than the cap":   {nil, long, `{"url":"https://0tabc.tunneled.pizza/?1"}`, `"truncated":true`, false},
		"without tunnel:exec": {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, "", `{"url":"https://0tabc.tunneled.pizza/?1"}`, "needs the tunnel:exec scope", true},
		"not a terminal":      {exec, "", `{"url":"https://0tabc.tunneled.pizza/"}`, "is not a terminal", true},
	} {
		t.Run(name, func(t *testing.T) {
			c := tool.Context{
				Origins: tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh"),
				Bound:   tooltest.Bound{Terminals: map[int]attach.Terminal{1: &tooltest.Terminal{Prints: tc.prints}}},
			}
			status, body := tooltest.Call(t, Tool, c, tc.access, tc.args)
			body = strings.ReplaceAll(body, `\\n`, `\n`)
			if status != 200 || !strings.Contains(body, tc.says) || strings.Contains(body, `"isError":true`) != tc.failed {
				t.Errorf("terminal_read = %d %.300s, want %q, a tool error: %v", status, body, tc.says, tc.failed)
			}
		})
	}
}
