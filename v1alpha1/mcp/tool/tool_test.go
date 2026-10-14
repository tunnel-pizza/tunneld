package tool

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
)

// logged is a Context logging into a buffer, and what it has logged so far.
func logged(t *testing.T) (Context, func() string) {
	t.Helper()
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil))
	return Context{Log: log}, func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestFailed pins a tool error: an error result carrying the text, for the
// model to read.
func TestFailed(t *testing.T) {
	r := Failed("nope")
	if !r.IsError || len(r.Content) != 1 || r.Content[0].(*sdk.TextContent).Text != "nope" {
		t.Errorf("Failed = %+v", r)
	}
}

// TestRefused pins the scope check: the secret, and a token holding the
// scope, go on; a token without it gets a tool error naming the scope, and
// the call is logged as refused.
func TestRefused(t *testing.T) {
	c, got := logged(t)
	with := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Scopes: []string{"tunnel:exec"}})
	without := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Scopes: []string{"mcp"}})
	if r := c.Refused(t.Context(), "a_tool", "tunnel:exec", time.Now()); r != nil {
		t.Errorf("the secret's call: %+v, want none", r)
	}
	if r := c.Refused(with, "a_tool", "tunnel:exec", time.Now()); r != nil {
		t.Errorf("a token holding the scope: %+v, want none", r)
	}
	r := c.Refused(without, "a_tool", "tunnel:exec", time.Now())
	if r == nil || !strings.Contains(r.Content[0].(*sdk.TextContent).Text, "a_tool needs the tunnel:exec scope") {
		t.Errorf("a token without the scope: %+v, want a refusal naming it", r)
	}
	if !strings.Contains(got(), "ended=refused") {
		t.Errorf("log %q, want the refusal", got())
	}
}

// TestLogged pins the log: one line per call naming the tool, the person
// and the client acting for them, and how it ended.
func TestLogged(t *testing.T) {
	c, got := logged(t)
	ctx := auth.WithAccess(context.Background(), auth.Access{Sub: "github:1", Actor: "https://claude.ai/client.json"})
	c.Logged(ctx, "a_tool", "ok", time.Now())
	for _, s := range []string{"tool=a_tool", "sub=github:1", "actor=https://claude.ai/client.json", "ended=ok"} {
		if !strings.Contains(got(), s) {
			t.Errorf("log %q lacks %s", got(), s)
		}
	}
	if n := strings.Count(got(), "\n"); n != 1 {
		t.Errorf("log has %d lines, want 1: %q", n, got())
	}
}

// TestAll pins a Context with no origins: none, rather than a nil to test
// for at every tool.
func TestAll(t *testing.T) {
	if got := (Context{}).All(); got != nil {
		t.Errorf("All() = %v, want none", got)
	}
}

// served is a run's served origins as a test arranges them, terminals by
// index.
type served struct {
	attach.Bound // the rest, which a tool never asks of it
	at           map[int]attach.Terminal
}

func (b served) Terminal(index int) (attach.Terminal, bool) {
	t, ok := b.at[index]
	return t, ok
}

// TestTerminal pins how a tool reaches a terminal: url names an origin of
// the run, that origin is served with a terminal, and the caller holds the
// scope of its scheme. Each refusal is a tool error, logged.
func TestTerminal(t *testing.T) {
	var opts []origins.Option
	for _, raw := range []string{"http://localhost:3000", "exec:///bin/sh", "attach://dockerd/api"} {
		u, _ := url.Parse(raw)
		opts = append(opts, origins.WithURL(u))
	}
	type terminal struct {
		attach.Terminal
		name string
	}
	shell, container := attach.Terminal(terminal{name: "sh"}), attach.Terminal(terminal{name: "api"})
	run := served{at: map[int]attach.Terminal{1: shell, 2: container}}
	exec := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Scopes: []string{"tunnel:exec"}})
	for name, tc := range map[string]struct {
		c    Context
		ctx  context.Context
		url  string
		want attach.Terminal
		says string
	}{
		"the secret":                       {Context{Origins: origins.New(opts...), Bound: run}, t.Context(), "https://0tabc.tunneled.pizza/?1", shell, ""},
		"the scheme's scope":               {Context{Origins: origins.New(opts...), Bound: run}, exec, "https://0tabc.tunneled.pizza/?1", shell, ""},
		"another scheme's scope":           {Context{Origins: origins.New(opts...), Bound: run}, exec, "https://0tabc.tunneled.pizza/?2", nil, "a_tool needs the tunnel:attach scope"},
		"an origin that is not a terminal": {Context{Origins: origins.New(opts...), Bound: run}, exec, "https://0tabc.tunneled.pizza/", nil, "https://0tabc.tunneled.pizza/ is not a terminal: origin 0 is http"},
		"past the list":                    {Context{Origins: origins.New(opts...), Bound: run}, exec, "https://0tabc.tunneled.pizza/?5", nil, "names origin 5"},
		"a run serving nothing":            {Context{Origins: origins.New(opts...)}, t.Context(), "https://0tabc.tunneled.pizza/?1", nil, "This run serves no terminal."},
		"a run with no origins":            {Context{Bound: run}, t.Context(), "https://0tabc.tunneled.pizza/?1", nil, "This run serves no terminal."},
	} {
		t.Run(name, func(t *testing.T) {
			c, got := logged(t)
			tc.c.Log = c.Log
			term, refusal := tc.c.Terminal(tc.ctx, "a_tool", tc.url, time.Now())
			if term != tc.want {
				t.Errorf("Terminal = %v, want %v", term, tc.want)
			}
			if tc.says == "" {
				if refusal != nil {
					t.Errorf("refusal = %+v, want none", refusal)
				}
				return
			}
			if refusal == nil || !refusal.IsError || !strings.Contains(refusal.Content[0].(*sdk.TextContent).Text, tc.says) {
				t.Errorf("refusal = %+v, want a tool error saying %q", refusal, tc.says)
			}
			if !strings.Contains(got(), "ended=refused") {
				t.Errorf("log %q, want the refusal", got())
			}
		})
	}
}

// TestCapped pins the bound on output an agent is handed: text within
// MaxText is as it is, and longer text keeps its end, from a whole
// character, and says it was cut.
func TestCapped(t *testing.T) {
	if got, cut := Capped("short"); got != "short" || cut {
		t.Errorf("Capped(short) = %q, %v", got, cut)
	}
	long := "✳" + strings.Repeat("x", MaxText-2) + "end"
	got, cut := Capped(long)
	if !cut || !strings.HasSuffix(got, "end") || len(got) > MaxText || !utf8.ValidString(got) {
		t.Errorf("Capped(long) = %d bytes ending %q, %v; want at most %d valid bytes ending end, cut", len(got), got[len(got)-3:], cut, MaxText)
	}
}
