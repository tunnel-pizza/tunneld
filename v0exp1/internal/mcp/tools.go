package mcp

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxOutput        = 1 << 20 // 1 MiB each of stdout and stderr
	defaultTimeoutMs = 60_000
	maxTimeoutMs     = 600_000
)

type originRow struct {
	N      int    `json:"n"`
	Origin string `json:"origin"`
	Kind   Kind   `json:"kind"`
}

// listOrigins is the origins tool: every origin of the run, index first,
// whether or not the other tools can do anything with it.
func (s *server) listOrigins(context.Context, *sdk.CallToolRequest, any) (*sdk.CallToolResult, []originRow, error) {
	rows := make([]originRow, len(s.origins))
	for i, o := range s.origins {
		rows[i] = originRow{N: i, Origin: o.Name, Kind: o.Kind}
	}
	return nil, rows, nil
}

type execIn struct {
	N         int      `json:"n" jsonschema:"origin index, as origins lists"`
	Argv      []string `json:"argv" jsonschema:"the program and its arguments; on a shell origin, [\"-c\", \"...\"]"`
	Stdin     string   `json:"stdin,omitempty" jsonschema:"bytes for the program's stdin"`
	TimeoutMs int      `json:"timeout_ms,omitempty" jsonschema:"kill the program after this long; default 60000, at most 600000"`
}

type execOut struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code" jsonschema:"the program's exit code; -1 when it was killed, at the timeout or by a signal"`
	Truncated bool   `json:"truncated" jsonschema:"true when stdout or stderr passed 1 MiB and the rest was dropped"`
}

// exec is the exec tool: argv once, privately, on origin n.
func (s *server) exec(ctx context.Context, _ *sdk.CallToolRequest, in execIn) (*sdk.CallToolResult, execOut, error) {
	o, err := s.origin(in.N)
	if err != nil {
		return nil, execOut{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(clampTimeout(in.TimeoutMs))*time.Millisecond)
	defer cancel()
	// A nil reader when there is nothing to send: the program's stdin is then
	// closed at once, rather than a pipe that is.
	var stdin io.Reader
	if in.Stdin != "" {
		stdin = strings.NewReader(in.Stdin)
	}
	out, errb := newCapped(maxOutput), newCapped(maxOutput)
	start := time.Now()
	exit, err := o.Spawner.Spawn(ctx, in.Argv, stdin, out, errb)
	s.log.Debug("mcp exec", "origin", in.N, "argv", in.Argv, "exit", exit, "took", time.Since(start), "error", err)
	if err != nil {
		return nil, execOut{}, err
	}
	return nil, execOut{
		Stdout:    out.text(),
		Stderr:    errb.text(),
		ExitCode:  exit,
		Truncated: out.truncated || errb.truncated,
	}, nil
}

// clampTimeout is timeout_ms as asked, made sane: zero or less is the
// default, past the maximum is the maximum — an agent that asks for an hour
// gets ten minutes rather than a refusal.
func clampTimeout(ms int) int {
	switch {
	case ms <= 0:
		return defaultTimeoutMs
	case ms > maxTimeoutMs:
		return maxTimeoutMs
	}
	return ms
}

// capped is a writer that keeps the first max bytes and drops the rest,
// remembering that it did. Safe to write from the process's own goroutines.
type capped struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newCapped(max int) *capped { return &capped{max: max} }

// Write accepts every byte, kept or dropped: a short count would make
// io.Copy stop with io.ErrShortWrite and the program's pipe close under it.
func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := c.max - c.buf.Len(); len(p) > room {
		c.truncated = true
		p = p[:room]
	}
	c.buf.Write(p)
	return n, nil
}

// text is what was kept, made valid UTF-8: JSON cannot carry a stray byte,
// and a program's output is not always text.
func (c *capped) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.ToValidUTF8(c.buf.String(), "\uFFFD")
}

// take is what arrived since the last take, as text, and the buffer is
// emptied: the cap is on what waits unread, so a session that is read keeps
// printing.
func (c *capped) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := strings.ToValidUTF8(c.buf.String(), "\uFFFD")
	c.buf.Reset()
	c.truncated = false
	return s
}
