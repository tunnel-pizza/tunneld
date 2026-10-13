package mcp

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
)

// connect stands m's server up over an in-memory transport and returns a
// client session on it.
func connect(t *testing.T, m *McpImpl) *sdk.ClientSession {
	t.Helper()
	t.Cleanup(func() { _ = m.Close() })
	ct, st := sdk.NewInMemoryTransports()
	ss, err := m.server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestNew pins the options: the origins are kept in order, a log replaces
// the discarding default, and a nil log keeps it.
func TestNew(t *testing.T) {
	origins := []Origin{{Name: "http://localhost:3000", Scheme: v1.HttpScheme}, {Name: "exec:///bin/sh", Scheme: v1.ExecScheme}}
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name    string
		opts    []Option
		origins []Origin
		log     *slog.Logger
	}{
		{"defaults", nil, nil, nil},
		{"origins", []Option{WithOrigins(origins)}, origins, nil},
		{"log", []Option{WithLog(log)}, nil, log},
		{"nil log keeps the default", []Option{WithLog(nil)}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.opts...)
			if len(m.origins) != len(tc.origins) {
				t.Fatalf("origins = %v, want %v", m.origins, tc.origins)
			}
			for i := range tc.origins {
				if m.origins[i] != tc.origins[i] {
					t.Errorf("origin %d = %+v, want %+v", i, m.origins[i], tc.origins[i])
				}
			}
			if m.log == nil {
				t.Error("log = nil, want the discarding default")
			}
			if tc.log != nil && m.log != tc.log {
				t.Error("log is not the one given")
			}
		})
	}
}

// TestListsTerminalSessions pins the surface: terminal_sessions, read-only,
// and nothing else yet.
func TestListsTerminalSessions(t *testing.T) {
	cs := connect(t, New())
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 2 || res.Tools[0].Name != "terminal_exec" || res.Tools[1].Name != "terminal_sessions" {
		t.Fatalf("tools = %+v, want terminal_exec and terminal_sessions", res.Tools)
	}
	if a := res.Tools[0].Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("terminal_exec annotations = %+v, want destructive, not read-only", a)
	}
	tool := res.Tools[1]
	if tool.Title != "Terminal sessions" {
		t.Errorf("title = %q", tool.Title)
	}
	if a := tool.Annotations; a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Errorf("annotations = %+v, want read-only, idempotent, closed-world", a)
	}
	schema, _ := json.Marshal(tool.OutputSchema)
	for _, s := range []string{"a bare ?1 (not ?n=1)", "never its arguments"} {
		if !strings.Contains(string(schema), s) {
			t.Errorf("outputSchema %s lacks %q", schema, s)
		}
	}
	for _, s := range []string{"A tunnel with a single origin needs no parameter in a URL", "a bare ?1 (not ?n=1) picks one, and ?0 goes back to the first"} {
		if !strings.Contains(tool.Description, s) {
			t.Errorf("description %q lacks %q", tool.Description, s)
		}
	}
}

// callOver calls terminal_sessions for scheme on m's handler over HTTP, the
// way the router hands a request on: with access on its context when a token
// let it in, with none when the secret did. An empty scheme sends none.
func callOver(t *testing.T, m *McpImpl, access *auth.Access, scheme string) (int, string) {
	t.Helper()
	args := `{}`
	if scheme != "" {
		args = `{"scheme":"` + scheme + `"}`
	}
	return callTool(t, m, access, "terminal_sessions", args)
}

// callTool calls tool with args, a JSON object, as callOver does.
func callTool(t *testing.T, m *McpImpl, access *auth.Access, tool, args string) (int, string) {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if access != nil {
			r = r.WithContext(auth.WithAccess(r.Context(), *access))
		}
		m.Handler().ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestTerminalSessions pins the tool: the run's origins of the scheme asked
// for, each by its run-wide ?n, to the secret's holder and to any token the
// MCP server admits, whatever its scopes: listing is not acting. A scheme
// that is not one is a tool error, with no origin.
func TestTerminalSessions(t *testing.T) {
	origins := []Origin{
		{Name: "http://localhost:3000", Scheme: v1.HttpScheme},
		{Name: "exec:///bin/sh", Scheme: v1.ExecScheme},
		{Name: "attach://dockerd/web", Scheme: v1.AttachScheme},
		{Name: "exec:///usr/bin/k9s?arg=--readonly", Scheme: v1.ExecScheme},
	}
	execs := &Sessions{Sessions: []Session{{N: 1, Name: "/bin/sh"}, {N: 3, Name: "/usr/bin/k9s"}}}
	containers := &Sessions{Sessions: []Session{{N: 2, Name: "web"}}}
	for name, tc := range map[string]struct {
		access *auth.Access
		scheme string
		want   *Sessions
		says   string
	}{
		"the secret, exec":              {nil, "exec", execs, ""},
		"any admitted token, exec":      {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, "exec", execs, ""},
		"any admitted token, attach":    {&auth.Access{Sub: "github:1"}, "attach", containers, ""},
		"no scheme":                     {nil, "", nil, "missing properties"},
		"a scheme that is none of them": {nil, "ssh", nil, "exec, attach or http"},
	} {
		t.Run(name, func(t *testing.T) {
			tracked := cache.New(cache.WithDir(""), cache.WithTracking(map[string]string{
				"HOSTNAME": "box.local", "USERNAME": "christian", "PID": "16653", "PWD": "/Users/christian/cnuss",
				"SHELL": "/bin/zsh",
			}))
			status, body := callOver(t, New(WithOrigins(origins), WithCache(tracked)), tc.access, tc.scheme)
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			var msg struct {
				Result struct {
					IsError           bool      `json:"isError"`
					StructuredContent *Sessions `json:"structuredContent"`
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
			if tc.want != nil && (r.StructuredContent == nil || !slices.Equal(r.StructuredContent.Sessions, tc.want.Sessions)) {
				t.Errorf("structuredContent = %+v, want %+v", r.StructuredContent, tc.want)
			}
			machine := Machine{Hostname: "box.local", Username: "christian", PID: 16653, PWD: "/Users/christian/cnuss"}
			if tc.want != nil && r.StructuredContent.Machine != machine {
				t.Errorf("machine = %+v, want %+v, from the run's .env", r.StructuredContent.Machine, machine)
			}
			if tc.says != "" && (len(r.Content) != 1 || !strings.Contains(r.Content[0].Text, tc.says) || strings.Contains(body, "/bin/sh")) {
				t.Errorf("refusal %s, want %q and no origin", body, tc.says)
			}
		})
	}
}

// TestTerminalSessionsLogs pins the log: one line per call naming the tool,
// the person and the client acting for them, and how it ended; never the
// origins it answered with.
func TestTerminalSessionsLogs(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil))
	m := New(WithOrigins([]Origin{{Name: "exec:///bin/sh", Scheme: v1.ExecScheme}}), WithLog(log))
	callOver(t, m, &auth.Access{Sub: "github:1", Actor: "https://claude.ai/client.json", Scopes: []string{"tunnel:exec"}}, "exec")
	mu.Lock()
	got := buf.String()
	mu.Unlock()
	for _, s := range []string{"tool=terminal_sessions", "sub=github:1", "actor=https://claude.ai/client.json", "ended=ok"} {
		if !strings.Contains(got, s) {
			t.Errorf("log %q lacks %s", got, s)
		}
	}
	if strings.Contains(got, "/bin/sh") {
		t.Errorf("log %q names an origin", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("log has %d lines, want 1: %q", n, got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestCloseTwice pins the closer: the run's defer and whatever else ends it
// may both close it.
func TestCloseTwice(t *testing.T) {
	m := New()
	for range 2 {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}

// TestServeAcceptsAForwardedHost pins the loopback: the router listens on
// 127.0.0.1 and the tunnel forwards the public hostname as Host, which the
// SDK calls DNS rebinding and refuses unless told the secret is the guard.
func TestServeAcceptsAForwardedHost(t *testing.T) {
	m := New()
	defer m.Close()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
	req.Host = "abc.tunnel.pizza"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize with a forwarded Host = %d %s, want 200", resp.StatusCode, b)
	}
}

// TestNameOf pins how an origin is named beside its kind: what the kind does
// not already say, and never an exec origin's arguments.
func TestNameOf(t *testing.T) {
	for raw, want := range map[string]string{
		"exec:///bin/zsh": "/bin/zsh",
		"exec:///Users/c/.local/bin/claude?arg=--resume&arg=b2e1": "/Users/c/.local/bin/claude",
		"http://localhost:3000":                                   "localhost:3000",
		"http://localhost:3000/":                                  "localhost:3000",
		"http://127.0.0.1:8080/app?x=1":                           "127.0.0.1:8080/app",
		"https://u:pw@example.test/admin":                         "example.test/admin",
		"attach://dockerd/web":                                    "web",
		"not a url %zz":                                           "not a url %zz",
	} {
		if got := nameOf(raw); got != want {
			t.Errorf("nameOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestMachineWithoutACache pins a server with no cache, or one that holds no
// tracking yet: the machine says nothing rather than guessing.
func TestMachineWithoutACache(t *testing.T) {
	if got := New().machine(); got != (Machine{}) {
		t.Errorf("machine() = %+v, want nothing", got)
	}
	if got := New(WithCache(cache.None{})).machine(); got != (Machine{}) {
		t.Errorf("machine() from None = %+v, want nothing", got)
	}
}

// TestTerminalSessionsWithoutTerminals pins a run that serves no program: an
// empty list, never null, so a client reads "none" rather than "unknown".
func TestTerminalSessionsWithoutTerminals(t *testing.T) {
	_, body := callOver(t, New(WithOrigins([]Origin{{Name: "http://localhost:3000", Scheme: v1.HttpScheme}})), nil, "exec")
	if !strings.Contains(body, `"sessions":[]`) || strings.Contains(body, "localhost:3000") {
		t.Errorf("terminal_sessions = %s, want an empty sessions list", body)
	}
}

// TestTerminalExec pins the stub: the secret, and a token holding
// tunnel:exec, reach it and learn it is not implemented; a token without the
// scope is refused, naming it, before anything else; n and command are
// required.
func TestTerminalExec(t *testing.T) {
	m := func() *McpImpl {
		return New(WithOrigins([]Origin{{Name: "exec:///bin/sh", Scheme: v1.ExecScheme}}))
	}
	args := `{"n":0,"command":"uptime"}`
	for name, tc := range map[string]struct {
		access *auth.Access
		args   string
		says   string
	}{
		"the secret":          {nil, args, "terminal_exec is not implemented yet"},
		"tunnel:exec":         {&auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}}, args, "terminal_exec is not implemented yet"},
		"without tunnel:exec": {&auth.Access{Sub: "github:1", Scopes: []string{"mcp"}}, args, "needs the tunnel:exec scope"},
		"no command":          {nil, `{"n":0}`, "missing properties"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := callTool(t, m(), tc.access, "terminal_exec", tc.args)
			if status != 200 || !strings.Contains(body, tc.says) || !strings.Contains(body, `"isError":true`) {
				t.Errorf("terminal_exec = %d %s, want a tool error saying %q", status, body, tc.says)
			}
		})
	}
}
