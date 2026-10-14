// Package tooltest stands one tool up on a server of its own, served over
// streamable HTTP the way the run serves it, for that tool's tests.
package tooltest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
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

// Bound is a run's served origins as a test arranges them: its terminals,
// by index.
type Bound struct {
	attach.Bound // the rest, which a tool never asks of it
	Terminals    map[int]attach.Terminal
}

// Terminal is the terminal at index.
func (b Bound) Terminal(index int) (attach.Terminal, bool) {
	t, ok := b.Terminals[index]
	return t, ok
}

// Terminal is a terminal that keeps what it was sent, shows Shows, and has
// printed Prints. Err, when set, is what Send answers, having kept nothing.
type Terminal struct {
	Shows  attach.Screen
	Prints string
	Err    error

	mu     sync.Mutex
	sent   []attach.Input
	waited Waited
}

// Waited is what the last Wait was asked: the offset, the pattern, the
// quiet, and how long its context had left.
type Waited struct {
	Since int64
	Until string
	Idle  time.Duration
	Left  time.Duration
}

// Send keeps input, in order, and answers where Prints ends.
func (t *Terminal) Send(input ...attach.Input) (int64, error) {
	if t.Err != nil {
		return 0, t.Err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append(t.sent, input...)
	return int64(len(t.Prints)), nil
}

// Screen is Shows.
func (t *Terminal) Screen() attach.Screen { return t.Shows }

// Read is Prints after since.
func (t *Terminal) Read(since int64) attach.Output {
	since = max(0, min(since, int64(len(t.Prints))))
	return attach.Output{Text: t.Prints[since:], Next: int64(len(t.Prints))}
}

// Wait keeps what it was asked and answers Read(since) at once, matched
// when until matches it.
func (t *Terminal) Wait(ctx context.Context, since int64, until *regexp.Regexp, idle time.Duration) (attach.Output, bool) {
	w := Waited{Since: since, Idle: idle}
	if until != nil {
		w.Until = until.String()
	}
	if deadline, ok := ctx.Deadline(); ok {
		w.Left = time.Until(deadline)
	}
	t.mu.Lock()
	t.waited = w
	t.mu.Unlock()
	got := t.Read(since)
	return got, until != nil && until.MatchString(got.Text)
}

// Sent is every step Send was given, in order.
func (t *Terminal) Sent() []attach.Input {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]attach.Input(nil), t.sent...)
}

// LastWait is what the last Wait was asked.
func (t *Terminal) LastWait() Waited {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waited
}
