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
// changes nothing but where this sign-in reads from next.
func TestTerminalReadListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if a := listed.Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("annotations = %+v, want read-only", a)
	}
	for _, says := range []string{"cursor", "other sessions"} {
		if !strings.Contains(listed.Description, says) {
			t.Errorf("description %q lacks %q", listed.Description, says)
		}
	}
}

// TestTerminalRead pins the call: the output of the terminal url names from
// the caller's position, or from cursor, answering where it stopped; capped;
// and a cursor the terminal refuses, or what Context.Terminal refuses, a
// tool error.
func TestTerminalRead(t *testing.T) {
	exec := &auth.Access{Sub: "github:1", Grant: "g-1", Scopes: []string{"mcp", "tunnel:exec"}}
	long := strings.Repeat("x", tool.MaxText) + "end"
	for name, tc := range map[string]struct {
		access *auth.Access
		prints string
		args   string
		says   string
		failed bool
	}{
		"from where this sign-in stopped": {exec, "$ ls\ngo.mod\n", `{"url":"https://0tabc.tunneled.pizza/?1"}`, `"structuredContent":{"cursor":"@12","dropped":false,"text":"$ ls\ngo.mod\n","truncated":false}`, false},
		"from a cursor":                   {exec, "$ ls\ngo.mod\n", `{"url":"https://0tabc.tunneled.pizza/?1","cursor":"@5"}`, `"text":"go.mod\n"`, false},
		"more than the cap":               {nil, long, `{"url":"https://0tabc.tunneled.pizza/?1"}`, `"truncated":true`, false},
		"a stale cursor":                  {exec, "", `{"url":"https://0tabc.tunneled.pizza/?1","cursor":"stale"}`, "not this terminal's", true},
		"a junk cursor":                   {exec, "", `{"url":"https://0tabc.tunneled.pizza/?1","cursor":"junk"}`, "not a cursor", true},
		"since, not cursor":               {exec, "", `{"url":"https://0tabc.tunneled.pizza/?1","since":5}`, "unexpected additional properties", true},
		"without tunnel:exec":             {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, "", `{"url":"https://0tabc.tunneled.pizza/?1"}`, "needs the tunnel:exec scope", true},
		"not a terminal":                  {exec, "", `{"url":"https://0tabc.tunneled.pizza/"}`, "is not a terminal", true},
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

// TestTerminalReadsOnFromWhereItStopped pins the caller's position: a
// second read without a cursor gets only what came after the first.
func TestTerminalReadsOnFromWhereItStopped(t *testing.T) {
	term := &tooltest.Terminal{Prints: "$ ls\n"}
	c := tool.Context{
		Origins: tooltest.Origins(t, "exec:///bin/sh"),
		Bound:   tooltest.Bound{Terminals: map[int]attach.Terminal{0: term}},
	}
	me := &auth.Access{Sub: "github:1", Grant: "g-1", Scopes: []string{"mcp", "tunnel:exec"}}
	read := func() string {
		_, body := tooltest.Call(t, Tool, c, me, `{"url":"https://0tabc.tunneled.pizza/"}`)
		return body
	}
	if first := read(); !strings.Contains(first, `"text":"$ ls\n"`) {
		t.Fatalf("first read = %s", first)
	}
	if second := read(); !strings.Contains(second, `"text":""`) {
		t.Errorf("second read = %s, want nothing new", second)
	}
}
