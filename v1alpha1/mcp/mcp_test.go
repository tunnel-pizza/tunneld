package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/tooltest"
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

// TestNew pins the options: the origins and the served origins are kept, a
// log replaces the discarding default, and a nil log keeps it.
func TestNew(t *testing.T) {
	all := tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh")
	served := tooltest.Bound{}
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name    string
		opts    []Option
		origins v1.Origins
		bound   attach.Bound
		log     *slog.Logger
	}{
		{"defaults", nil, nil, nil, nil},
		{"origins", []Option{WithOrigins(all)}, all, nil, nil},
		{"bound", []Option{WithBound(served)}, nil, served, nil},
		{"log", []Option{WithLog(log)}, nil, nil, log},
		{"nil log keeps the default", []Option{WithLog(nil)}, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.opts...)
			if m.origins != tc.origins {
				t.Errorf("origins = %v, want the ones given, %v", m.origins, tc.origins)
			}
			if (m.bound == nil) != (tc.bound == nil) {
				t.Errorf("bound = %v, want the one given, %v", m.bound, tc.bound)
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

// TestCloseTwice pins the closer: the context's end and a caller's own Close
// may both close it.
func TestCloseTwice(t *testing.T) {
	m := New()
	for range 2 {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}

// TestWithContext pins the run's context as the closer: once it is done the
// server is closed, with nothing left for a caller to defer.
func TestWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	m := New(WithContext(ctx))
	cancel()
	if m.stop == nil || m.stop() {
		t.Error("the context is done and the server was not closed")
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

// TestATerminalOverMcp drives a real /bin/sh through the server, as an agent
// does: terminal_sessions finds it, terminal_send types a command and
// presses Enter, and terminal_screen shows what it printed.
func TestATerminalOverMcp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh")
	}
	shown := tooltest.Origins(t, "http://localhost:3000", "exec:///bin/sh")
	_, bound, err := attach.New(attach.WithTargets(shell.New())).Bind(t.Context(), shown, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	defer func() { _ = bound.Close() }()
	cs := connect(t, New(WithOrigins(shown), WithBound(bound)))

	call := func(name, args string) *sdk.CallToolResult {
		t.Helper()
		var a map[string]any
		_ = json.Unmarshal([]byte(args), &a)
		res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: a})
		if err != nil || res.IsError {
			t.Fatalf("%s(%s) = %+v, %v", name, args, res, err)
		}
		return res
	}
	if listed, _ := json.Marshal(call("terminal_sessions", `{"scheme":"exec"}`).StructuredContent); !strings.Contains(string(listed), `"index":1`) {
		t.Fatalf("terminal_sessions = %s, want the shell at index 1", listed)
	}
	call("terminal_send", `{"url":"https://0tabc.tunneled.pizza/?1","input":[{"text":"echo $((6*7))"},{"key":"Enter"}]}`)

	deadline := time.Now().Add(10 * time.Second)
	for {
		var screen attach.Screen
		raw, _ := json.Marshal(call("terminal_screen", `{"url":"https://0tabc.tunneled.pizza/?1"}`).StructuredContent)
		_ = json.Unmarshal(raw, &screen)
		if slices.Contains(screen.Rows, "42") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("screen = %q, want a row reading 42", screen.Rows)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
