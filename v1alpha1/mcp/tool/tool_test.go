package tool

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/auth"
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
