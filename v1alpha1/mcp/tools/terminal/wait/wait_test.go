package wait

import (
	"strings"
	"testing"
	"time"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
)

// TestTerminalWaitListing pins how terminal_wait is listed: it reads, and
// changes nothing but where this sign-in reads from next.
func TestTerminalWaitListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if a := listed.Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("annotations = %+v, want read-only", a)
	}
	for _, says := range []string{"until", "tunnel:exec"} {
		if !strings.Contains(listed.Description, says) {
			t.Errorf("description %q lacks %q", listed.Description, says)
		}
	}
}

// TestTerminalWait pins the call: the wait is the caller's, from its
// position or a cursor, with the defaults tool.Reply gives, and what
// Context.Terminal or the terminal refuses is a tool error.
func TestTerminalWait(t *testing.T) {
	exec := &auth.Access{Sub: "github:1", Grant: "g-1", Scopes: []string{"mcp", "tunnel:exec"}}
	at := func(rest string) string { return `{"url":"https://0tabc.tunneld.domain/?1"` + rest + `}` }
	for name, tc := range map[string]struct {
		access *auth.Access
		args   string
		waited tooltest.Waited
		says   string
		failed bool
	}{
		"from where this sign-in stopped": {exec, at(""), tooltest.Waited{Reader: "grant g-1", Idle: 500 * time.Millisecond}, `"cursor":"@12"`, false},
		"from a cursor, until":            {exec, at(`,"cursor":"@5","until":"go\\.mod"`), tooltest.Waited{Reader: "grant g-1", Cursor: "@5", Until: `go\.mod`, Idle: 500 * time.Millisecond}, `"matched":true`, false},
		"the secret":                      {nil, at(""), tooltest.Waited{Reader: "secret", Idle: 500 * time.Millisecond}, `"text":"$ ls\ngo.mod\n"`, false},
		"a stale cursor":                  {exec, at(`,"cursor":"stale"`), tooltest.Waited{Reader: "grant g-1", Cursor: "stale", Idle: 500 * time.Millisecond}, "not this terminal's", true},
		"a bad pattern":                   {exec, at(`,"until":"("`), tooltest.Waited{}, "until is not a regular expression", true},
		"since, not cursor":               {exec, at(`,"since":0`), tooltest.Waited{}, "unexpected additional properties", true},
		"without tunnel:exec":             {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, at(""), tooltest.Waited{}, "needs the tunnel:exec scope", true},
	} {
		t.Run(name, func(t *testing.T) {
			term := &tooltest.Terminal{Prints: "$ ls\ngo.mod\n"}
			c := tool.Context{
				Origins:  tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh"),
				Attached: tooltest.Bound{Terminals: map[int]attach.Terminal{1: term}},
			}
			status, body := tooltest.Call(t, Tool, c, tc.access, tc.args)
			body = strings.ReplaceAll(body, `\\n`, `\n`)
			if status != 200 || !strings.Contains(body, tc.says) || strings.Contains(body, `"isError":true`) != tc.failed {
				t.Fatalf("terminal_wait = %d %.400s, want %q, a tool error: %v", status, body, tc.says, tc.failed)
			}
			// Refused before the wait, nothing was asked: the zero Waited.
			got := term.LastWait()
			got.Left = 0
			if got != tc.waited {
				t.Errorf("Wait was asked %+v, want %+v", got, tc.waited)
			}
		})
	}
}
