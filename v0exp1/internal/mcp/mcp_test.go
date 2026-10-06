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

// connect stands the server up over an in-memory transport and returns a
// client session on it.
func connect(t *testing.T, origins []Origin) *sdk.ClientSession {
	t.Helper()
	server, closer := newServer(origins, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = closer.Close() })
	ct, st := sdk.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
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

// TestListsNoTools pins the surface while it is redesigned: the server
// answers initialize and offers nothing.
func TestListsNoTools(t *testing.T) {
	cs := connect(t, nil)
	if caps := cs.InitializeResult().Capabilities; caps.Tools != nil {
		t.Errorf("capabilities.tools = %+v, want none", caps.Tools)
	}
}

// TestHandlerAcceptsAForwardedHost pins the loopback: the router listens on
// 127.0.0.1 and the tunnel forwards the public hostname as Host, which the
// SDK calls DNS rebinding and refuses unless told the secret is the guard.
func TestHandlerAcceptsAForwardedHost(t *testing.T) {
	h, closer := Handler(nil, slog.New(slog.DiscardHandler))
	defer closer.Close()
	srv := httptest.NewServer(h)
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
