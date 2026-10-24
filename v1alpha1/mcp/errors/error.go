// Package errors is every tool error an MCP tool answers: an error result
// carrying text for the model to read, not a JSON-RPC error. Each says what
// went wrong and what to do instead. Logging the call as refused is the
// caller's.
package errors

import (
	"errors"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// listed is what to do about an address that reaches no terminal.
var listed = errors.New("terminal_sessions lists the terminals")

// Failed is a tool error saying errs, one text each, in order.
func Failed(errs ...error) *sdk.CallToolResult {
	content := make([]sdk.Content, len(errs))
	for i, err := range errs {
		content[i] = &sdk.TextContent{Text: err.Error()}
	}
	return &sdk.CallToolResult{IsError: true, Content: content}
}

// Refused is the tool error for a sign-in whose token does not hold scope,
// which tool needs.
func Refused(tool, scope string) *sdk.CallToolResult {
	return Failed(fmt.Errorf("This sign-in can't use the terminal: %s needs the %s scope", tool, scope))
}

// NoTerminal is the tool error for a run with nothing a terminal tool can
// reach.
func NoTerminal() *sdk.CallToolResult {
	return Failed(errors.New("This run serves no terminal"))
}

// Unlisted is the tool error for an address that names no origin of the run.
func Unlisted(err error) *sdk.CallToolResult {
	return Failed(err, listed)
}

// NotATerminal is the tool error for url, which names origin, an origin the
// run dials rather than serves with a terminal.
func NotATerminal(url string, origin v1.Origin) *sdk.CallToolResult {
	return Failed(fmt.Errorf("%s is not a terminal: origin %d is %s", url, origin.Index, origin.Scheme), listed)
}

// NotAPattern is the tool error for an until that does not compile.
func NotAPattern(err error) *sdk.CallToolResult {
	return Failed(fmt.Errorf("until is not a regular expression: %w", err))
}

// NothingToSend is the tool error for input with no steps.
func NothingToSend() *sdk.CallToolResult {
	return Failed(errors.New("Nothing to send: input needs at least one step"))
}

// UnknownScheme is the tool error for a scheme no origin can have.
func UnknownScheme() *sdk.CallToolResult {
	return Failed(errors.New("scheme must be exec, attach or http"))
}

// Unreadable is the tool error for a read the terminal refused: a cursor not
// its own, from another terminal or before tunneld started again, or one no
// terminal gave.
func Unreadable(err error) *sdk.CallToolResult {
	switch {
	case errors.Is(err, attach.ErrStaleCursor):
		return Failed(errors.New("That cursor is not this terminal's: it came from another terminal, or from before tunneld started again, so the output it points into is gone"),
			errors.New("Leave cursor out to read on from where this sign-in last stopped"))
	case errors.Is(err, attach.ErrBadCursor):
		return Failed(errors.New("That is not a cursor: pass back one a terminal tool answered, or leave it out"))
	}
	return Failed(err)
}
