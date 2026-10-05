package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxSessions   = 16
	sessionIdle   = 10 * time.Minute
	defaultReadMs = 1000
	readPoll      = 25 * time.Millisecond
	// writeWait is how long a write waits for the process to read it: one
	// that has stopped reading would hold the call for ever.
	writeWait = 5 * time.Second
)

// sessions is the run's session table: private processes that outlive the
// call that started them, so an agent keeps a cwd and an environment between
// commands the way a person does in the shared terminal.
type sessions struct {
	origins []Origin
	log     *slog.Logger

	mu    sync.Mutex
	table map[string]*session
	// all is every session ever opened, ended or not, for Close to wait on.
	all sync.WaitGroup
	// ctx is the table's life: every session's process ends with it.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // the reaper has stopped
}

// session is one process: its stdin, what it has printed since the last
// read, when it was last used, and how it ended.
type session struct {
	cancel context.CancelFunc
	stdin  io.WriteCloser
	out    *capped
	errb   *capped
	exited chan struct{}
	exit   int // set before exited closes
	last   time.Time
}

func newSessions(origins []Origin, log *slog.Logger) *sessions {
	ctx, cancel := context.WithCancel(context.Background())
	t := &sessions{origins: origins, log: log, table: map[string]*session{}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go t.reaper()
	return t
}

// Close ends every session and waits for their processes to be gone, so
// nothing a session started outlives the run. Callable more than once.
func (t *sessions) Close() error {
	t.cancel()
	<-t.done
	t.mu.Lock()
	for id, s := range t.table {
		s.cancel()
		delete(t.table, id)
	}
	t.mu.Unlock()
	t.all.Wait()
	return nil
}

// reaper ends sessions nothing has touched for sessionIdle, once a minute.
func (t *sessions) reaper() {
	defer close(t.done)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-tick.C:
			t.reap()
		}
	}
}

// reap ends every session idle past sessionIdle.
func (t *sessions) reap() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, s := range t.table {
		if time.Since(s.last) > sessionIdle {
			t.log.Debug("mcp session reaped", "id", id)
			s.cancel()
			delete(t.table, id)
		}
	}
}

// count is how many sessions are open.
func (t *sessions) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.table)
}

// get answers the open session with that id, touching it.
func (t *sessions) get(id string) (*session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.table[id]
	if !ok {
		return nil, fmt.Errorf("no session %q", id)
	}
	s.last = time.Now()
	return s, nil
}

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
	ExitCode int    `json:"exit_code" jsonschema:"the process's exit code once exited is true, -1 when a signal ended it"`
}

// open is the session_open tool: a process on origin n, its id to call it by.
func (t *sessions) open(_ context.Context, _ *sdk.CallToolRequest, in sessionOpenIn) (*sdk.CallToolResult, sessionOpenOut, error) {
	o, err := spawnable(t.origins, in.N)
	if err != nil {
		return nil, sessionOpenOut{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctx.Err() != nil {
		return nil, sessionOpenOut{}, errEnding
	}
	if len(t.table) >= maxSessions {
		return nil, sessionOpenOut{}, fmt.Errorf("%d sessions are open, the most this run allows; close one", maxSessions)
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, sessionOpenOut{}, err
	}
	id := hex.EncodeToString(raw[:])
	ctx, cancel := context.WithCancel(t.ctx)
	pr, pw := io.Pipe()
	s := &session{cancel: cancel, stdin: pw, out: newCapped(maxOutput), errb: newCapped(maxOutput), exited: make(chan struct{}), last: time.Now()}
	t.table[id] = s
	t.all.Add(1)
	go func() {
		defer t.all.Done()
		defer cancel()
		exit, err := o.Spawner.Spawn(ctx, in.Argv, pr, s.out, s.errb)
		// Whatever is still writing to a process that has gone is told so.
		_ = pr.CloseWithError(io.ErrClosedPipe)
		if err != nil {
			_, _ = io.WriteString(s.errb, err.Error()+"\n")
			exit = -1
		}
		s.exit = exit
		close(s.exited)
		t.log.Debug("mcp session ended", "id", id, "origin", in.N, "exit", exit, "error", err)
	}()
	t.log.Debug("mcp session opened", "id", id, "origin", in.N, "argv", in.Argv)
	return nil, sessionOpenOut{ID: id}, nil
}

// write is the session_write tool: bytes to the process's stdin, once it has
// read them.
func (t *sessions) write(_ context.Context, _ *sdk.CallToolRequest, in sessionWriteIn) (*sdk.CallToolResult, any, error) {
	s, err := t.get(in.ID)
	if err != nil {
		return nil, nil, err
	}
	select {
	case <-s.exited:
		return nil, nil, fmt.Errorf("session %q exited with %d; read it", in.ID, s.exit)
	default:
	}
	done := make(chan error, 1)
	go func() { _, err := io.WriteString(s.stdin, in.Stdin); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			<-s.exited
			return nil, nil, fmt.Errorf("session %q exited with %d; read it", in.ID, s.exit)
		}
		return nil, nil, nil
	case <-s.exited:
		return nil, nil, fmt.Errorf("session %q exited with %d; read it", in.ID, s.exit)
	case <-time.After(writeWait):
		return nil, nil, fmt.Errorf("session %q is not reading its stdin", in.ID)
	}
}

// read is the session_read tool: what arrived since the last read, waiting
// up to timeout_ms for something when nothing has. Once it reports the exit,
// the session is forgotten.
func (t *sessions) read(ctx context.Context, _ *sdk.CallToolRequest, in sessionReadIn) (*sdk.CallToolResult, sessionReadOut, error) {
	s, err := t.get(in.ID)
	if err != nil {
		return nil, sessionReadOut{}, err
	}
	wait := in.TimeoutMs
	if wait <= 0 {
		wait = defaultReadMs
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Millisecond)
	for {
		// The exit before the output: once it is seen, everything the process
		// printed has been written, so the take below has all of it.
		exited := false
		select {
		case <-s.exited:
			exited = true
		default:
		}
		out, errText := s.out.take(), s.errb.take()
		if out != "" || errText != "" || exited || time.Now().After(deadline) || ctx.Err() != nil {
			res := sessionReadOut{Stdout: out, Stderr: errText, Exited: exited}
			if exited {
				res.ExitCode = s.exit
				t.mu.Lock()
				delete(t.table, in.ID)
				t.mu.Unlock()
			}
			return nil, res, nil
		}
		time.Sleep(readPoll)
	}
}

// close is the session_close tool: the process ended, and the session with
// it.
func (t *sessions) close(_ context.Context, _ *sdk.CallToolRequest, in sessionIDIn) (*sdk.CallToolResult, any, error) {
	t.mu.Lock()
	s, ok := t.table[in.ID]
	delete(t.table, in.ID)
	t.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("no session %q", in.ID)
	}
	_ = s.stdin.Close()
	s.cancel()
	<-s.exited
	t.log.Debug("mcp session closed", "id", in.ID, "exit", s.exit)
	return nil, nil, nil
}
