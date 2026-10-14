// Package wait is the terminal_wait tool: what one of the run's terminals
// prints from an offset, waiting until it prints what was asked for, falls
// quiet, or the wait runs out.
package wait

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

// Tool is terminal_wait.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_wait",
	Title:       "Wait for a terminal's output",
	Description: "What the program in the terminal at url prints after since, as text with control sequences dropped, waiting until until (a regular expression) matches it, the program prints nothing for idle_ms (500 by default, 0 never), or timeout_ms passes (10000 by default, at most 25000). matched and timed_out say which; neither means it fell quiet. Pass next from terminal_send as since to get a command's reply, and next from this answer to keep reading. dropped says output after since is no longer kept. Text past 64 KiB keeps its end and says truncated. url is the terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none. Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
}, run)

// In is what terminal_wait takes.
type In struct {
	URL       string `json:"url" jsonschema:"The terminal's address on the tunnel: https://hostname/. If the tunnel presents multiple origins, add the terminal's index from terminal_sessions as a bare query parameter: https://hostname/?1, not ?n=1. The first origin, index 0, needs none."`
	Since     int64  `json:"since" jsonschema:"the offset to read after: next from terminal_send, terminal_read or an earlier wait"`
	Until     string `json:"until,omitempty" jsonschema:"a regular expression (RE2) that ends the wait once the text after since matches it"`
	IdleMs    *int   `json:"idle_ms,omitempty" jsonschema:"end once the program has printed nothing for this long, in milliseconds: 500 when left out, 0 never"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"end after this long, in milliseconds: 10000 when left out, at most 25000"`
}

// Out is terminal_wait's answer.
type Out struct {
	attach.Output
	Matched   bool `json:"matched" jsonschema:"whether until matched"`
	TimedOut  bool `json:"timed_out" jsonschema:"whether timeout_ms passed first"`
	Truncated bool `json:"truncated" jsonschema:"whether text was cut to its last 64 KiB"`
}

// run waits on the output of the terminal url names.
func run(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
	start := time.Now()
	var until *regexp.Regexp
	if in.Until != "" {
		var err error
		if until, err = regexp.Compile(in.Until); err != nil {
			c.Logged(ctx, "terminal_wait", "refused", start)
			return tool.Failed("until is not a regular expression: " + err.Error() + "."), Out{}, nil
		}
	}
	term, refusal := c.Terminal(ctx, "terminal_wait", in.URL, start)
	if refusal != nil {
		return refusal, Out{}, nil
	}
	idle := defaultIdle
	if in.IdleMs != nil {
		idle = time.Duration(max(0, *in.IdleMs)) * time.Millisecond
	}
	timeout := defaultTimeout
	if in.TimeoutMs > 0 {
		timeout = min(time.Duration(in.TimeoutMs)*time.Millisecond, maxTimeout)
	}
	waiting, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	got, matched := term.Wait(waiting, in.Since, until, idle)
	out := Out{Output: got, Matched: matched, TimedOut: !matched && errors.Is(waiting.Err(), context.DeadlineExceeded)}
	out.Text, out.Truncated = tool.Capped(out.Text)
	c.Logged(ctx, "terminal_wait", "read", start)
	return nil, out, nil
}
