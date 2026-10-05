package mcp

import (
	"context"
	"errors"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessions is the run's session table.
type sessions struct{}

func newSessions([]Origin, *slog.Logger) *sessions { return &sessions{} }

// Close ends every session.
func (*sessions) Close() error { return nil }

var errNoSessions = errors.New("sessions are not available yet")

type sessionOpenIn struct {
	N    int      `json:"n" jsonschema:"origin index, as origins lists"`
	Argv []string `json:"argv,omitempty" jsonschema:"the program and its arguments; omitted runs the origin's own"`
}

type sessionIDIn struct {
	ID string `json:"id" jsonschema:"the session id session_open returned"`
}

type sessionWriteIn struct {
	ID    string `json:"id" jsonschema:"the session id session_open returned"`
	Stdin string `json:"stdin" jsonschema:"bytes to send to the process's stdin"`
}

type sessionReadIn struct {
	ID        string `json:"id" jsonschema:"the session id session_open returned"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"how long to wait for output when there is none; default 1000"`
}

type sessionOpenOut struct {
	ID string `json:"id"`
}

type sessionReadOut struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Exited   bool   `json:"exited"`
	ExitCode int    `json:"exit_code,omitempty"`
}

func (*sessions) open(context.Context, *sdk.CallToolRequest, sessionOpenIn) (*sdk.CallToolResult, sessionOpenOut, error) {
	return nil, sessionOpenOut{}, errNoSessions
}

func (*sessions) write(context.Context, *sdk.CallToolRequest, sessionWriteIn) (*sdk.CallToolResult, any, error) {
	return nil, nil, errNoSessions
}

func (*sessions) read(context.Context, *sdk.CallToolRequest, sessionReadIn) (*sdk.CallToolResult, sessionReadOut, error) {
	return nil, sessionReadOut{}, errNoSessions
}

func (*sessions) close(context.Context, *sdk.CallToolRequest, sessionIDIn) (*sdk.CallToolResult, any, error) {
	return nil, nil, errNoSessions
}
