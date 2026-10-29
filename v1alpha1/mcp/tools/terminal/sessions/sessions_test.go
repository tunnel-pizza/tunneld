package sessions

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	v1types "github.com/tunnel-pizza/tunneld/v1/types"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// TestTerminalSessionsListing pins how terminal_sessions is listed: its
// title, read-only, idempotent and closed-world, and the schema and
// description an agent reads to use it.
func TestTerminalSessionsListing(t *testing.T) {
	listed := tooltest.Listed(t, Tool)
	if listed.Title != "Terminal sessions" {
		t.Errorf("title = %q", listed.Title)
	}
	if a := listed.Annotations; a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Errorf("annotations = %+v, want read-only, idempotent, closed-world", a)
	}
	schema, _ := json.Marshal(listed.OutputSchema)
	for _, s := range []string{"a bare ?1 (not ?n=1)", "never its arguments", `"index"`, `"ref"`, `"scheme"`, "a program on this machine", "a container", "a web address"} {
		if !strings.Contains(string(schema), s) {
			t.Errorf("outputSchema %s lacks %q", schema, s)
		}
	}
	input, _ := json.Marshal(listed.InputSchema)
	for _, s := range []string{"exec for its programs", "attach for its containers", "http for the web addresses"} {
		if !strings.Contains(string(input), s) {
			t.Errorf("inputSchema %s lacks %q", input, s)
		}
	}
	for _, s := range []string{"origins of one scheme", "index"} {
		if !strings.Contains(listed.Description, s) {
			t.Errorf("description %q lacks %q", listed.Description, s)
		}
	}
}

// TestTerminalSessions pins the tool: the run's origins of the scheme asked
// for, each by its run-wide ?n, to the secret's holder and to any token the
// MCP server admits, whatever its scopes: listing is not acting. A scheme
// that is not one is a tool error, with no origin.
func TestTerminalSessions(t *testing.T) {
	origins := tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh", "attach://dockerd/web", "exec:///usr/bin/k9s?arg=--readonly")
	execs := &types.Sessions{Sessions: []v1types.Session{
		{Origin: v1.Origin{Index: 1, Scheme: v1.ExecScheme, Ref: "/bin/sh"}},
		{Origin: v1.Origin{Index: 3, Scheme: v1.ExecScheme, Ref: "/usr/bin/k9s"}},
	}}
	containers := &types.Sessions{Sessions: []v1types.Session{{Origin: v1.Origin{Index: 2, Scheme: v1.AttachScheme, Provider: v1.DockerProvider, Ref: "web"}}}}
	for name, tc := range map[string]struct {
		access *auth.Access
		scheme string
		want   *types.Sessions
		says   string
	}{
		"the secret, exec":                 {nil, "exec", execs, ""},
		"any admitted token, exec":         {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, "exec", execs, ""},
		"any admitted token, attach":       {&auth.Access{Sub: "github:1"}, "attach", containers, ""},
		"a token an unowned tunnel let in": {&auth.Access{Sub: "github:9", Scopes: []string{"mcp"}, Unowned: true}, "exec", nil, "terminal_exec"},
		"no scheme":                        {nil, "", nil, "missing properties"},
		"a scheme that is none of them":    {nil, "ssh", nil, "exec, attach or http"},
	} {
		t.Run(name, func(t *testing.T) {
			tracked := cache.New(cache.WithDir(""), cache.WithTracking(map[string]string{
				"HOSTNAME": "box.local", "USERNAME": "christian", "PID": "16653", "PWD": "/Users/christian/cnuss",
				"SHELL": "/bin/zsh",
			}))
			status, body := call(t, tool.Context{Origins: origins, Cache: tracked}, tc.access, tc.scheme)
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			var msg struct {
				Result struct {
					IsError           bool            `json:"isError"`
					StructuredContent *types.Sessions `json:"structuredContent"`
					Content           []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			data, _ := strings.CutPrefix(strings.TrimSpace(body[strings.Index(body, "data: "):]), "data: ")
			if err := json.Unmarshal([]byte(data), &msg); err != nil {
				t.Fatalf("%v: %s", err, body)
			}
			r := msg.Result
			if r.IsError != (tc.want == nil) {
				t.Errorf("isError = %v: %s", r.IsError, body)
			}
			// An origin's arguments and its URI, which carries them, never
			// leave the machine.
			if strings.Contains(body, "--readonly") || strings.Contains(body, "exec:///") {
				t.Errorf("terminal_sessions = %s, want no arguments and no URI", body)
			}
			if tc.want != nil && (r.StructuredContent == nil || !reflect.DeepEqual(r.StructuredContent.Sessions, tc.want.Sessions)) {
				t.Errorf("structuredContent = %+v, want %+v", r.StructuredContent, tc.want)
			}
			machine := v1types.Machine{Hostname: "box.local", Username: "christian", PID: 16653, PWD: "/Users/christian/cnuss"}
			if tc.want != nil && r.StructuredContent.Machine != machine {
				t.Errorf("machine = %+v, want %+v, from the run's .env", r.StructuredContent.Machine, machine)
			}
			if tc.says != "" && (len(r.Content) == 0 || !strings.Contains(body, tc.says) || strings.Contains(body, "/bin/sh")) {
				t.Errorf("refusal %s, want %q and no origin", body, tc.says)
			}
		})
	}
}

// TestTerminalSessionsAddresses pins the tunnel's hostname, as the run's .env
// holds it, and each session's address from it: the bare address for a run
// of one origin, ?n for a run of more, as the run prints them.
func TestTerminalSessionsAddresses(t *testing.T) {
	tracked := cache.New(cache.WithDir(""), cache.WithTracking(map[string]string{"LIBTUNNEL_HOSTNAME": "0tabc.tunneld.domain"}))
	for name, tc := range map[string]struct {
		origins []string
		want    []string
	}{
		"one origin": {[]string{"exec:///bin/sh"}, []string{"https://0tabc.tunneld.domain/"}},
		"several":    {[]string{"http://localhost:3000", "exec:///bin/sh", "exec:///usr/bin/k9s"}, []string{"https://0tabc.tunneld.domain/?1", "https://0tabc.tunneld.domain/?2"}},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := call(t, tool.Context{Origins: tooltest.Origins(t, tc.origins...), Cache: tracked}, nil, "exec")
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			var msg struct {
				Result struct {
					StructuredContent *types.Sessions `json:"structuredContent"`
				} `json:"result"`
			}
			data, _ := strings.CutPrefix(strings.TrimSpace(body[strings.Index(body, "data: "):]), "data: ")
			if err := json.Unmarshal([]byte(data), &msg); err != nil || msg.Result.StructuredContent == nil {
				t.Fatalf("%v: %s", err, body)
			}
			out := msg.Result.StructuredContent
			if out.Machine.Tunnel != "0tabc.tunneld.domain" {
				t.Errorf("machine.tunnel = %q, want 0tabc.tunneld.domain", out.Machine.Tunnel)
			}
			var got []string
			for _, s := range out.Sessions {
				got = append(got, s.Address)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("addresses = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTerminalSessionsWithoutTerminals pins a run that serves no program: an
// empty list, never null, so a client reads "none" rather than "unknown".
func TestTerminalSessionsWithoutTerminals(t *testing.T) {
	_, body := call(t, tool.Context{Origins: tooltest.Origins(t, "http://localhost:3000")}, nil, "exec")
	if !strings.Contains(body, `"sessions":[]`) || strings.Contains(body, "localhost:3000") {
		t.Errorf("terminal_sessions = %s, want an empty sessions list", body)
	}
}

// TestMachineWithoutACache pins a server with no cache, or one that holds no
// tracking yet: the machine says nothing rather than guessing.
func TestMachineWithoutACache(t *testing.T) {
	if got := machine(tool.Context{}); got != (v1types.Machine{}) {
		t.Errorf("machine() = %+v, want nothing", got)
	}
	if got := machine(tool.Context{Cache: cache.None{}}); got != (v1types.Machine{}) {
		t.Errorf("machine() from None = %+v, want nothing", got)
	}
}

// call calls terminal_sessions for scheme, reading c, as tooltest.Call does;
// an empty scheme sends none.
func call(t *testing.T, c tool.Context, access *auth.Access, scheme string) (int, string) {
	t.Helper()
	args := `{}`
	if scheme != "" {
		args = `{"scheme":"` + scheme + `"}`
	}
	return tooltest.Call(t, Tool, c, access, args)
}
