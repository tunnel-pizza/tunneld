// Package output is what the terminal tools that read a program's output
// share: when a wait ends (WaitFor), what it answers (Reply), the wait
// itself, and the tool error for a cursor a terminal refused.
package output

import (
	"context"
	"errors"
	"regexp"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
)

// Waits stay short: the relay holds one request per call, and the edge ends
// one that writes nothing for about 100 seconds.
const (
	defaultIdle    = 500 * time.Millisecond
	defaultTimeout = 10 * time.Second
	maxTimeout     = 25 * time.Second
)

// WaitFor is when a terminal tool that waits for output stops: until a
// pattern matches what it read, the program falls quiet, or the wait runs
// out.
type WaitFor struct {
	Until     string `json:"until,omitempty" jsonschema:"a regular expression (RE2) that ends the wait once the text read matches it"`
	IdleMs    *int   `json:"idle_ms,omitempty" jsonschema:"end once the program has printed nothing for this long, in milliseconds: 500 when left out, 0 never"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"end after this long, in milliseconds: 10000 when left out, at most 25000"`
}

// Pattern is Until compiled, nil when there is none.
func (w WaitFor) Pattern() (*regexp.Regexp, error) {
	if w.Until == "" {
		return nil, nil
	}
	return regexp.Compile(w.Until)
}

// Reply is what a terminal tool that waits answers: the output read, and
// how the wait ended.
type Reply struct {
	attach.Output
	Matched   bool `json:"matched" jsonschema:"whether until matched"`
	TimedOut  bool `json:"timed_out" jsonschema:"whether timeout_ms passed first; neither this nor matched means the program fell quiet"`
	Truncated bool `json:"truncated" jsonschema:"whether text was cut to its last 64 KiB"`
}

// Wait waits on term for reader from cursor, or reader's position when
// cursor is "", as w says, or answers the tool error why not, logged: a
// pattern that does not compile, or a cursor not the terminal's.
func Wait(c tool.Context, ctx context.Context, name string, term attach.Terminal, reader, cursor string, w WaitFor, start time.Time) (Reply, *sdk.CallToolResult) {
	until, err := w.Pattern()
	if err != nil {
		c.Logged(ctx, name, "refused", start)
		return Reply{}, tool.Failed("until is not a regular expression: " + err.Error() + ".")
	}
	idle := defaultIdle
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
		return Reply{}, Unreadable(c, ctx, name, err, start)
	}
	reply := Reply{Output: got, Matched: matched, TimedOut: !matched && errors.Is(waiting.Err(), context.DeadlineExceeded)}
	reply.Text, reply.Truncated = tool.Capped(reply.Text)
	return reply, nil
}

// Unreadable is the tool error for a read the terminal refused, logged: a
// cursor not its own, from another terminal or before tunneld started
// again, or one no terminal gave.
func Unreadable(c tool.Context, ctx context.Context, name string, err error, start time.Time) *sdk.CallToolResult {
	c.Logged(ctx, name, "refused", start)
	if errors.Is(err, attach.ErrStaleCursor) {
		return tool.Failed("That cursor is not this terminal's: it came from another terminal, or from before tunneld started again, so the output it points into is gone. Leave cursor out to read on from where this sign-in last stopped.")
	}
	if errors.Is(err, attach.ErrBadCursor) {
		return tool.Failed("That is not a cursor: pass back one a terminal tool answered, or leave it out.")
	}
	return tool.Failed(err.Error() + ".")
}
