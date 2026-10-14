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
// changes nothing.
func TestTerminalWaitListing(t *testing.T) {
	if a := tooltest.Listed(t, Tool).Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("annotations = %+v, want read-only", a)
	}
}

// TestTerminalWait pins the call: the arguments reach Wait with their
// defaults and the timeout's cap, the answer says how it ended, and a bad
// pattern or what Context.Terminal refuses is a tool error.
func TestTerminalWait(t *testing.T) {
	exec := &auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}}
	at := func(rest string) string { return `{"url":"https://0tabc.tunneled.pizza/?1",` + rest + `}` }
	for name, tc := range map[string]struct {
		access *auth.Access
		args   string
		waited tooltest.Waited
		says   string
		failed bool
	}{
		"defaults":            {exec, at(`"since":5`), tooltest.Waited{Since: 5, Idle: 500 * time.Millisecond, Left: 10 * time.Second}, `"matched":false,"next":12,"text":"go.mod\n","timed_out":false`, false},
		"a match":             {nil, at(`"since":0,"until":"go\\.mod"`), tooltest.Waited{Until: `go\.mod`, Idle: 500 * time.Millisecond, Left: 10 * time.Second}, `"matched":true`, false},
		"no quiet":            {nil, at(`"since":0,"idle_ms":0,"timeout_ms":2000`), tooltest.Waited{Left: 2 * time.Second}, `"matched":false`, false},
		"past the cap":        {nil, at(`"since":0,"timeout_ms":60000`), tooltest.Waited{Idle: 500 * time.Millisecond, Left: 25 * time.Second}, `"next":12`, false},
		"a bad pattern":       {exec, at(`"since":0,"until":"("`), tooltest.Waited{}, "until is not a regular expression", true},
		"no since":            {exec, `{"url":"https://0tabc.tunneled.pizza/?1"}`, tooltest.Waited{}, "missing properties", true},
		"without tunnel:exec": {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, at(`"since":0`), tooltest.Waited{}, "needs the tunnel:exec scope", true},
	} {
		t.Run(name, func(t *testing.T) {
			term := &tooltest.Terminal{Prints: "$ ls\ngo.mod\n"}
			c := tool.Context{
				Origins: tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh"),
				Bound:   tooltest.Bound{Terminals: map[int]attach.Terminal{1: term}},
			}
			status, body := tooltest.Call(t, Tool, c, tc.access, tc.args)
			body = strings.ReplaceAll(body, `\\n`, `\n`)
			if status != 200 || !strings.Contains(body, tc.says) || strings.Contains(body, `"isError":true`) != tc.failed {
				t.Fatalf("terminal_wait = %d %.400s, want %q, a tool error: %v", status, body, tc.says, tc.failed)
			}
			if tc.failed {
				return
			}
			got := term.LastWait()
			if got.Since != tc.waited.Since || got.Until != tc.waited.Until || got.Idle != tc.waited.Idle || got.Left > tc.waited.Left || got.Left < tc.waited.Left-time.Second {
				t.Errorf("Wait was asked %+v, want %+v", got, tc.waited)
			}
		})
	}
}
