package tooltest

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
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
