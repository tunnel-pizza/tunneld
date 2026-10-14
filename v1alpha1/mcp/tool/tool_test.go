package tool

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
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

// waiting is a terminal that answers Wait with got, matched, err, keeping
// what it was asked.
type waiting struct {
	attach.Terminal // the rest, which Reply never asks of it
	got             attach.Output
	matched         bool
	err             error
	block           bool
	asked           struct {
		reader, cursor, until string
		idle                  time.Duration
		left                  time.Duration
	}
}

func (w *waiting) Wait(ctx context.Context, reader, cursor string, until *regexp.Regexp, idle time.Duration) (attach.Output, bool, error) {
	w.asked.reader, w.asked.cursor, w.asked.idle = reader, cursor, idle
	if until != nil {
		w.asked.until = until.String()
	}
	if deadline, ok := ctx.Deadline(); ok {
		w.asked.left = time.Until(deadline)
	}
	if w.block {
		<-ctx.Done()
	}
	return w.got, w.matched, w.err
}

// TestReply pins how a terminal tool waits: the defaults and the cap on
// what it is asked, the answer saying how the wait ended, its text capped,
// and a bad pattern or a cursor not the terminal's a tool error.
func TestReply(t *testing.T) {
	ms := func(n int) *int { return &n }
	for name, tc := range map[string]struct {
		w      WaitFor
		term   *waiting
		idle   time.Duration
		left   time.Duration
		until  string
		reply  Reply
		failed string
	}{
		"defaults":       {WaitFor{}, &waiting{got: attach.Output{Text: "hi", Cursor: "@2"}}, 500 * time.Millisecond, 10 * time.Second, "", Reply{Output: attach.Output{Text: "hi", Cursor: "@2"}}, ""},
		"a match":        {WaitFor{Until: `done: \d`}, &waiting{matched: true}, 500 * time.Millisecond, 10 * time.Second, `done: \d`, Reply{Matched: true}, ""},
		"no quiet":       {WaitFor{IdleMs: ms(0), TimeoutMs: 2000}, &waiting{}, 0, 2 * time.Second, "", Reply{}, ""},
		"past the cap":   {WaitFor{TimeoutMs: 60_000}, &waiting{}, 500 * time.Millisecond, 25 * time.Second, "", Reply{}, ""},
		"timed out":      {WaitFor{TimeoutMs: 1, IdleMs: ms(0)}, &waiting{block: true}, 0, time.Millisecond, "", Reply{TimedOut: true}, ""},
		"a bad pattern":  {WaitFor{Until: "("}, &waiting{}, 0, 0, "", Reply{}, "until is not a regular expression"},
		"a stale cursor": {WaitFor{}, &waiting{err: attach.ErrStaleCursor}, 500 * time.Millisecond, 10 * time.Second, "", Reply{}, "not this terminal's"},
		"a junk cursor":  {WaitFor{}, &waiting{err: attach.ErrBadCursor}, 500 * time.Millisecond, 10 * time.Second, "", Reply{}, "not a cursor"},
		"too much text":  {WaitFor{}, &waiting{got: attach.Output{Text: strings.Repeat("x", MaxText+1)}}, 500 * time.Millisecond, 10 * time.Second, "", Reply{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			c, got := logged(t)
			reply, refusal := c.Reply(t.Context(), "a_tool", tc.term, "me", "@0", tc.w, time.Now())
			if tc.failed != "" {
				if refusal == nil || !strings.Contains(refusal.Content[0].(*sdk.TextContent).Text, tc.failed) || !strings.Contains(got(), "ended=refused") {
					t.Errorf("refusal = %+v, log %q; want a logged tool error saying %q", refusal, got(), tc.failed)
				}
				return
			}
			if refusal != nil {
				t.Fatalf("refusal = %+v, want none", refusal)
			}
			if tc.term.asked.reader != "me" || tc.term.asked.cursor != "@0" || tc.term.asked.idle != tc.idle || tc.term.asked.until != tc.until ||
				tc.term.asked.left > tc.left || tc.term.asked.left < tc.left-time.Second {
				t.Errorf("asked %+v, want idle %v, about %v left, until %q", tc.term.asked, tc.idle, tc.left, tc.until)
			}
			if name == "too much text" {
				if !reply.Truncated || len(reply.Text) != MaxText {
					t.Errorf("reply %d bytes, truncated %v; want %d, truncated", len(reply.Text), reply.Truncated, MaxText)
				}
				return
			}
			if reply != tc.reply {
				t.Errorf("reply = %+v, want %+v", reply, tc.reply)
			}
		})
	}
}

// TestUnreadable pins the tool error for a cursor a read cannot use.
func TestUnreadable(t *testing.T) {
	c, got := logged(t)
	for err, says := range map[error]string{
		attach.ErrStaleCursor: "not this terminal's",
		attach.ErrBadCursor:   "not a cursor",
	} {
		r := c.Unreadable(t.Context(), "a_tool", err, time.Now())
		if r == nil || !r.IsError || !strings.Contains(r.Content[0].(*sdk.TextContent).Text, says) {
			t.Errorf("Unreadable(%v) = %+v, want a tool error saying %q", err, r, says)
		}
	}
	if !strings.Contains(got(), "ended=refused") {
		t.Errorf("log %q, want the refusals", got())
	}
}
