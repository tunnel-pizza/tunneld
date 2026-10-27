// Package exec is the terminal_exec tool: running something in one of the
// run's terminals, typed as the person at it would, with the files it needs
// written first, and reading the reply.
package exec

import (
	"context"
	"slices"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors/refusals"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool/output"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// Tool is terminal_exec.
var Tool = tool.New(sdk.Tool{
	Name:        "terminal_exec",
	Title:       "Run in a terminal",
	Description: "Types input into a terminal on this machine, at url, then waits for the reply as terminal_wait does. It first writes attachments, files such as a script, to this machine; that write is not shown in the terminal, so the command should name what it runs (sh /tmp/probe.sh). Needs the scope of the origin's scheme: tunnel:exec for a program.",
	Annotations: &sdk.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(false)},
}, run())

// In is what terminal_exec takes.
type In struct {
	URL         string         `json:"url" jsonschema:"the address of a terminal on this machine: https://hostname/, or https://hostname/?n for the origin at index n"`
	Attachments []Attachment   `json:"attachments,omitempty" jsonschema:"files written to this machine before anything is typed, 1 MiB in all"`
	Input       []attach.Input `json:"input,omitempty" jsonschema:"what to send, in order: each step exactly one of text or key"`
	Command     string         `json:"command,omitempty" jsonschema:"a line typed after input, then Enter"`
	output.For
}

// run writes the attachments, types the input and then the command into the
// terminal url names, then waits for the reply from where this sign-in last
// read (where the input went in, the first time), unless told to answer at
// once or there was nothing to type.
func run() tool.RunFunc[In, types.Reply] {
	return func(c tool.Context, ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, types.Reply, error) {
		start := time.Now()
		if len(in.Input) == 0 && in.Command == "" && len(in.Attachments) == 0 {
			c.Logged(ctx, "terminal_exec", "refused", start)
			return errors.ErrExec(refusals.NothingToSend())
		}
		// Checked before anything is typed: a pattern refused after would leave
		// the input sent and its reply unread.
		until, err := in.Pattern()
		if err != nil {
			c.Logged(ctx, "terminal_exec", "refused", start)
			return errors.ErrExec(refusals.NotAPattern(err))
		}
		term, err := c.Terminal(ctx, "terminal_exec", in.URL, start)
		if err != nil {
			return errors.ErrExec(err)
		}
		files, err := prepare(in.Attachments)
		if err != nil {
			c.Logged(ctx, "terminal_exec", "refused", start)
			return errors.ErrExec(refusals.BadAttachment(err))
		}
		if err := write(files); err != nil {
			c.Logged(ctx, "terminal_exec", "refused", start)
			return errors.ErrExec(refusals.Unwritable(err))
		}
		input := in.Input
		if in.Command != "" {
			input = append(slices.Clip(input), attach.Input{Text: in.Command}, attach.Input{Key: "Enter"})
		}
		if len(input) == 0 {
			c.Logged(ctx, "terminal_exec", "wrote", start)
			return nil, types.Reply{}, nil
		}
		reader := c.Caller(ctx)
		sent, err := term.Send(reader, input...)
		if err != nil {
			c.Logged(ctx, "terminal_exec", "refused", start)
			return errors.ErrExec(refusals.Unsendable(err))
		}
		if in.Until == "" && in.IdleMs != nil && *in.IdleMs == 0 {
			c.Logged(ctx, "terminal_exec", "sent", start)
			return nil, types.Reply{Output: attach.Output{Cursor: sent}}, nil
		}
		reply, err := output.Wait(c, ctx, "terminal_exec", term, reader, sent, until, in.For, start)
		if err != nil {
			return errors.ErrExec(err)
		}
		c.Logged(ctx, "terminal_exec", "sent", start)
		return nil, reply, nil
	}
}
