// Package output is waiting on a terminal's output, which terminal_wait and
// terminal_exec share: when a wait stops (For), and the wait itself (Wait).
package output

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors/refusals"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// Waits stay short: the relay holds one request per call, and the edge ends
// one that writes nothing for about 100 seconds.
const (
	defaultIdle    = 500 * time.Millisecond
	defaultTimeout = 10 * time.Second
	maxTimeout     = 25 * time.Second
)

// For is when a wait stops: until a pattern matches what it read, the
// program falls quiet, or the wait runs out.
type For struct {
	Until     string `json:"until,omitempty" jsonschema:"a regular expression (RE2) that ends the wait once the text read, or a full-screen program's rows, match it; \\s is ASCII only, \\p{Zs} matches any space"`
	IdleMs    *int   `json:"idle_ms,omitempty" jsonschema:"end once the program has printed nothing, or a full-screen program's screen has not changed, for this long, in milliseconds: left out, 500 without until and 0 with it; 0 never"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"end after this long, in milliseconds: 10000 when left out, at most 25000"`
}

// Pattern is Until compiled, nil when there is none.
func (w For) Pattern() (*regexp.Regexp, error) {
	if w.Until == "" {
		return nil, nil
	}
	return regexp.Compile(w.Until)
}

// Wait waits on term for reader from cursor, or reader's position when
// cursor is "", until until matches, or as w's idle and timeout say; or
// answers why not, a cursor not the terminal's, logged as name.
//
// until is w's Pattern, compiled by the caller, which refuses one that does
// not compile before it does anything else.
func Wait(c tool.Context, ctx context.Context, name string, term attach.Terminal, reader, cursor string, until *regexp.Regexp, w For, start time.Time) (types.Reply, error) {
	idle := defaultIdle
	if until != nil {
		idle = 0
	}
	if w.IdleMs != nil {
		idle = time.Duration(max(0, *w.IdleMs)) * time.Millisecond
	}
	timeout := defaultTimeout
	if w.TimeoutMs > 0 {
		timeout = min(time.Duration(w.TimeoutMs)*time.Millisecond, maxTimeout)
	}
	waiting, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	got, matched, err := term.Wait(waiting, reader, cursor, until, idle)
	if err != nil {
		c.Logged(ctx, name, "refused", start)
		return types.Reply{}, refusals.Unreadable(err)
	}
	reply := types.Reply{Output: got, Matched: matched, TimedOut: !matched && errors.Is(waiting.Err(), context.DeadlineExceeded)}
	reply.Text, reply.Truncated = tool.Capped(reply.Text)
	return reply, nil
}
