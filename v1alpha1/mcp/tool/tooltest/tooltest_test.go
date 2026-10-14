package tooltest

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

type in struct {
	Say string `json:"say"`
}

type out struct {
	Said    string `json:"said"`
	Who     string `json:"who"`
	Origins int    `json:"origins"`
}

// echo answers what it was told, who asked, and how many origins it reads.
var echo = tool.New(sdk.Tool{Name: "echo", Description: "echo"}, func(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, i in) (*sdk.CallToolResult, out, error) {
	access, _ := auth.AccessOf(ctx)
	return nil, out{Said: i.Say, Who: access.Sub, Origins: len(c.All())}, nil
})

// TestCall pins Call: the tool runs over HTTP with the Context it is given
// and the access on the request, and the answer comes back as sent.
func TestCall(t *testing.T) {
	c := tool.Context{Origins: Origins(t, "http://localhost:3000", "exec:///bin/sh")}
	status, body := Call(t, echo, c, &auth.Access{Sub: "github:1"}, `{"say":"hi"}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	for _, s := range []string{`\"said\":\"hi\"`, `\"who\":\"github:1\"`, `\"origins\":2`} {
		if !strings.Contains(body, s) {
			t.Errorf("body %s lacks %s", body, s)
		}
	}
}

// TestListed pins Listed: the tool as tools/list shows it, its input schema
// inferred.
func TestListed(t *testing.T) {
	got := Listed(t, echo)
	if got.Name != "echo" || got.InputSchema == nil {
		t.Errorf("Listed = %+v", got)
	}
}

// TestOrigins pins Origins: the URLs as a run settles them, in order.
func TestOrigins(t *testing.T) {
	o := Origins(t, "http://localhost:3000", "exec:///bin/sh")
	if o.Len() != 2 || o.At(1).Scheme != v1.ExecScheme || o.At(1).Ref != "/bin/sh" {
		t.Errorf("Origins = %+v", o.All())
	}
}

// TestBound pins the fake run: a terminal at the index it was given, none
// elsewhere.
func TestBound(t *testing.T) {
	term := &Terminal{}
	b := Bound{Terminals: map[int]attach.Terminal{1: term}}
	if got, ok := b.Terminal(1); !ok || got != term {
		t.Errorf("Terminal(1) = %v, %v; want the one given", got, ok)
	}
	if got, ok := b.Terminal(0); ok || got != nil {
		t.Errorf("Terminal(0) = %v, %v; want none", got, ok)
	}
}

// TestTerminal pins the fake terminal: it keeps every step in order and
// shows what it was given, and with Err keeps nothing.
func TestTerminal(t *testing.T) {
	term := &Terminal{Shows: attach.Screen{Title: "sh"}}
	_, _ = term.Send("a", attach.Input{Text: "ls"}, attach.Input{Key: "Enter"})
	_, _ = term.Send("a", attach.Input{Key: "Ctrl-C"})
	if want := []attach.Input{{Text: "ls"}, {Key: "Enter"}, {Key: "Ctrl-C"}}; !slices.Equal(term.Sent(), want) {
		t.Errorf("Sent = %q, want %q", term.Sent(), want)
	}
	if term.Screen().Title != "sh" {
		t.Errorf("Screen = %+v, want the one given", term.Screen())
	}
	failing := &Terminal{Err: attach.ErrUnknownKey}
	if _, err := failing.Send("a", attach.Input{Text: "ls"}); !errors.Is(err, attach.ErrUnknownKey) || failing.Sent() != nil {
		t.Errorf("Send = %v with %q kept, want Err and nothing", err, failing.Sent())
	}
}

// TestTerminalPrints pins the fake's log: Send answers the cursor where
// Prints ended, moves the reader there, and appends Reply; Read is Prints
// from a cursor or the reader's position, moving it; a stale or junk cursor
// is refused; and Wait keeps what it was asked and matches until.
func TestTerminalPrints(t *testing.T) {
	term := &Terminal{Prints: "$ ls\n", Reply: "go.mod\n"}
	sent, err := term.Send("a", attach.Input{Key: "Enter"})
	if err != nil || sent != "@5" {
		t.Errorf("Send = %q, %v; want @5, where Prints ended", sent, err)
	}
	if got, err := term.Read("a", ""); err != nil || got.Text != "go.mod\n" || got.Cursor != "@12" {
		t.Errorf("a's Read = %+v, %v; want the reply, up to @12", got, err)
	}
	if got, _ := term.Read("b", ""); got.Text != "$ ls\ngo.mod\n" {
		t.Errorf("b's Read = %+v, want everything: a's position is not b's", got)
	}
	if got, _ := term.Read("a", "@2"); got.Text != "ls\ngo.mod\n" {
		t.Errorf("Read from @2 = %+v", got)
	}
	if _, err := term.Read("a", "stale"); !errors.Is(err, attach.ErrStaleCursor) {
		t.Errorf("a stale cursor: %v, want ErrStaleCursor", err)
	}
	if _, err := term.Read("a", "junk"); !errors.Is(err, attach.ErrBadCursor) {
		t.Errorf("a junk cursor: %v, want ErrBadCursor", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	got, matched, err := term.Wait(ctx, "c", "@0", regexp.MustCompile(`go\.mod`), time.Second)
	if err != nil || !matched || got.Text != term.Prints {
		t.Errorf("Wait = %+v, %v, %v; want Prints, matched", got, matched, err)
	}
	if w := term.LastWait(); w.Reader != "c" || w.Cursor != "@0" || w.Until != `go\.mod` || w.Idle != time.Second || w.Left <= 0 || w.Left > time.Minute {
		t.Errorf("LastWait = %+v, want what Wait was asked", w)
	}
}
