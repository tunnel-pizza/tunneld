package errors

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors/refusals"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
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

// noTerminal is what refusals.NoTerminal says.
var noTerminal = []string{"This run serves no terminal: it has no program (exec) or container (attach) to type into", "A terminal is there once tunneld runs a program or attaches a container; terminal_sessions with scheme http lists the web addresses it serves, if any"}

// TestWrapped pins that a refusal wrapped by a check still answers as one.
func TestWrapped(t *testing.T) {
	r, _, err := ErrWait(fmt.Errorf("checking: %w", refusals.NoTerminal()))
	if err != nil || !slices.Equal(texts(t, r), noTerminal) {
		t.Errorf("ErrWait(a wrapped refusal) = %+v, %v; want its text, no Go error", r, err)
	}
}

// TestAnswers pins each tool's Err: a refusal is a tool error with the empty
// output and no Go error, and any other error is the Go error it is.
func TestAnswers(t *testing.T) {
	refusal, other := refusals.NoTerminal(), fmt.Errorf("broke")
	check := func(name string, r *sdk.CallToolResult, empty bool, err error, wantErr error) {
		t.Helper()
		if wantErr == nil {
			if err != nil || !empty || !slices.Equal(texts(t, r), noTerminal) {
				t.Errorf("%s(refusal) = %+v, empty output %v, %v; want the tool error, the empty output, no Go error", name, r, empty, err)
			}
			return
		}
		if r != nil || err != wantErr {
			t.Errorf("%s(other) = %+v, %v; want no result and the Go error", name, r, err)
		}
	}
	for _, err := range []error{refusal, other} {
		var want error
		if err == other {
			want = other
		}
		r, reply, e := ErrExec(err)
		check("ErrExec", r, reply == types.Reply{}, e, want)
		r, added, e := ErrOriginAdd(err)
		check("ErrOriginAdd", r, reflect.DeepEqual(added, types.Origin{}), e, want)
		r, removed, e := ErrOriginRemove(err)
		check("ErrOriginRemove", r, reflect.DeepEqual(removed, types.Origin{}), e, want)
		r, read, e := ErrRead(err)
		check("ErrRead", r, read == types.Read{}, e, want)
		r, screen, e := ErrScreen(err)
		check("ErrScreen", r, screen.Rows == nil, e, want)
		r, sessions, e := ErrSessions(err)
		check("ErrSessions", r, sessions.Sessions == nil, e, want)
		r, reply, e = ErrWait(err)
		check("ErrWait", r, reply == types.Reply{}, e, want)
	}
}
