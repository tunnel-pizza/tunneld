package exec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tunnel-pizza/tunneld/v0exp1/internal/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v0exp1/internal/mcp/tool/tooltest"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
)

// TestTerminalExecListing pins how terminal_exec is listed: it acts, so it
// is destructive and not read-only.
func TestTerminalExecListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if a := listed.Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("annotations = %+v, want destructive, not read-only", a)
	}
	schema, _ := json.Marshal(listed.InputSchema)
	for _, s := range []string{"https://hostname/. If the tunnel presents multiple origins", "https://hostname/?1, not ?n=1", "index 0, needs none"} {
		if !strings.Contains(string(schema), s) || !strings.Contains(listed.Description, s[:16]) {
			t.Errorf("inputSchema %s or description %q lacks %q", schema, listed.Description, s)
		}
	}
}

// TestTerminalExec pins the stub: the scope is checked first, then the url
// must name one of the run's terminals; a caller past both learns it is not
// implemented.
func TestTerminalExec(t *testing.T) {
	c := tool.Context{Origins: tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh")}
	at := func(url string) string { return `{"url":"` + url + `","command":"uptime"}` }
	exec := &auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}}
	for name, tc := range map[string]struct {
		access *auth.Access
		args   string
		says   string
	}{
		"the secret":                       {nil, at("https://0tabc.tunneled.pizza/?1"), "terminal_exec is not implemented yet"},
		"tunnel:exec":                      {exec, at("https://0tabc.tunneled.pizza/?1"), "terminal_exec is not implemented yet"},
		"without tunnel:exec":              {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, at("https://0tabc.tunneled.pizza/?1"), "needs the tunnel:exec scope"},
		"an origin that is not a terminal": {exec, at("https://0tabc.tunneled.pizza/?0"), "is not a terminal"},
		"past the list":                    {exec, at("https://0tabc.tunneled.pizza/?5"), "names origin 5"},
		"no command":                       {nil, `{"url":"https://0tabc.tunneled.pizza/?1"}`, "missing properties"},
		"index, not url":                   {nil, `{"index":1,"command":"uptime"}`, "unexpected additional properties"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := tooltest.Call(t, Tool, c, tc.access, tc.args)
			if status != 200 || !strings.Contains(body, tc.says) || !strings.Contains(body, `"isError":true`) {
				t.Errorf("terminal_exec = %d %s, want a tool error saying %q", status, body, tc.says)
			}
		})
	}
}
