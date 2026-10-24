package tool

import (
	"context"
	"log/slog"
	"net/http"
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
	attach.Attached // the rest, which a tool never asks of it
	at              map[int]attach.Terminal
}

func (b served) Terminal(index int) (attach.Terminal, bool) {
	t, ok := b.at[index]
	return t, ok
}

// TestTerminal pins how a tool reaches a terminal: url names an origin of
// the run, that origin is served with a terminal, and the caller holds the
// scope of its scheme. Each refusal is an error saying why, logged.
func TestTerminal(t *testing.T) {
	var opts []origins.Option
	for _, raw := range []string{"http://localhost:3000", "exec:///bin/sh", "attach://dockerd/api"} {
		u, _ := url.Parse(raw)
		opts = append(opts, origins.WithOrigins(origins.NewOrigin(u)))
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
		"the secret":                       {Context{Origins: origins.New(opts...), Attached: run}, t.Context(), "https://0tabc.tunneld.domain/?1", shell, ""},
		"the scheme's scope":               {Context{Origins: origins.New(opts...), Attached: run}, exec, "https://0tabc.tunneld.domain/?1", shell, ""},
		"another scheme's scope":           {Context{Origins: origins.New(opts...), Attached: run}, exec, "https://0tabc.tunneld.domain/?2", nil, "a_tool needs the tunnel:attach scope"},
		"an origin that is not a terminal": {Context{Origins: origins.New(opts...), Attached: run}, exec, "https://0tabc.tunneld.domain/", nil, "https://0tabc.tunneld.domain/ is not a terminal: origin 0 is http"},
		"past the list":                    {Context{Origins: origins.New(opts...), Attached: run}, exec, "https://0tabc.tunneld.domain/?5", nil, "names origin 5"},
		"a run serving nothing":            {Context{Origins: origins.New(opts...)}, t.Context(), "https://0tabc.tunneld.domain/?1", nil, "This run serves no terminal"},
		"a run with no origins":            {Context{Attached: run}, t.Context(), "https://0tabc.tunneld.domain/?1", nil, "This run serves no terminal"},
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
			if refusal == nil || !strings.Contains(refusal.Error(), tc.says) {
				t.Errorf("refusal = %v, want one saying %q", refusal, tc.says)
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

// TestCaller pins whose read position a call moves: the sign-in's grant,
// so one connected app's; a token with none, its person and the client
// acting; the tunnel's secret, its own.
func TestCaller(t *testing.T) {
	c := Context{}
	signedIn := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Actor: "https://claude.ai/client.json", Grant: "grant-1"})
	again := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Actor: "https://other.example/", Grant: "grant-1"})
	noGrant := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Actor: "https://claude.ai/client.json"})
	otherActor := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Actor: "https://other.example/"})
	if c.Caller(signedIn) != c.Caller(again) {
		t.Error("one grant is one caller, whatever else the token says")
	}
	for name, pair := range map[string][2]context.Context{
		"two grants":               {signedIn, auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Grant: "grant-2"})},
		"a grant and none":         {signedIn, noGrant},
		"no grant, another client": {noGrant, otherActor},
		"a token and the secret":   {noGrant, t.Context()},
	} {
		if c.Caller(pair[0]) == c.Caller(pair[1]) {
			t.Errorf("%s: one caller, want two", name)
		}
	}
	if c.Caller(t.Context()) != c.Caller(context.Background()) {
		t.Error("the secret is one caller")
	}
}

// TestStdio pins how a call over stdio is told from one through the tunnel:
// the HTTP transport hands every request its header, stdio none.
func TestStdio(t *testing.T) {
	if !Stdio(&sdk.CallToolRequest{}) {
		t.Error("a request with no transport extra is not stdio, want it to be")
	}
	if Stdio(&sdk.CallToolRequest{Extra: &sdk.RequestExtra{Header: http.Header{}}}) {
		t.Error("a request carrying an HTTP header is stdio, want it not to be")
	}
}

// TestChangesOrigins pins who may change a run's origins: the stdio client,
// and a token holding tunnel:origins; the tunnel's secret and a token
// without the scope are refused, saying why, and logged.
func TestChangesOrigins(t *testing.T) {
	overHTTP := &sdk.CallToolRequest{Extra: &sdk.RequestExtra{Header: http.Header{}}}
	origins := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:origins"}})
	exec := auth.WithAccess(t.Context(), auth.Access{Sub: "github:1", Scopes: []string{"mcp", "tunnel:exec"}})
	for name, tc := range map[string]struct {
		ctx  context.Context
		req  *sdk.CallToolRequest
		says string
	}{
		"the stdio client":            {t.Context(), &sdk.CallToolRequest{}, ""},
		"a token with tunnel:origins": {origins, overHTTP, ""},
		"the tunnel's secret":         {t.Context(), overHTTP, "The tunnel's secret can't call a_tool"},
		"a token without the scope":   {exec, overHTTP, "a_tool needs the tunnel:origins scope"},
	} {
		t.Run(name, func(t *testing.T) {
			c, got := logged(t)
			err := c.ChangesOrigins(tc.ctx, tc.req, "a_tool", time.Now())
			if tc.says == "" {
				if err != nil {
					t.Errorf("ChangesOrigins = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.says) || !strings.Contains(got(), "ended=refused") {
				t.Errorf("ChangesOrigins = %v, log %q; want a logged refusal saying %q", err, got(), tc.says)
			}
		})
	}
}
