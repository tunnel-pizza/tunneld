package exec

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
)

// TestTerminalExecListing pins how terminal_exec is listed: it types into
// a program, so it is destructive, and the keys it knows are named.
func TestTerminalExecListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if a := listed.Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("annotations = %+v, want destructive, not read-only", a)
	}
	schema, _ := json.Marshal(listed.InputSchema)
	for _, s := range []string{"https://hostname/?n", "or Ctrl-", "PageDown", "exactly one of text or key"} {
		if !strings.Contains(string(schema), s) {
			t.Errorf("inputSchema %s lacks %q", schema, s)
		}
	}
	for _, says := range []string{"waits for the reply", "tunnel:exec"} {
		if !strings.Contains(listed.Description, says) {
			t.Errorf("description %q lacks %q", listed.Description, says)
		}
	}
}

// TestTerminalExec pins the call: the steps reach the terminal url names in
// order, the reply is waited for from where the input went in, under the
// caller's sign-in, and what Context.Terminal or the terminal refuses is a
// tool error, with nothing sent.
func TestTerminalExec(t *testing.T) {
	exec := &auth.Access{Sub: "github:1", Grant: "g-1", Scopes: []string{"mcp", "tunnel:exec"}}
	ls := `[{"text":"ls"},{"key":"Enter"}]`
	at := func(url, input, rest string) string {
		return `{"url":"` + url + `","input":` + input + rest + `}`
	}
	for name, tc := range map[string]struct {
		access *auth.Access
		args   string
		err    error
		sent   []attach.Input
		says   string
		waited tooltest.Waited
	}{
		"text, then a key, and the reply": {exec, at("https://0tabc.tunneld.domain/?1", ls, ""), nil, []attach.Input{{Text: "ls"}, {Key: "Enter"}},
			`"structuredContent":{"cursor":"@10","dropped":false,"matched":false,"text":"go.mod\n","timed_out":false,"truncated":false}`,
			tooltest.Waited{Reader: "grant g-1", Cursor: "@3", Idle: 500 * time.Millisecond}},
		"until the prompt": {exec, at("https://0tabc.tunneld.domain/?1", ls, `,"until":"go\\.mod"`), nil, []attach.Input{{Text: "ls"}, {Key: "Enter"}},
			`"matched":true`, tooltest.Waited{Reader: "grant g-1", Cursor: "@3", Until: `go\.mod`}},
		"no wait": {exec, at("https://0tabc.tunneld.domain/?1", ls, `,"idle_ms":0`), nil, []attach.Input{{Text: "ls"}, {Key: "Enter"}},
			`"structuredContent":{"cursor":"@3","dropped":false,"matched":false,"text":"","timed_out":false,"truncated":false}`, tooltest.Waited{}},
		"the secret": {nil, at("https://0tabc.tunneld.domain/?1", `[{"key":"Esc"},{"text":":wq"}]`, ""), nil, []attach.Input{{Key: "Esc"}, {Text: ":wq"}},
			`"text":"go.mod\n"`, tooltest.Waited{Reader: "secret", Cursor: "@3", Idle: 500 * time.Millisecond}},
		"a bad pattern":       {exec, at("https://0tabc.tunneld.domain/?1", ls, `,"until":"("`), nil, nil, "until is not a regular expression", tooltest.Waited{}},
		"without tunnel:exec": {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, at("https://0tabc.tunneld.domain/?1", ls, ""), nil, nil, "needs the tunnel:exec scope", tooltest.Waited{}},
		"not a terminal":      {exec, at("https://0tabc.tunneld.domain/", ls, ""), nil, nil, "is not a terminal", tooltest.Waited{}},
		"a step it refuses":   {exec, at("https://0tabc.tunneld.domain/?1", `[{"key":"Hyper-Q"}]`, ""), attach.ErrUnknownKey, nil, "unknown key", tooltest.Waited{}},
		"nothing to send":     {exec, at("https://0tabc.tunneld.domain/?1", `[]`, ""), nil, nil, "Nothing to send", tooltest.Waited{}},
		"no input":            {exec, `{"url":"https://0tabc.tunneld.domain/?1"}`, nil, nil, "missing properties", tooltest.Waited{}},
		"text, not input":     {exec, `{"url":"https://0tabc.tunneld.domain/?1","text":"ls"}`, nil, nil, "unexpected additional properties", tooltest.Waited{}},
	} {
		t.Run(name, func(t *testing.T) {
			term := &tooltest.Terminal{Prints: "$ \n", Reply: "go.mod\n", Err: tc.err}
			c := tool.Context{
				Origins:  tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh"),
				Attached: tooltest.Attached{Terminals: map[int]attach.Terminal{1: term}},
			}
			status, body := tooltest.Call(t, Tool, c, tc.access, tc.args)
			if status != 200 {
				t.Fatalf("terminal_exec = %d %s", status, body)
			}
			failed := strings.Contains(body, `"isError":true`)
			if !strings.Contains(body, tc.says) || failed != (len(tc.sent) == 0) {
				t.Errorf("terminal_exec = %s, want %q, a tool error: %v", body, tc.says, len(tc.sent) == 0)
			}
			if !slices.Equal(term.Sent(), tc.sent) {
				t.Errorf("sent %q, want %q", term.Sent(), tc.sent)
			}
			got := term.LastWait()
			got.Left = 0
			if got != tc.waited {
				t.Errorf("waited %+v, want %+v", got, tc.waited)
			}
		})
	}
}
