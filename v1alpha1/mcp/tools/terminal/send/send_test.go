package send

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
)

// TestTerminalSendListing pins how terminal_send is listed: it types into
// a program, so it is destructive, and the keys it knows are named.
func TestTerminalSendListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if a := listed.Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("annotations = %+v, want destructive, not read-only", a)
	}
	schema, _ := json.Marshal(listed.InputSchema)
	for _, s := range []string{"https://hostname/?1, not ?n=1", "or Ctrl-", "PageDown", "exactly one of text or key"} {
		if !strings.Contains(string(schema), s) {
			t.Errorf("inputSchema %s lacks %q", schema, s)
		}
	}
	if !strings.Contains(listed.Description, "terminal_screen") {
		t.Errorf("description %q does not say how to read the result", listed.Description)
	}
}

// TestTerminalSend pins the call: the steps reach the terminal url names in
// order, and what Context.Terminal or the terminal refuses is a tool error.
func TestTerminalSend(t *testing.T) {
	exec := &auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}}
	ls := `[{"text":"ls"},{"key":"Enter"}]`
	at := func(url, input string) string { return `{"url":"` + url + `","input":` + input + `}` }
	for name, tc := range map[string]struct {
		access *auth.Access
		args   string
		err    error
		sent   []attach.Input
		says   string
	}{
		"text, then a key":    {exec, at("https://0tabc.tunneled.pizza/?1", ls), nil, []attach.Input{{Text: "ls"}, {Key: "Enter"}}, ""},
		"a key, then text":    {nil, at("https://0tabc.tunneled.pizza/?1", `[{"key":"Esc"},{"text":":wq"}]`), nil, []attach.Input{{Key: "Esc"}, {Text: ":wq"}}, ""},
		"without tunnel:exec": {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, at("https://0tabc.tunneled.pizza/?1", ls), nil, nil, "needs the tunnel:exec scope"},
		"not a terminal":      {exec, at("https://0tabc.tunneled.pizza/", ls), nil, nil, "is not a terminal"},
		"a step it refuses":   {exec, at("https://0tabc.tunneled.pizza/?1", `[{"key":"Hyper-Q"}]`), attach.ErrUnknownKey, nil, "unknown key"},
		"nothing to send":     {exec, at("https://0tabc.tunneled.pizza/?1", `[]`), nil, nil, "Nothing to send"},
		"no input":            {exec, `{"url":"https://0tabc.tunneled.pizza/?1"}`, nil, nil, "missing properties"},
		"text, not input":     {exec, `{"url":"https://0tabc.tunneled.pizza/?1","text":"ls"}`, nil, nil, "unexpected additional properties"},
	} {
		t.Run(name, func(t *testing.T) {
			term := &tooltest.Terminal{Err: tc.err}
			c := tool.Context{
				Origins: tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh"),
				Bound:   tooltest.Bound{Terminals: map[int]attach.Terminal{1: term}},
			}
			status, body := tooltest.Call(t, Tool, c, tc.access, tc.args)
			if status != 200 {
				t.Fatalf("terminal_send = %d %s", status, body)
			}
			if failed := strings.Contains(body, `"isError":true`); failed != (tc.says != "") || !strings.Contains(body, tc.says) {
				t.Errorf("terminal_send = %s, want a tool error saying %q: %v", body, tc.says, tc.says != "")
			}
			if !slices.Equal(term.Sent(), tc.sent) {
				t.Errorf("sent %q, want %q", term.Sent(), tc.sent)
			}
		})
	}
}
