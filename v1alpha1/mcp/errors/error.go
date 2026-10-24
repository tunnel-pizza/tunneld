// Package errors is how an MCP tool answers when it refuses a call: each
// tool's Err answers a refusals.Refusal as a tool error, for the model to
// read, not a JSON-RPC error. Logging the call as refused is the caller's.
package errors

import (
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/errors/refusals"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// answer is err as a tool's answer: a refusal a tool error with a text for
// each of its reasons and the empty output; anything else the Go error it is.
func answer[Out any](err error) (*sdk.CallToolResult, Out, error) {
	var out Out
	var refusal refusals.Refusal
	if !errors.As(err, &refusal) {
		return nil, out, err
	}
	content := make([]sdk.Content, len(refusal))
	for i, reason := range refusal {
		content[i] = &sdk.TextContent{Text: reason.Error()}
	}
	return &sdk.CallToolResult{IsError: true, Content: content}, out, nil
}

// ErrExec is err as terminal_exec's answer.
func ErrExec(err error) (*sdk.CallToolResult, types.Reply, error) {
	return answer[types.Reply](err)
}

// ErrRead is err as terminal_read's answer.
func ErrRead(err error) (*sdk.CallToolResult, types.Read, error) {
	return answer[types.Read](err)
}

// ErrScreen is err as terminal_screen's answer.
func ErrScreen(err error) (*sdk.CallToolResult, types.Screen, error) {
	return answer[types.Screen](err)
}

// ErrSessions is err as terminal_sessions' answer.
func ErrSessions(err error) (*sdk.CallToolResult, types.Sessions, error) {
	return answer[types.Sessions](err)
}

// ErrWait is err as terminal_wait's answer.
func ErrWait(err error) (*sdk.CallToolResult, types.Reply, error) {
	return answer[types.Reply](err)
}
