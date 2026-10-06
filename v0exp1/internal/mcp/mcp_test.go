package mcp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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
	origins := []Origin{{Name: "http://localhost:3000", Kind: KindHTTP}, {Name: "exec:///bin/sh", Kind: KindExec}}
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

// TestListsNoTools pins the surface while it is redesigned: the server
// answers initialize and offers nothing.
func TestListsNoTools(t *testing.T) {
	cs := connect(t, New())
	if caps := cs.InitializeResult().Capabilities; caps.Tools != nil {
		t.Errorf("capabilities.tools = %+v, want none", caps.Tools)
	}
}

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
