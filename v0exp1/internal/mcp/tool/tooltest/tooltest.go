// Package tooltest stands one tool up on a server of its own, served over
// streamable HTTP the way the run serves it, for that tool's tests.
package tooltest

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v0exp1/internal/mcp/tool"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
)

// Call calls tl with args, a JSON object, reading c: with access on the
// request's context when a token let it in, with none when the secret did.
// It answers the HTTP status and body, as the client would read them. A c
// with no Log logs nowhere.
func Call(t *testing.T, tl tool.Tool, c tool.Context, access *auth.Access, args string) (int, string) {
	t.Helper()
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "tooltest", Version: "0"}, nil)
	tl.Add(s, c)
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, &sdk.StreamableHTTPOptions{
		Stateless:                  true,
		DisableLocalhostProtection: true,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if access != nil {
			r = r.WithContext(auth.WithAccess(r.Context(), *access))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tl.Name + `","arguments":` + args + `}}`
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

// Listed is tl as tools/list shows it, schemas inferred.
func Listed(t *testing.T, tl tool.Tool) *sdk.Tool {
	t.Helper()
	s := sdk.NewServer(&sdk.Implementation{Name: "tooltest", Version: "0"}, nil)
	tl.Add(s, tool.Context{Log: slog.New(slog.DiscardHandler)})
	ct, st := sdk.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(res.Tools) != 1 {
		t.Fatalf("tools/list = %+v, %v; want %s alone", res, err, tl.Name)
	}
	return res.Tools[0]
}

// Origins is raws as a run settles them.
func Origins(t *testing.T, raws ...string) v1.Origins {
	t.Helper()
	var opts []origins.Option
	for _, raw := range raws {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		opts = append(opts, origins.WithURL(u))
	}
	return origins.New(opts...)
}
