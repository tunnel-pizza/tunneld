package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSpawner is a Spawner that writes what it is told and exits with what
// it is told, recording what it was asked to run.
type fakeSpawner struct {
	out, errText string
	exit         int
	echoStdin    bool
	block        bool // never returns until ctx ends
	argv         [][]string
}

func (f *fakeSpawner) Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.argv = append(f.argv, argv)
	if f.block {
		<-ctx.Done()
		return -1, nil
	}
	if f.echoStdin && stdin != nil {
		_, _ = io.Copy(stdout, stdin)
	}
	_, _ = io.WriteString(stdout, f.out)
	_, _ = io.WriteString(stderr, f.errText)
	return f.exit, nil
}

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

// call runs one tool and decodes its structured result into out; it returns
// the tool error's text, "" for none.
func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String()
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decoding %s result %s: %v", name, raw, err)
		}
	}
	return ""
}

// TestListsEveryTool pins the surface by name: what an agent sees in
// tools/list is the spec's table, nothing more.
func TestListsEveryTool(t *testing.T) {
	cs := connect(t, nil)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	want := "exec get_file origins put_file session_close session_open session_read session_write"
	if strings.Join(slices.Sorted(slices.Values(got)), " ") != want {
		t.Errorf("tools = %v, want %s", got, want)
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
