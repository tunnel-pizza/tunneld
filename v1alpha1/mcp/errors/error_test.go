package errors

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// texts is r's texts, failing unless r is a tool error carrying only text.
func texts(t *testing.T, r *sdk.CallToolResult) []string {
	t.Helper()
	if r == nil || !r.IsError || len(r.Content) == 0 {
		t.Fatalf("result = %+v, want a tool error with text", r)
	}
	out := make([]string, len(r.Content))
	for i, c := range r.Content {
		out[i] = c.(*sdk.TextContent).Text
	}
	return out
}

// TestFailed pins a tool error: an error result with each error's text as
// its own content, in order, for the model to read.
func TestFailed(t *testing.T) {
	errs := []error{fmt.Errorf("nope"), fmt.Errorf("try this instead")}
	if got, want := texts(t, Failed(errs...)), []string{"nope", "try this instead"}; !slices.Equal(got, want) {
		t.Errorf("Failed(%v) says %q, want %q", errs, got, want)
	}
}

// TestSays pins what each tool error tells the model: what went wrong, and
// what to do instead where there is something.
func TestSays(t *testing.T) {
	shell, _ := url.Parse("http://localhost:3000")
	for name, tc := range map[string]struct {
		r    *sdk.CallToolResult
		says string
	}{
		"Refused":       {Refused("a_tool", "tunnel:exec"), "a_tool needs the tunnel:exec scope"},
		"NoTerminal":    {NoTerminal(), "serves no terminal"},
		"Unlisted":      {Unlisted(fmt.Errorf("names origin 5")), "names origin 5\nterminal_sessions lists the terminals"},
		"NotATerminal":  {NotATerminal("https://0tabc.tunneld.domain/", v1.Origin{Index: 0, URI: *shell, Scheme: v1.HttpScheme}), "is not a terminal: origin 0 is http"},
		"NotAPattern":   {NotAPattern(fmt.Errorf("missing closing )")), "until is not a regular expression: missing closing )"},
		"NothingToSend": {NothingToSend(), "input needs at least one step"},
		"UnknownScheme": {UnknownScheme(), "exec, attach or http"},
	} {
		if got := strings.Join(texts(t, tc.r), "\n"); !strings.Contains(got, tc.says) {
			t.Errorf("%s says %q, want it to say %q", name, got, tc.says)
		}
	}
}

// TestUnreadable pins the tool error for a cursor a read cannot use, and any
// other read error said as it is.
func TestUnreadable(t *testing.T) {
	for err, says := range map[error]string{
		attach.ErrStaleCursor:           "not this terminal's",
		attach.ErrBadCursor:             "not a cursor",
		fmt.Errorf("the session ended"): "the session ended",
	} {
		if got := strings.Join(texts(t, Unreadable(err)), "\n"); !strings.Contains(got, says) {
			t.Errorf("Unreadable(%v) says %q, want %q", err, got, says)
		}
	}
}
